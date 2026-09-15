package google

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/oauth2"

	"github.com/Cadet94957/Exec-Assist-AI/internal/events"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
)

// Provider is the connections-table key and the appointment source.
const Provider = "google"

// Window is how much calendar to mirror: enough history for "when did we last
// work for Ferguson", enough future for scheduling. Deliberately small — this
// is a working set, not an archive.
const (
	WindowPast   = 30 * 24 * time.Hour
	WindowFuture = 60 * 24 * time.Hour
)

// Result reports what one sync changed, for logs and the dashboard.
type Result struct {
	Fetched int
	Created int
	Changed int
	Removed int
}

// Connect stores a freshly issued token for the tenant.
func Connect(store *ledger.Store, box *secrets.Box, account string, tok *oauth2.Token) error {
	sealed, err := sealToken(box, tok)
	if err != nil {
		return err
	}
	return store.SaveConnection(Provider, account, sealed)
}

// Sync mirrors the tenant's calendar into the ledger and returns what changed.
//
// The outcome is recorded on the connection either way: a failure that is
// merely logged leaves the contractor looking at a stale calendar with no idea
// it stopped updating.
func Sync(ctx context.Context, cfg Config, store *ledger.Store, box *secrets.Box, now time.Time) (Result, error) {
	res, err := sync(ctx, cfg, store, box, now)
	if markErr := store.MarkSynced(Provider, err); markErr != nil && err == nil {
		return res, markErr
	}
	return res, err
}

func sync(ctx context.Context, cfg Config, store *ledger.Store, box *secrets.Box, now time.Time) (Result, error) {
	conn, err := store.GetConnection(Provider)
	if err != nil {
		return Result{}, err
	}

	tok, err := openToken(box, conn.Token)
	if err != nil {
		return Result{}, err
	}

	client := cfg.NewClient(ctx, tok)
	from := now.Add(-WindowPast)
	to := now.Add(WindowFuture)

	fetched, err := client.Events(ctx, "primary", from, to)
	if err != nil {
		return Result{}, err
	}

	// What the ledger currently believes, so the sync can tell what actually
	// changed rather than blindly writing the present state over it.
	existing, err := store.Between(from, to)
	if err != nil {
		return Result{}, err
	}
	known := make(map[string]ledger.Appointment, len(existing))
	for _, a := range existing {
		if a.Source == Provider && a.ExternalID.Valid {
			known[a.ExternalID.String] = a
		}
	}

	res := Result{Fetched: len(fetched)}
	keep := make([]string, 0, len(fetched))

	for _, ev := range fetched {
		keep = append(keep, ev.ID)
		appt := toAppointment(ev)

		prior, seen := known[ev.ID]
		switch {
		case !seen:
			if err := emit(store, newAppointmentEvent(events.AppointmentCreated, ev, nil), now); err != nil {
				return res, err
			}
			res.Created++
		case differs(prior, appt):
			before := &events.AppointmentBefore{
				Title:    prior.Title,
				StartsAt: prior.StartsAt,
				Location: prior.Location.String,
			}
			if err := emit(store, newAppointmentEvent(events.AppointmentChanged, ev, before), now); err != nil {
				return res, err
			}
			res.Changed++
		}

		if err := store.UpsertAppointment(appt); err != nil {
			return res, err
		}
	}

	// Anything still in the window that Google no longer returns was deleted
	// upstream. Reconciling against the fetched set is more robust than
	// tracking tombstones, which the API only surfaces on incremental syncs.
	fetchedIDs := make(map[string]bool, len(keep))
	for _, id := range keep {
		fetchedIDs[id] = true
	}
	for extID, a := range known {
		if fetchedIDs[extID] {
			continue
		}
		cancelled := events.Event{
			ID:         Provider + ":cancelled:" + extID + ":" + a.StartsAt.UTC().Format(time.RFC3339),
			Type:       events.AppointmentCancelled,
			Source:     events.SourceGoogle,
			OccurredAt: now.UTC(),
			Appointment: &events.Appointment{
				ExternalID: extID,
				Title:      a.Title,
				StartsAt:   a.StartsAt,
				Location:   a.Location.String,
			},
		}
		if err := emit(store, cancelled, now); err != nil {
			return res, err
		}
	}

	removed, err := store.PruneSourceWindow(Provider, from, to, keep)
	if err != nil {
		return res, err
	}

	// The access token may have been refreshed during the call. Persisting it
	// saves a refresh round trip on every future sync.
	if refreshed, err := client.Token(); err == nil && refreshed.AccessToken != tok.AccessToken {
		if sealed, err := sealToken(box, refreshed); err == nil {
			store.UpdateConnectionToken(Provider, sealed) //nolint:errcheck // best effort
		}
	}

	res.Removed = removed
	return res, nil
}

// emit validates and stores a typed event before the ledger is changed, so the
// record of what happened survives even if the write that follows fails.
func emit(store *ledger.Store, e events.Event, now time.Time) error {
	e.ReceivedAt = now.UTC()
	return store.AppendEvent(e)
}

// newAppointmentEvent builds a typed event from a calendar entry. The id is
// derived from the event and its start time so a redelivery is idempotent but
// a genuine move still records as a new change.
func newAppointmentEvent(t events.Type, ev Event, before *events.AppointmentBefore) events.Event {
	payload := &events.Appointment{
		ExternalID: ev.ID,
		Title:      ev.Title,
		StartsAt:   ev.Start,
		Location:   ev.Location,
		AllDay:     ev.AllDay,
		Previous:   before,
	}
	if !ev.End.IsZero() {
		end := ev.End
		payload.EndsAt = &end
	}
	return events.Event{
		ID:          fmt.Sprintf("%s:%s:%s:%s", Provider, t, ev.ID, ev.Start.UTC().Format(time.RFC3339)),
		Type:        t,
		Source:      events.SourceGoogle,
		OccurredAt:  ev.Start.UTC(),
		Appointment: payload,
	}
}

// differs reports whether anything worth recording as a change has changed.
func differs(prior ledger.Appointment, next ledger.Appointment) bool {
	if !prior.StartsAt.Equal(next.StartsAt) {
		return true
	}
	if prior.Title != next.Title {
		return true
	}
	if prior.Location.String != next.Location.String {
		return true
	}
	return false
}

// toAppointment maps a calendar event onto a ledger row. The Google event id
// is the external id, so re-syncing updates in place rather than duplicating.
func toAppointment(ev Event) ledger.Appointment {
	a := ledger.Appointment{
		ID:         Provider + "-" + ev.ID,
		Title:      ev.Title,
		StartsAt:   ev.Start,
		Source:     Provider,
		ExternalID: sql.NullString{String: ev.ID, Valid: true},
	}
	if !ev.End.IsZero() {
		a.EndsAt = sql.NullTime{Time: ev.End, Valid: true}
	}
	if ev.Location != "" {
		a.Location = sql.NullString{String: ev.Location, Valid: true}
	}
	return a
}

func sealToken(box *secrets.Box, tok *oauth2.Token) ([]byte, error) {
	if box == nil {
		return nil, secrets.ErrNoKey
	}
	raw, err := json.Marshal(tok)
	if err != nil {
		return nil, fmt.Errorf("google: encode token: %w", err)
	}
	return box.Seal(raw)
}

func openToken(box *secrets.Box, sealed []byte) (*oauth2.Token, error) {
	if box == nil {
		return nil, secrets.ErrNoKey
	}
	raw, err := box.Open(sealed)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("google: decode token: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("google: stored token has no refresh token — reconnect the calendar")
	}
	return &tok, nil
}
