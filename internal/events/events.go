// Package events is the typed boundary between integrations and business logic.
//
// The spec's rule (§16) is that LLMs classify and extract, while application
// code validates and executes. Nothing downstream of this package should ever
// depend on the shape of model output or on a third party's JSON.
//
// There is a second reason this exists. A sync that only ever writes the
// current state knows what the calendar says today but not that anything
// moved. Exception-first behaviour (§13) and conflict alerts (§29 Phase 3)
// both need the change itself — "Ferguson moved to 2pm" is the useful
// sentence, and it cannot be recovered from an upsert.
package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Type names what happened. These are the only kinds the application handles;
// anything else is rejected at the boundary rather than carried inward.
type Type string

const (
	AppointmentCreated   Type = "appointment.created"
	AppointmentChanged   Type = "appointment.changed"
	AppointmentCancelled Type = "appointment.cancelled"
	JobCompleted         Type = "job.completed"
	InvoiceOutstanding   Type = "invoice.outstanding"
	EmailReceived        Type = "email.received"
)

var validTypes = map[Type]bool{
	AppointmentCreated: true, AppointmentChanged: true, AppointmentCancelled: true,
	JobCompleted: true, InvoiceOutstanding: true, EmailReceived: true,
}

// Source is where an event came from. It is part of provenance: a fact learned
// from a forwarded email is not as reliable as one from a connected calendar.
type Source string

const (
	SourceGoogle  Source = "google"
	SourceEmail   Source = "inbound-email"
	SourceWebhook Source = "webhook"
	SourceSystem  Source = "system"
)

var validSources = map[Source]bool{
	SourceGoogle: true, SourceEmail: true, SourceWebhook: true, SourceSystem: true,
}

// Appointment is the payload for every appointment.* event.
type Appointment struct {
	// ExternalID is the id in the originating system, and the key the ledger
	// deduplicates on.
	ExternalID string     `json:"external_id"`
	Title      string     `json:"title"`
	StartsAt   time.Time  `json:"starts_at"`
	EndsAt     *time.Time `json:"ends_at,omitempty"`
	Location   string     `json:"location,omitempty"`
	AllDay     bool       `json:"all_day,omitempty"`

	// Previous carries what the appointment looked like before a change, so a
	// brief can say what moved rather than only where it landed.
	Previous *AppointmentBefore `json:"previous,omitempty"`
}

// AppointmentBefore is the prior state of a changed appointment.
type AppointmentBefore struct {
	Title    string    `json:"title"`
	StartsAt time.Time `json:"starts_at"`
	Location string    `json:"location,omitempty"`
}

// Moved reports whether a change shifted the time, as opposed to only
// retitling it. A retitle is not worth interrupting anyone about.
func (a Appointment) Moved() bool {
	return a.Previous != nil && !a.Previous.StartsAt.Equal(a.StartsAt)
}

// Event is one typed thing that happened.
type Event struct {
	ID         string
	Type       Type
	Source     Source
	OccurredAt time.Time
	ReceivedAt time.Time

	// Appointment is set for every appointment.* type.
	Appointment *Appointment

	// Raw is the original payload, kept so a memory drawn from this event can
	// cite what was actually received rather than our reading of it.
	Raw json.RawMessage
}

// ErrUnknownType reports a type the application does not handle.
var ErrUnknownType = errors.New("events: unknown event type")

// Validate checks an event before it is allowed past the boundary.
func (e Event) Validate() error {
	if e.ID == "" {
		return errors.New("events: id is required")
	}
	if !validTypes[e.Type] {
		return fmt.Errorf("%w: %q", ErrUnknownType, e.Type)
	}
	if !validSources[e.Source] {
		return fmt.Errorf("events: unknown source %q", e.Source)
	}
	if e.OccurredAt.IsZero() {
		return errors.New("events: occurred_at is required")
	}

	switch e.Type {
	case AppointmentCreated, AppointmentChanged, AppointmentCancelled:
		if e.Appointment == nil {
			return fmt.Errorf("events: %s requires an appointment payload", e.Type)
		}
		if e.Appointment.ExternalID == "" {
			return fmt.Errorf("events: %s requires an external id", e.Type)
		}
		if e.Type == AppointmentChanged && e.Appointment.Previous == nil {
			return errors.New("events: appointment.changed requires the previous state — a change with nothing to compare is just a write")
		}
	}
	return nil
}

// Payload serialises the typed payload for storage.
func (e Event) Payload() ([]byte, error) {
	if e.Appointment != nil {
		return json.Marshal(e.Appointment)
	}
	if len(e.Raw) > 0 {
		return e.Raw, nil
	}
	return []byte("{}"), nil
}

// Describe renders a short human sentence for logs and briefs.
func (e Event) Describe() string {
	if e.Appointment == nil {
		return string(e.Type)
	}
	a := e.Appointment
	switch e.Type {
	case AppointmentCreated:
		return fmt.Sprintf("%q added at %s", a.Title, a.StartsAt.Format("Mon 15:04"))
	case AppointmentCancelled:
		return fmt.Sprintf("%q cancelled", a.Title)
	case AppointmentChanged:
		if a.Moved() {
			return fmt.Sprintf("%q moved from %s to %s", a.Title,
				a.Previous.StartsAt.Format("Mon 15:04"), a.StartsAt.Format("Mon 15:04"))
		}
		return fmt.Sprintf("%q updated", a.Title)
	}
	return string(e.Type)
}
