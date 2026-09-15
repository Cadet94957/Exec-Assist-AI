package events_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/events"
)

func appt() *events.Appointment {
	return &events.Appointment{
		ExternalID: "evt-1",
		Title:      "Harborview reglaze",
		StartsAt:   time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC),
	}
}

// §16: the application validates at the boundary so nothing downstream has to
// trust a third party's JSON or a model's output.
func TestValidateRejectsMalformedEvents(t *testing.T) {
	base := events.Event{
		ID: "e1", Type: events.AppointmentCreated, Source: events.SourceGoogle,
		OccurredAt: time.Now(), Appointment: appt(),
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("a well-formed event was rejected: %v", err)
	}

	cases := map[string]func(*events.Event){
		"no id":          func(e *events.Event) { e.ID = "" },
		"unknown type":   func(e *events.Event) { e.Type = events.Type("appointment.exploded") },
		"unknown source": func(e *events.Event) { e.Source = events.Source("telepathy") },
		"no occurred_at": func(e *events.Event) { e.OccurredAt = time.Time{} },
		"no payload":     func(e *events.Event) { e.Appointment = nil },
		"no external id": func(e *events.Event) { e.Appointment.ExternalID = "" },
	}
	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			e := base
			a := *appt()
			e.Appointment = &a
			mangle(&e)
			if err := e.Validate(); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestUnknownTypeIsIdentifiable(t *testing.T) {
	e := events.Event{
		ID: "e1", Type: events.Type("nonsense"), Source: events.SourceGoogle,
		OccurredAt: time.Now(),
	}
	if err := e.Validate(); !errors.Is(err, events.ErrUnknownType) {
		t.Errorf("got %v, want ErrUnknownType", err)
	}
}

// A change with nothing to compare against is just a write, and produces no
// useful sentence in a brief.
func TestChangedRequiresPreviousState(t *testing.T) {
	e := events.Event{
		ID: "e1", Type: events.AppointmentChanged, Source: events.SourceGoogle,
		OccurredAt: time.Now(), Appointment: appt(),
	}
	if err := e.Validate(); err == nil {
		t.Error("appointment.changed was accepted with no previous state")
	}
}

func TestDescribeReadsLikeASentence(t *testing.T) {
	moved := *appt()
	moved.StartsAt = time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC)
	moved.Previous = &events.AppointmentBefore{
		Title:    "Harborview reglaze",
		StartsAt: time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC),
	}

	e := events.Event{
		ID: "e1", Type: events.AppointmentChanged, Source: events.SourceGoogle,
		OccurredAt: time.Now(), Appointment: &moved,
	}
	got := e.Describe()
	for _, want := range []string{"moved from", "08:00", "14:00"} {
		if !contains(got, want) {
			t.Errorf("describe = %q, missing %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
