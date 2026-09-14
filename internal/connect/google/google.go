// Package google connects a tenant's Google Calendar and mirrors it into the
// ledger.
//
// This talks to the Calendar REST API directly rather than through
// google.golang.org/api/calendar/v3. That library is correct, but it pulls in
// grpc, protobuf and around sixty-five other modules to make what is a single
// GET request — and every one of those is a dependency someone has to keep
// patched. The oauth2 package is worth its weight because it refreshes access
// tokens transparently; the rest is one endpoint with a stable, documented
// shape.
package google

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	googleoauth "golang.org/x/oauth2/google"
)

// Scope covers reading and writing calendar events. It is a "sensitive" scope
// in Google's classification, not a "restricted" one — so it needs OAuth
// consent-screen verification but not the CASA security assessment that Gmail
// read access would demand.
//
// Write access is requested now even though this phase only reads: adding it
// later would force every existing customer through a second consent screen.
const Scope = "https://www.googleapis.com/auth/calendar.events"

// DefaultBaseURL is Google's Calendar v3 root.
const DefaultBaseURL = "https://www.googleapis.com/calendar/v3"

// Config holds the OAuth client credentials from Google Cloud Console.
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	// BaseURL overrides the Calendar API root. Empty means Google. Tests set
	// it to a local server so the sync path can be exercised without
	// credentials or network.
	BaseURL string
}

// Configured reports whether Google credentials are present. When false the
// dashboard should say the feature is unavailable rather than offering a
// button that fails.
func (c Config) Configured() bool {
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

func (c Config) oauth() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		RedirectURL:  c.RedirectURL,
		Endpoint:     googleoauth.Endpoint,
		Scopes:       []string{Scope, "https://www.googleapis.com/auth/userinfo.email"},
	}
}

// AuthURL builds the consent URL to send the contractor to.
//
// AccessTypeOffline plus ApprovalForce are both required to reliably receive a
// refresh token: Google issues one only on the first consent unless consent is
// re-prompted, and without it the connection silently dies in an hour.
func (c Config) AuthURL(state string) string {
	return c.oauth().AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
	)
}

// Exchange turns the callback's authorization code into a token.
func (c Config) Exchange(ctx context.Context, code string) (*oauth2.Token, error) {
	tok, err := c.oauth().Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("google: exchange code: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("google: no refresh token returned — the connection would expire within the hour")
	}
	return tok, nil
}

// Client is an authenticated Calendar client for one tenant.
type Client struct {
	http   *http.Client
	source oauth2.TokenSource
	base   string
}

// NewClient builds a client that refreshes the token as needed.
func (c Config) NewClient(ctx context.Context, tok *oauth2.Token) *Client {
	src := c.oauth().TokenSource(ctx, tok)
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{
		http:   oauth2.NewClient(ctx, src),
		source: src,
		base:   strings.TrimSuffix(base, "/"),
	}
}

// Token returns the current token, which may have been refreshed since the
// client was built. Callers should persist it when it differs from what they
// loaded, so the next sync starts from a live access token.
func (cl *Client) Token() (*oauth2.Token, error) {
	tok, err := cl.source.Token()
	if err != nil {
		return nil, fmt.Errorf("google: refresh token: %w", err)
	}
	return tok, nil
}

// Event is one calendar entry, reduced to what the ledger stores.
type Event struct {
	ID       string
	Title    string
	Location string
	Start    time.Time
	End      time.Time
	// AllDay marks an event given as a date rather than a timestamp. These
	// still occupy the day but should not be treated as a timed stop.
	AllDay bool
}

// wire types mirror the Calendar v3 JSON exactly.
type eventsResponse struct {
	Items         []wireEvent `json:"items"`
	NextPageToken string      `json:"nextPageToken"`
}

type wireEvent struct {
	ID       string        `json:"id"`
	Status   string        `json:"status"`
	Summary  string        `json:"summary"`
	Location string        `json:"location"`
	Start    wireEventTime `json:"start"`
	End      wireEventTime `json:"end"`
}

// wireEventTime carries either dateTime (RFC3339) for a timed event or date
// (yyyy-mm-dd) for an all-day one — never both.
type wireEventTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
	TimeZone string `json:"timeZone"`
}

// Events lists events starting in [from, to).
//
// singleEvents=true expands recurring events into individual instances, which
// is what a day view needs — without it a weekly job appears once, at its
// original date, forever.
func (cl *Client) Events(ctx context.Context, calendarID string, from, to time.Time) ([]Event, error) {
	if calendarID == "" {
		calendarID = "primary"
	}
	endpoint := cl.base + "/calendars/" + url.PathEscape(calendarID) + "/events"

	var out []Event
	pageToken := ""

	// Bounded so a pagination bug cannot spin forever against a paid API.
	for page := 0; page < 20; page++ {
		q := url.Values{}
		q.Set("timeMin", from.Format(time.RFC3339))
		q.Set("timeMax", to.Format(time.RFC3339))
		q.Set("singleEvents", "true")
		q.Set("orderBy", "startTime")
		q.Set("maxResults", "250")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
		if err != nil {
			return nil, fmt.Errorf("google: build events request: %w", err)
		}

		resp, err := cl.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("google: list events: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("google: read events response: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("google: calendar returned %d: %s",
				resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var parsed eventsResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("google: decode events: %w", err)
		}

		for _, item := range parsed.Items {
			ev, ok := convert(item)
			if ok {
				out = append(out, ev)
			}
		}

		pageToken = parsed.NextPageToken
		if pageToken == "" {
			return out, nil
		}
	}
	return out, nil
}

// convert reduces a wire event, reporting false for anything not worth storing.
func convert(w wireEvent) (Event, bool) {
	// Cancelled events only appear on incremental syncs or with showDeleted,
	// but skip them defensively — a deleted job must never show up as a stop.
	if w.Status == "cancelled" || w.ID == "" {
		return Event{}, false
	}

	start, allDay, ok := parseEventTime(w.Start)
	if !ok {
		return Event{}, false
	}
	end, _, _ := parseEventTime(w.End)

	title := strings.TrimSpace(w.Summary)
	if title == "" {
		title = "(no title)"
	}

	return Event{
		ID:       w.ID,
		Title:    title,
		Location: strings.TrimSpace(w.Location),
		Start:    start,
		End:      end,
		AllDay:   allDay,
	}, true
}

func parseEventTime(t wireEventTime) (parsed time.Time, allDay bool, ok bool) {
	if t.DateTime != "" {
		v, err := time.Parse(time.RFC3339, t.DateTime)
		if err != nil {
			return time.Time{}, false, false
		}
		return v.UTC(), false, true
	}
	if t.Date != "" {
		// An all-day event is a date with no zone. Interpret it in the
		// calendar's own timezone when given, so a job on the 15th does not
		// land on the 14th for a contractor west of UTC.
		loc := time.UTC
		if t.TimeZone != "" {
			if l, err := time.LoadLocation(t.TimeZone); err == nil {
				loc = l
			}
		}
		v, err := time.ParseInLocation("2006-01-02", t.Date, loc)
		if err != nil {
			return time.Time{}, false, false
		}
		return v.UTC(), true, true
	}
	return time.Time{}, false, false
}
