package google_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/Cadet94957/Exec-Assist-AI/internal/connect/google"
)

// liveToken is far from expiry so the oauth2 library never tries to refresh
// against Google during a test.
func liveToken() *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  "test-access",
		RefreshToken: "test-refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(24 * time.Hour),
	}
}

// fakeCalendar serves pages of a canned Google Calendar response.
func fakeCalendar(t *testing.T, pages []string) (*httptest.Server, *[]string) {
	t.Helper()
	var gotQueries []string
	var srv *httptest.Server
	page := 0

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		if !strings.HasSuffix(r.URL.Path, "/events") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-access" {
			t.Errorf("Authorization = %q, want the bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if page < len(pages) {
			w.Write([]byte(pages[page]))
			page++
			return
		}
		w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotQueries
}

func newClient(t *testing.T, baseURL string) *google.Client {
	t.Helper()
	cfg := google.Config{ClientID: "id", ClientSecret: "secret", RedirectURL: "http://x/cb", BaseURL: baseURL}
	return cfg.NewClient(context.Background(), liveToken())
}

// The shape here is Google's real events.list response. If this parsing breaks,
// every stop silently disappears from the brief.
func TestEventsParsesRealResponseShape(t *testing.T) {
	body := `{
      "kind": "calendar#events",
      "items": [
        {
          "id": "evt-timed",
          "status": "confirmed",
          "summary": "Harborview — storefront reglaze",
          "location": "88 Harbor Way",
          "start": {"dateTime": "2026-09-15T08:00:00-07:00"},
          "end":   {"dateTime": "2026-09-15T11:00:00-07:00"}
        },
        {
          "id": "evt-allday",
          "status": "confirmed",
          "summary": "Shop closed — inventory",
          "start": {"date": "2026-09-16"},
          "end":   {"date": "2026-09-17"}
        },
        {
          "id": "evt-cancelled",
          "status": "cancelled",
          "summary": "Cancelled measure",
          "start": {"dateTime": "2026-09-15T14:00:00-07:00"}
        },
        {
          "id": "evt-untitled",
          "status": "confirmed",
          "start": {"dateTime": "2026-09-15T16:00:00-07:00"}
        }
      ]
    }`

	srv, _ := fakeCalendar(t, []string{body})
	events, err := newClient(t, srv.URL).Events(context.Background(), "primary",
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("events: %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (the cancelled one must be dropped)", len(events))
	}

	timed := events[0]
	if timed.ID != "evt-timed" || timed.Title != "Harborview — storefront reglaze" {
		t.Errorf("first event = %+v", timed)
	}
	if timed.Location != "88 Harbor Way" {
		t.Errorf("location = %q", timed.Location)
	}
	if timed.AllDay {
		t.Error("a dateTime event was marked all-day")
	}
	// 08:00-07:00 is 15:00 UTC.
	if want := time.Date(2026, 9, 15, 15, 0, 0, 0, time.UTC); !timed.Start.Equal(want) {
		t.Errorf("start = %v, want %v", timed.Start.UTC(), want)
	}

	allDay := events[1]
	if !allDay.AllDay {
		t.Error("a date-only event was not marked all-day")
	}

	// A calendar entry with no title must still be a visible stop, not a blank
	// line in the morning brief.
	if events[2].Title != "(no title)" {
		t.Errorf("untitled event title = %q, want a placeholder", events[2].Title)
	}
}

// An all-day event given in a west-of-UTC calendar must not slide to the
// previous day.
func TestAllDayEventUsesCalendarTimezone(t *testing.T) {
	body := `{"items":[{"id":"a","status":"confirmed","summary":"Inventory",
	  "start":{"date":"2026-09-16","timeZone":"America/Los_Angeles"},
	  "end":{"date":"2026-09-17","timeZone":"America/Los_Angeles"}}]}`

	srv, _ := fakeCalendar(t, []string{body})
	events, err := newClient(t, srv.URL).Events(context.Background(), "primary",
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}

	la, _ := time.LoadLocation("America/Los_Angeles")
	if got := events[0].Start.In(la); got.Day() != 16 {
		t.Errorf("all-day event landed on day %d in the tenant's zone, want 16 (%v)", got.Day(), got)
	}
}

func TestEventsFollowsPagination(t *testing.T) {
	pages := []string{
		`{"items":[{"id":"a","status":"confirmed","summary":"One","start":{"dateTime":"2026-09-15T08:00:00Z"}}],"nextPageToken":"p2"}`,
		`{"items":[{"id":"b","status":"confirmed","summary":"Two","start":{"dateTime":"2026-09-15T09:00:00Z"}}]}`,
	}
	srv, queries := fakeCalendar(t, pages)

	events, err := newClient(t, srv.URL).Events(context.Background(), "primary",
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events across pages, want 2", len(events))
	}
	if len(*queries) != 2 {
		t.Fatalf("made %d requests, want 2", len(*queries))
	}
	if !strings.Contains((*queries)[1], "pageToken=p2") {
		t.Errorf("second request did not carry the page token: %s", (*queries)[1])
	}
}

// singleEvents=true is what expands a recurring job into individual instances.
// Without it a weekly job shows up once, at its original date, forever.
func TestEventsRequestsExpandedRecurrences(t *testing.T) {
	srv, queries := fakeCalendar(t, []string{`{"items":[]}`})
	if _, err := newClient(t, srv.URL).Events(context.Background(), "primary",
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("events: %v", err)
	}

	q := (*queries)[0]
	for _, want := range []string{"singleEvents=true", "orderBy=startTime", "timeMin=", "timeMax="} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q is missing %q", q, want)
		}
	}
}

func TestEventsSurfacesAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "insufficient permissions"}})
	}))
	defer srv.Close()

	_, err := newClient(t, srv.URL).Events(context.Background(), "primary",
		time.Now(), time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("expected an error for a 403")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "insufficient permissions") {
		t.Errorf("error should name the status and Google's message, got: %v", err)
	}
}

func TestAuthURLRequestsOfflineAccess(t *testing.T) {
	cfg := google.Config{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb"}
	u := cfg.AuthURL("state-123")

	// Without offline access and a forced prompt Google withholds the refresh
	// token and the connection dies within the hour.
	for _, want := range []string{"access_type=offline", "prompt=consent", "state=state-123", "calendar.events"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth URL missing %q:\n%s", want, u)
		}
	}
}

func TestConfigured(t *testing.T) {
	full := google.Config{ClientID: "a", ClientSecret: "b", RedirectURL: "c"}
	if !full.Configured() {
		t.Error("a fully populated config should be configured")
	}
	for _, partial := range []google.Config{
		{ClientSecret: "b", RedirectURL: "c"},
		{ClientID: "a", RedirectURL: "c"},
		{ClientID: "a", ClientSecret: "b"},
	} {
		if partial.Configured() {
			t.Errorf("%+v should not count as configured", partial)
		}
	}
}
