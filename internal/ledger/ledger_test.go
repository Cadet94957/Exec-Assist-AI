package ledger_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
)

func open(t *testing.T) *ledger.Store {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func text(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

// Calendar sync re-reads the same window repeatedly. If an external id did not
// deduplicate, a glazier would open the dashboard to four copies of every stop.
func TestUpsertDeduplicatesByExternalID(t *testing.T) {
	s := open(t)
	start := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)

	appt := ledger.Appointment{
		ID: "a1", Title: "Storefront reglaze", StartsAt: start,
		Location: text("412 Pike St"), Source: "google", ExternalID: text("gcal-123"),
	}
	if err := s.UpsertAppointment(appt); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Same calendar event, moved an hour later and retitled.
	moved := appt
	moved.ID = "a2"
	moved.Title = "Storefront reglaze (rescheduled)"
	moved.StartsAt = start.Add(time.Hour)
	if err := s.UpsertAppointment(moved); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := s.Between(start.Add(-24*time.Hour), start.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d appointments, want 1 — the calendar event was duplicated", len(got))
	}
	if got[0].Title != moved.Title {
		t.Errorf("title = %q, want the updated %q", got[0].Title, moved.Title)
	}
	if !got[0].StartsAt.Equal(moved.StartsAt) {
		t.Errorf("starts_at = %v, want the updated %v", got[0].StartsAt, moved.StartsAt)
	}
}

// Manual entries have no external id, so they must never collide with each
// other the way synced events do.
func TestManualAppointmentsDoNotCollide(t *testing.T) {
	s := open(t)
	start := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)

	for _, id := range []string{"m1", "m2", "m3"} {
		if err := s.UpsertAppointment(ledger.Appointment{
			ID: id, Title: "Site measure " + id, StartsAt: start, Source: "manual",
		}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	got, err := s.Between(start.Add(-time.Hour), start.Add(time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d appointments, want 3", len(got))
	}
}

// The brief walks the day in order. Out-of-order stops would produce a route
// that sends the contractor back and forth across town.
func TestBetweenReturnsChronologicalOrderAndRespectsBounds(t *testing.T) {
	s := open(t)
	day := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	for i, hour := range []int{14, 8, 11} {
		if err := s.UpsertAppointment(ledger.Appointment{
			ID:       string(rune('a' + i)),
			Title:    "stop",
			StartsAt: day.Add(time.Duration(hour) * time.Hour),
			Source:   "manual",
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	// Yesterday and tomorrow must not appear in today's window.
	for i, offset := range []time.Duration{-4 * time.Hour, 26 * time.Hour} {
		if err := s.UpsertAppointment(ledger.Appointment{
			ID: "out" + string(rune('a'+i)), Title: "outside", StartsAt: day.Add(offset), Source: "manual",
		}); err != nil {
			t.Fatalf("upsert outside: %v", err)
		}
	}

	got, err := s.Between(day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("between: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d stops, want 3 inside the day", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].StartsAt.Before(got[i-1].StartsAt) {
			t.Fatalf("stop %d (%v) is before stop %d (%v)", i, got[i].StartsAt, i-1, got[i-1].StartsAt)
		}
	}
}

func TestStats(t *testing.T) {
	s := open(t)
	now := time.Now().UTC()

	db := s.DB()
	if _, err := db.Exec(
		`INSERT INTO contacts (id, name, kind, created_at, updated_at) VALUES ('c1','Marriott 4th St','gc',?,?)`,
		now, now); err != nil {
		t.Fatalf("insert contact: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO jobs (id, contact_id, title, status, created_at, updated_at)
		 VALUES ('j1','c1','Lobby glass','scheduled',?,?), ('j2','c1','Old job','paid',?,?)`,
		now, now, now, now); err != nil {
		t.Fatalf("insert jobs: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO invoices (id, job_id, contact_id, amount_cents, status, created_at)
		 VALUES ('i1','j1','c1',125000,'sent',?), ('i2','j2','c1',40000,'paid',?)`,
		now, now); err != nil {
		t.Fatalf("insert invoices: %v", err)
	}

	st, err := s.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Contacts != 1 {
		t.Errorf("contacts = %d, want 1", st.Contacts)
	}
	if st.Jobs != 2 {
		t.Errorf("jobs = %d, want 2", st.Jobs)
	}
	if st.OpenJobs != 1 {
		t.Errorf("open jobs = %d, want 1 (paid jobs are closed)", st.OpenJobs)
	}
	// Only unpaid invoices count toward what he is owed.
	if st.UnpaidCents != 125000 {
		t.Errorf("unpaid = %d, want 125000", st.UnpaidCents)
	}
}

func TestRecordEvent(t *testing.T) {
	s := open(t)

	if err := s.RecordEvent("e1", "webhook", "booking", `{"customer":"Marriott"}`); err != nil {
		t.Fatalf("record event: %v", err)
	}

	var count int
	if err := s.DB().QueryRow(
		`SELECT COUNT(*) FROM events WHERE processed_at IS NULL`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("unprocessed events = %d, want 1", count)
	}
}
