package google_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/connect/google"
	"github.com/Cadet94957/Exec-Assist-AI/internal/events"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
)

const testKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testStore(t *testing.T) *ledger.Store {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testBox(t *testing.T) *secrets.Box {
	t.Helper()
	b, err := secrets.NewBox(testKey)
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return b
}

// serveEvents returns a config pointed at a server that replies with body,
// which the test can swap between syncs.
func serveEvents(t *testing.T, body *string) google.Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(*body))
	}))
	t.Cleanup(srv.Close)
	return google.Config{ClientID: "id", ClientSecret: "sec", RedirectURL: "http://x/cb", BaseURL: srv.URL}
}

func event(id, title string, start time.Time) string {
	return fmt.Sprintf(
		`{"id":%q,"status":"confirmed","summary":%q,"location":"88 Harbor Way","start":{"dateTime":%q},"end":{"dateTime":%q}}`,
		id, title, start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339))
}

func TestSyncMirrorsCalendarIntoLedger(t *testing.T) {
	store, box := testStore(t), testBox(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	body := `{"items":[` +
		event("a", "Harborview reglaze", now.Add(2*time.Hour)) + `,` +
		event("b", "Marriott measure", now.Add(4*time.Hour)) + `]}`
	cfg := serveEvents(t, &body)

	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	res, err := google.Sync(context.Background(), cfg, store, box, now)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Fetched != 2 {
		t.Errorf("fetched = %d, want 2", res.Fetched)
	}

	appts, err := store.Between(now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(appts) != 2 {
		t.Fatalf("ledger has %d appointments, want 2", len(appts))
	}
	if appts[0].Title != "Harborview reglaze" {
		t.Errorf("first appointment = %q", appts[0].Title)
	}
	if appts[0].Source != google.Provider {
		t.Errorf("source = %q, want %q", appts[0].Source, google.Provider)
	}
	if !appts[0].Location.Valid || appts[0].Location.String != "88 Harbor Way" {
		t.Errorf("location = %+v", appts[0].Location)
	}
}

// Syncing runs every ten minutes forever. If it did not deduplicate, a
// glazier's dashboard would accumulate a copy of every stop per sync.
func TestRepeatedSyncDoesNotDuplicate(t *testing.T) {
	store, box := testStore(t), testBox(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	body := `{"items":[` + event("a", "Harborview reglaze", now.Add(2*time.Hour)) + `]}`
	cfg := serveEvents(t, &body)

	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := google.Sync(context.Background(), cfg, store, box, now); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	appts, err := store.Between(now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(appts) != 1 {
		t.Fatalf("after 3 syncs the ledger has %d appointments, want 1", len(appts))
	}
}

// A rescheduled event must move, not appear twice.
func TestSyncUpdatesMovedEvent(t *testing.T) {
	store, box := testStore(t), testBox(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	body := `{"items":[` + event("a", "Harborview reglaze", now.Add(2*time.Hour)) + `]}`
	cfg := serveEvents(t, &body)

	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := google.Sync(context.Background(), cfg, store, box, now); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	moved := now.Add(6 * time.Hour)
	body = `{"items":[` + event("a", "Harborview reglaze (moved)", moved) + `]}`
	if _, err := google.Sync(context.Background(), cfg, store, box, now); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	appts, err := store.Between(now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(appts) != 1 {
		t.Fatalf("got %d appointments, want 1", len(appts))
	}
	if !appts[0].StartsAt.Equal(moved) {
		t.Errorf("starts_at = %v, want the moved time %v", appts[0].StartsAt, moved)
	}
	if appts[0].Title != "Harborview reglaze (moved)" {
		t.Errorf("title = %q, want the updated one", appts[0].Title)
	}
}

// The one that matters most: a job deleted in Google must vanish from the
// ledger. A full-window sync simply stops returning it — Google only reports
// cancellations on incremental syncs — so the reconcile is what catches it.
// Without this, a glazier drives to a cancelled job.
func TestSyncRemovesDeletedEvent(t *testing.T) {
	store, box := testStore(t), testBox(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	body := `{"items":[` +
		event("a", "Harborview reglaze", now.Add(2*time.Hour)) + `,` +
		event("b", "Cancelled measure", now.Add(4*time.Hour)) + `]}`
	cfg := serveEvents(t, &body)

	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := google.Sync(context.Background(), cfg, store, box, now); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	// "b" is deleted upstream and simply stops being returned.
	body = `{"items":[` + event("a", "Harborview reglaze", now.Add(2*time.Hour)) + `]}`
	res, err := google.Sync(context.Background(), cfg, store, box, now)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Removed != 1 {
		t.Errorf("removed = %d, want 1", res.Removed)
	}

	appts, err := store.Between(now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(appts) != 1 {
		t.Fatalf("got %d appointments, want 1 — the deleted job is still there", len(appts))
	}
	if appts[0].Title != "Harborview reglaze" {
		t.Errorf("the wrong appointment survived: %q", appts[0].Title)
	}
}

// Manually added stops are not Google's to delete.
func TestSyncLeavesManualAppointmentsAlone(t *testing.T) {
	store, box := testStore(t), testBox(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	if err := store.UpsertAppointment(ledger.Appointment{
		ID: "manual-1", Title: "Pick up glass from fabricator",
		StartsAt: now.Add(3 * time.Hour), Source: "manual",
	}); err != nil {
		t.Fatalf("seed manual: %v", err)
	}

	body := `{"items":[` + event("a", "Harborview reglaze", now.Add(2*time.Hour)) + `]}`
	cfg := serveEvents(t, &body)
	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := google.Sync(context.Background(), cfg, store, box, now); err != nil {
		t.Fatalf("sync: %v", err)
	}

	appts, err := store.Between(now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(appts) != 2 {
		t.Fatalf("got %d appointments, want 2 — the manual stop was eaten", len(appts))
	}
}

func TestSyncWithoutConnection(t *testing.T) {
	store, box := testStore(t), testBox(t)
	body := `{"items":[]}`
	cfg := serveEvents(t, &body)

	_, err := google.Sync(context.Background(), cfg, store, box, time.Now())
	if !errors.Is(err, ledger.ErrNoConnection) {
		t.Errorf("got %v, want ErrNoConnection", err)
	}
}

// A sync failure has to be visible. Silently serving a stale calendar is worse
// than saying something is wrong.
func TestSyncRecordsFailureOnTheConnection(t *testing.T) {
	store, box := testStore(t), testBox(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"backend error"}}`))
	}))
	defer srv.Close()
	cfg := google.Config{ClientID: "id", ClientSecret: "sec", RedirectURL: "http://x/cb", BaseURL: srv.URL}

	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := google.Sync(context.Background(), cfg, store, box, time.Now()); err == nil {
		t.Fatal("expected an error from a 500")
	}

	conn, err := store.GetConnection(google.Provider)
	if err != nil {
		t.Fatalf("get connection: %v", err)
	}
	if !conn.LastError.Valid || conn.LastError.String == "" {
		t.Error("the failure was not recorded on the connection")
	}
	if !conn.LastSync.Valid {
		t.Error("last_sync_at should be stamped even on a failed attempt")
	}
}

// Disconnecting must not leave the calendar's jobs behind.
func TestDisconnectRemovesAppointments(t *testing.T) {
	store, box := testStore(t), testBox(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	body := `{"items":[` + event("a", "Harborview reglaze", now.Add(2*time.Hour)) + `]}`
	cfg := serveEvents(t, &body)
	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := google.Sync(context.Background(), cfg, store, box, now); err != nil {
		t.Fatalf("sync: %v", err)
	}

	if err := store.DeleteConnection(google.Provider); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	appts, err := store.Between(now, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(appts) != 0 {
		t.Errorf("%d appointments survived a disconnect", len(appts))
	}
	if _, err := store.GetConnection(google.Provider); !errors.Is(err, ledger.ErrNoConnection) {
		t.Errorf("connection still present after disconnect: %v", err)
	}
}

// The refresh token is a long-lived key to someone's calendar and must never
// sit in the database in the clear.
func TestStoredTokenIsEncrypted(t *testing.T) {
	store, box := testStore(t), testBox(t)

	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	conn, err := store.GetConnection(google.Provider)
	if err != nil {
		t.Fatalf("get connection: %v", err)
	}
	if len(conn.Token) == 0 {
		t.Fatal("no token stored")
	}
	for _, secret := range []string{"test-refresh", "test-access", "refresh_token"} {
		if bytesContain(conn.Token, secret) {
			t.Errorf("stored token contains %q in the clear", secret)
		}
	}
}

func bytesContain(haystack []byte, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}

// §16: the sync must record what *changed*, not just the current state. A
// brief that can only say where a job is now cannot say "Ferguson moved to
// 2pm", which is the sentence that actually helps.
func TestSyncEmitsTypedEvents(t *testing.T) {
	store, box := testStore(t), testBox(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	body := `{"items":[` +
		event("a", "Harborview reglaze", now.Add(2*time.Hour)) + `,` +
		event("b", "Ferguson measure", now.Add(4*time.Hour)) + `]}`
	cfg := serveEvents(t, &body)

	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// First sync: both are new.
	res, err := google.Sync(context.Background(), cfg, store, box, now)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if res.Created != 2 {
		t.Errorf("created = %d, want 2", res.Created)
	}

	// Second sync, unchanged: nothing should be recorded as happening.
	res, err = google.Sync(context.Background(), cfg, store, box, now)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if res.Created != 0 || res.Changed != 0 {
		t.Errorf("an unchanged sync reported created=%d changed=%d, want 0 and 0 — "+
			"this would make the assistant announce news every ten minutes",
			res.Created, res.Changed)
	}

	// Ferguson moves, Harborview disappears.
	moved := now.Add(6 * time.Hour)
	body = `{"items":[` + event("b", "Ferguson measure", moved) + `]}`
	res, err = google.Sync(context.Background(), cfg, store, box, now)
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if res.Changed != 1 {
		t.Errorf("changed = %d, want 1", res.Changed)
	}
	if res.Removed != 1 {
		t.Errorf("removed = %d, want 1", res.Removed)
	}

	recs, err := store.RecentEvents(now.Add(-48*time.Hour), now.Add(48*time.Hour), 50)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}

	counts := map[events.Type]int{}
	for _, r := range recs {
		counts[r.Type]++
	}
	if counts[events.AppointmentCreated] != 2 {
		t.Errorf("created events = %d, want 2", counts[events.AppointmentCreated])
	}
	if counts[events.AppointmentChanged] != 1 {
		t.Errorf("changed events = %d, want 1", counts[events.AppointmentChanged])
	}
	if counts[events.AppointmentCancelled] != 1 {
		t.Errorf("cancelled events = %d, want 1", counts[events.AppointmentCancelled])
	}

	// The change must carry what it moved *from*, or nobody can describe it.
	var changed ledger.EventRecord
	for _, r := range recs {
		if r.Type == events.AppointmentChanged {
			changed = r
		}
	}
	var payload events.Appointment
	if err := json.Unmarshal([]byte(changed.Payload), &payload); err != nil {
		t.Fatalf("decode changed payload: %v", err)
	}
	if payload.Previous == nil {
		t.Fatal("appointment.changed carries no previous state")
	}
	if !payload.Moved() {
		t.Error("a time change was not reported as a move")
	}
	if !payload.Previous.StartsAt.Equal(now.Add(4 * time.Hour)) {
		t.Errorf("previous start = %v, want %v", payload.Previous.StartsAt, now.Add(4*time.Hour))
	}
}

// A retitle is not a move, and interrupting someone about it would be noise.
func TestRetitleIsChangedButNotMoved(t *testing.T) {
	store, box := testStore(t), testBox(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	at := now.Add(2 * time.Hour)
	body := `{"items":[` + event("a", "Harborview reglaze", at) + `]}`
	cfg := serveEvents(t, &body)
	if err := google.Connect(store, box, "dave@example.com", liveToken()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := google.Sync(context.Background(), cfg, store, box, now); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	body = `{"items":[` + event("a", "Harborview reglaze (confirmed)", at) + `]}`
	if _, err := google.Sync(context.Background(), cfg, store, box, now); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	recs, err := store.RecentEvents(now.Add(-48*time.Hour), now.Add(48*time.Hour), 50)
	if err != nil {
		t.Fatalf("recent events: %v", err)
	}
	for _, r := range recs {
		if r.Type != events.AppointmentChanged {
			continue
		}
		var p events.Appointment
		if err := json.Unmarshal([]byte(r.Payload), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if p.Moved() {
			t.Error("a retitle was reported as a move")
		}
	}
}
