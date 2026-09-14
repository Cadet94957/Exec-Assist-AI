package google

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/oauth2"

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

	events, err := client.Events(ctx, "primary", from, to)
	if err != nil {
		return Result{}, err
	}

	keep := make([]string, 0, len(events))
	for _, ev := range events {
		if err := store.UpsertAppointment(toAppointment(ev)); err != nil {
			return Result{}, err
		}
		keep = append(keep, ev.ID)
	}

	// Anything still in the window that Google no longer returns was deleted
	// upstream. Reconciling against the fetched set is more robust than
	// tracking tombstones, which the API only surfaces on incremental syncs.
	removed, err := store.PruneSourceWindow(Provider, from, to, keep)
	if err != nil {
		return Result{}, err
	}

	// The access token may have been refreshed during the call. Persisting it
	// saves a refresh round trip on every future sync.
	if refreshed, err := client.Token(); err == nil && refreshed.AccessToken != tok.AccessToken {
		if sealed, err := sealToken(box, refreshed); err == nil {
			store.UpdateConnectionToken(Provider, sealed) //nolint:errcheck // best effort
		}
	}

	return Result{Fetched: len(events), Removed: removed}, nil
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
