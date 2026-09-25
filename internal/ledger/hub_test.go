package ledger_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
)

func hubEvent(kind, id string, at time.Time) ledger.HubEvent {
	return ledger.HubEvent{ID: id, Type: kind, OccurredAt: at, JobID: "job-1", Title: "Window measure", CustomerID: "customer-1", CustomerName: "Ferguson", PropertyID: "site-1", Address: "41 Cedar Lane", Evidence: "Technician recorded the measurement in the source tool."}
}

func connection(t *testing.T, s *ledger.Store) string {
	t.Helper()
	id, err := s.AddHubConnection("Receptionist", []byte("sealed-test-credential"))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHubWorkflowRetryAndVerifiedCompletion(t *testing.T) {
	s := open(t)
	conn := connection(t, s)
	now := time.Now().UTC()
	e := hubEvent("measurement.completed", "measurement-1", now.Add(-time.Hour))
	r, err := s.IngestHubEvent(conn, e, now)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.IngestHubEvent(conn, e, now)
	if err != nil || !retry.Duplicate || retry.JobID != r.JobID {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	e.Evidence = "Different payload with the same ID"
	if _, err := s.IngestHubEvent(conn, e, now); !errors.Is(err, ledger.ErrHubConflict) {
		t.Fatalf("mutated replay accepted: %v", err)
	}
	tasks, err := s.Commitments(false)
	if err != nil || len(tasks) != 1 || tasks[0].Evidence == e.Evidence {
		t.Fatalf("commitments = %+v, %v", tasks, err)
	}
	if err := s.UpdateCommitment(tasks[0].ID, "Alex", "Supplier pricing"); err != nil {
		t.Fatal(err)
	}
	sent := hubEvent("estimate.sent", "sent-1", now)
	if _, err := s.IngestHubEvent(conn, sent, now); err != nil {
		t.Fatal(err)
	}
	tasks, err = s.Commitments(false)
	if err != nil || len(tasks) != 0 {
		t.Fatalf("completed task still open: %+v, %v", tasks, err)
	}
	all, err := s.Commitments(true)
	if err != nil || len(all) != 1 || all[0].CompletedEvent != sent.ID || all[0].WaitingOn != "" {
		t.Fatalf("completion lost evidence: %+v, %v", all, err)
	}
	if err := s.UpdateCommitment(all[0].ID, "You", ""); !errors.Is(err, ledger.ErrHubConflict) {
		t.Fatalf("completed task reopened: %v", err)
	}
}

func TestHubOutOfOrderAndUnrelatedCompletion(t *testing.T) {
	for _, unrelated := range []bool{false, true} {
		s := open(t)
		conn := connection(t, s)
		now := time.Now().UTC()
		sent := hubEvent("estimate.sent", "sent-1", now)
		if unrelated {
			sent.JobID = "another-job"
		}
		if _, err := s.IngestHubEvent(conn, sent, now); err != nil {
			t.Fatal(err)
		}
		if _, err := s.IngestHubEvent(conn, hubEvent("measurement.completed", "measure-1", now.Add(-time.Hour)), now); err != nil {
			t.Fatal(err)
		}
		tasks, err := s.Commitments(true)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("tasks: %+v %v", tasks, err)
		}
		if (tasks[0].Status == "completed") == unrelated {
			t.Fatalf("wrong completion with unrelated=%v: %+v", unrelated, tasks[0])
		}
	}
}

func TestHubIdentityIsConnectionScopedAndCannotSilentlyChange(t *testing.T) {
	s := open(t)
	first, second := connection(t, s), connection(t, s)
	now := time.Now().UTC()
	e := hubEvent("booking.created", "book-1", now)
	a, err := s.IngestHubEvent(first, e, now)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.IngestHubEvent(second, e, now)
	if err != nil || a.JobID == b.JobID {
		t.Fatalf("connections collided: %+v %+v %v", a, b, err)
	}
	e.ID, e.CustomerID = "book-2", "other-customer"
	if _, err := s.IngestHubEvent(first, e, now); !errors.Is(err, ledger.ErrHubConflict) {
		t.Fatalf("job silently reassigned: %v", err)
	}
	if err := s.DisableHubConnection(first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IngestHubEvent(first, hubEvent("booking.created", "new", now), now); err == nil {
		t.Fatal("disabled connection accepted data")
	}
}

func TestHubApprovalLinksOnceAndDoesNotChangeCalendar(t *testing.T) {
	s := open(t)
	conn := connection(t, s)
	now := time.Now().UTC()
	r, err := s.IngestHubEvent(conn, hubEvent("booking.created", "book-1", now), now)
	if err != nil {
		t.Fatal(err)
	}
	appt := ledger.Appointment{ID: "google-a", Title: "Ferguson", StartsAt: now, Source: "google", ExternalID: text("a")}
	if err := s.UpsertAppointment(appt); err != nil {
		t.Fatal(err)
	}
	if err := s.ProposeAppointmentLink(r.JobID, appt.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Between(now.Add(-time.Hour), now.Add(time.Hour))
	if before[0].JobID.Valid {
		t.Fatal("proposal executed without approval")
	}
	actions, err := s.HubActions()
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions: %+v %v", actions, err)
	}
	if err := s.DecideHubAction(actions[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideHubAction(actions[0].ID, true); !errors.Is(err, ledger.ErrHubConflict) {
		t.Fatalf("action executed twice: %v", err)
	}
	// Syncing again must retain the approved identity link.
	if err := s.UpsertAppointment(appt); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Between(now.Add(-time.Hour), now.Add(time.Hour))
	if after[0].JobID.String != r.JobID || after[0].Title != appt.Title || !after[0].StartsAt.Equal(appt.StartsAt) {
		t.Fatalf("link or source fields corrupted: %+v", after[0])
	}
}

func TestHubConcurrentRedeliveryCreatesOneCommitment(t *testing.T) {
	s := open(t)
	conn := connection(t, s)
	now := time.Now().UTC()
	e := hubEvent("measurement.completed", "same-event", now)
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.IngestHubEvent(conn, e, now)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	tasks, err := s.Commitments(false)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("duplicate work: %+v %v", tasks, err)
	}
}

func TestSearchBusinessFindsSourceBackedFollowUp(t *testing.T) {
	s := open(t)
	conn := connection(t, s)
	now := time.Now().UTC()
	if _, err := s.IngestHubEvent(conn, hubEvent("measurement.completed", "measure-1", now), now); err != nil {
		t.Fatal(err)
	}
	results, err := s.SearchBusiness("show me Ferguson estimate")
	if err != nil || len(results) != 1 || results[0].Kind != "follow-up" || results[0].URL == "" {
		t.Fatalf("search: %+v %v", results, err)
	}
	results, err = s.SearchBusiness("% OR 1=1")
	if err != nil || len(results) != 0 {
		t.Fatalf("query widened results: %+v %v", results, err)
	}
}

func TestHubStaleApprovalRollsBackDecision(t *testing.T) {
	s := open(t)
	conn := connection(t, s)
	now := time.Now().UTC()
	r, err := s.IngestHubEvent(conn, hubEvent("booking.created", "book-1", now), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAppointment(ledger.Appointment{ID: "cancelled", Title: "Measure", StartsAt: now, Source: "google", ExternalID: text("cancelled")}); err != nil {
		t.Fatal(err)
	}
	if err := s.ProposeAppointmentLink(r.JobID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	actions, _ := s.HubActions()
	if _, err := s.PruneSourceWindow("google", now.Add(-time.Hour), now.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideHubAction(actions[0].ID, true); !errors.Is(err, ledger.ErrHubConflict) {
		t.Fatalf("approved missing appointment: %v", err)
	}
	remaining, err := s.HubActions()
	if err != nil || len(remaining) != 1 || remaining[0].Status != "proposed" {
		t.Fatalf("failed execution marked successful: %+v %v", remaining, err)
	}
}
