package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoConnection reports that this tenant has not connected that provider.
var ErrNoConnection = errors.New("ledger: no connection for that provider")

// Connection is a linked third-party account. Token is already encrypted by
// the caller — this package never sees the plaintext.
type Connection struct {
	Provider  string
	Account   string
	Token     []byte
	Connected time.Time
	LastSync  sql.NullTime
	LastError sql.NullString
}

// SaveConnection stores or replaces a provider connection.
func (s *Store) SaveConnection(provider, account string, token []byte) error {
	_, err := s.db.Exec(`
        INSERT INTO connections (provider, account, token, connected_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(provider) DO UPDATE SET
            account      = excluded.account,
            token        = excluded.token,
            connected_at = excluded.connected_at,
            last_error   = NULL`,
		provider, account, token, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("ledger: save %s connection: %w", provider, err)
	}
	return nil
}

// GetConnection loads one provider connection.
func (s *Store) GetConnection(provider string) (Connection, error) {
	var c Connection
	err := s.db.QueryRow(`
        SELECT provider, COALESCE(account, ''), token, connected_at, last_sync_at, last_error
        FROM connections WHERE provider = ?`, provider,
	).Scan(&c.Provider, &c.Account, &c.Token, &c.Connected, &c.LastSync, &c.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, ErrNoConnection
	}
	if err != nil {
		return Connection{}, fmt.Errorf("ledger: get %s connection: %w", provider, err)
	}
	return c, nil
}

// UpdateConnectionToken replaces just the stored token. The oauth2 library
// refreshes access tokens transparently, and the refreshed token is worth
// keeping so the next sync does not have to refresh again.
func (s *Store) UpdateConnectionToken(provider string, token []byte) error {
	_, err := s.db.Exec(`UPDATE connections SET token = ? WHERE provider = ?`, token, provider)
	if err != nil {
		return fmt.Errorf("ledger: update %s token: %w", provider, err)
	}
	return nil
}

// MarkSynced records the outcome of a sync attempt. A failure is recorded
// rather than thrown away so the dashboard can say what is wrong instead of
// silently showing a stale calendar.
func (s *Store) MarkSynced(provider string, syncErr error) error {
	var msg any
	if syncErr != nil {
		msg = syncErr.Error()
	}
	_, err := s.db.Exec(
		`UPDATE connections SET last_sync_at = ?, last_error = ? WHERE provider = ?`,
		time.Now().UTC(), msg, provider)
	if err != nil {
		return fmt.Errorf("ledger: mark %s synced: %w", provider, err)
	}
	return nil
}

// DeleteConnection removes a connection and every appointment that came from
// it. Disconnecting should leave no trace of the calendar behind.
func (s *Store) DeleteConnection(provider string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("ledger: delete %s connection: %w", provider, err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM appointments WHERE source = ?`, provider); err != nil {
		return fmt.Errorf("ledger: delete %s appointments: %w", provider, err)
	}
	if _, err := tx.Exec(`DELETE FROM connections WHERE provider = ?`, provider); err != nil {
		return fmt.Errorf("ledger: delete %s connection: %w", provider, err)
	}
	return tx.Commit()
}

// PruneSourceWindow deletes appointments from source inside [from, to) whose
// external id is not in keep.
//
// This is how deletions are handled. A full-window sync simply stops returning
// an event that was deleted in Google — cancelled events are only returned on
// an incremental sync or with showDeleted — so "present in the window but no
// longer upstream" is the signal, and reconciling against the fetched set is
// more robust than trying to track tombstones.
func (s *Store) PruneSourceWindow(source string, from, to time.Time, keep []string) (int, error) {
	query := `DELETE FROM appointments WHERE source = ? AND starts_at >= ? AND starts_at < ?`
	args := []any{source, from, to}

	if len(keep) > 0 {
		query += ` AND (external_id IS NULL OR external_id NOT IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",") + `))`
		for _, id := range keep {
			args = append(args, id)
		}
	}

	res, err := s.db.Exec(query, args...)
	if err != nil {
		return 0, fmt.Errorf("ledger: prune %s window: %w", source, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ledger: prune %s window: %w", source, err)
	}
	return int(n), nil
}
