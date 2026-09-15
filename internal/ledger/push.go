package ledger

import (
	"fmt"
	"time"
)

const pushSchema = `
CREATE TABLE IF NOT EXISTS push_subscriptions (
    endpoint   TEXT PRIMARY KEY,
    p256dh     TEXT NOT NULL,
    auth       TEXT NOT NULL,
    created_at DATETIME NOT NULL,
    last_ok_at DATETIME
);`

// PushSubscription is one installed browser.
type PushSubscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// SavePushSubscription stores or refreshes a browser's push endpoint. Keyed by
// endpoint so re-subscribing the same browser updates rather than duplicates —
// otherwise a reinstall would mean two notifications every morning.
func (s *Store) SavePushSubscription(sub PushSubscription) error {
	_, err := s.db.Exec(`
        INSERT INTO push_subscriptions (endpoint, p256dh, auth, created_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth`,
		sub.Endpoint, sub.P256dh, sub.Auth, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("ledger: save push subscription: %w", err)
	}
	return nil
}

// PushSubscriptions lists every device to notify.
func (s *Store) PushSubscriptions() ([]PushSubscription, error) {
	rows, err := s.db.Query(`SELECT endpoint, p256dh, auth FROM push_subscriptions`)
	if err != nil {
		return nil, fmt.Errorf("ledger: list push subscriptions: %w", err)
	}
	defer rows.Close()

	var out []PushSubscription
	for rows.Next() {
		var p PushSubscription
		if err := rows.Scan(&p.Endpoint, &p.P256dh, &p.Auth); err != nil {
			return nil, fmt.Errorf("ledger: scan push subscription: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePushSubscription forgets a dead endpoint. Browsers expire these
// routinely, so this is housekeeping rather than an incident.
func (s *Store) DeletePushSubscription(endpoint string) error {
	if _, err := s.db.Exec(`DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint); err != nil {
		return fmt.Errorf("ledger: delete push subscription: %w", err)
	}
	return nil
}

// MarkPushDelivered records a successful send.
func (s *Store) MarkPushDelivered(endpoint string) error {
	if _, err := s.db.Exec(
		`UPDATE push_subscriptions SET last_ok_at = ? WHERE endpoint = ?`,
		time.Now().UTC(), endpoint); err != nil {
		return fmt.Errorf("ledger: mark push delivered: %w", err)
	}
	return nil
}
