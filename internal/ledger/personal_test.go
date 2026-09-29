package ledger

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func personalStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestPersonalSnapshotsAreAtomicAndSourceScoped(t *testing.T) {
	s := personalStore(t)
	now := time.Now().UTC()
	msg := MailMessage{ExternalID: "1", Subject: "Saved", ReceivedAt: now}
	for _, source := range []string{"gmail", "icloud-mail"} {
		if err := s.ReplaceMailSnapshot(source, []MailMessage{msg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReplaceMailSnapshot("gmail", []MailMessage{msg, msg}); err == nil {
		t.Fatal("expected invalid duplicate snapshot")
	}
	mail, err := s.RecentMail(time.Time{}, 20)
	if err != nil || len(mail) != 2 {
		t.Fatalf("rollback lost data: %v %+v", err, mail)
	}
	if err := s.ReplaceMailSnapshot("gmail", nil); err != nil {
		t.Fatal(err)
	}
	mail, _ = s.RecentMail(time.Time{}, 20)
	if len(mail) != 1 || mail[0].Source != "icloud-mail" {
		t.Fatal("empty snapshot affected another provider")
	}
	a := Appointment{Title: "Before", StartsAt: now, ExternalID: NullText("event")}
	if err := s.ReplaceCalendarSnapshot("icloud-calendar", now.Add(-time.Hour), now.Add(time.Hour), []Appointment{a}); err != nil {
		t.Fatal(err)
	}
	// A contact association must survive a source update.
	_, err = s.db.Exec(`INSERT INTO contacts(id,name,created_at,updated_at) VALUES('person','Nick',?,?)`, now, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE appointments SET contact_id='person' WHERE source='icloud-calendar'`)
	if err != nil {
		t.Fatal(err)
	}
	a.Title = "After"
	if err = s.ReplaceCalendarSnapshot("icloud-calendar", now.Add(-time.Hour), now.Add(time.Hour), []Appointment{a}); err != nil {
		t.Fatal(err)
	}
	appts, _ := s.Between(now.Add(-time.Hour), now.Add(time.Hour))
	if len(appts) != 1 || appts[0].Title != "After" || appts[0].ContactID != (sql.NullString{String: "person", Valid: true}) {
		t.Fatalf("association lost: %+v", appts)
	}
	if err = s.ReplaceCalendarSnapshot("icloud-calendar", now.Add(-time.Hour), now.Add(time.Hour), []Appointment{a, a}); err == nil {
		t.Fatal("expected duplicate snapshot failure")
	}
	appts, _ = s.Between(now.Add(-time.Hour), now.Add(time.Hour))
	if len(appts) != 1 {
		t.Fatal("partial calendar replaced last good copy")
	}
	if err = s.ReplaceCalendarSnapshot("icloud-calendar", now.Add(-time.Hour), now.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	appts, _ = s.Between(now.Add(-time.Hour), now.Add(time.Hour))
	if len(appts) != 0 {
		t.Fatal("deleted event remained")
	}
}
