package ledger

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/events"
)

// AppendEvent records a typed event. Events are the audit trail behind both
// the ledger's current state and any memory drawn from it, so they are stored
// before they are applied, not after.
func (s *Store) AppendEvent(e events.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	payload, err := e.Payload()
	if err != nil {
		return fmt.Errorf("ledger: encode event %s: %w", e.ID, err)
	}
	received := e.ReceivedAt
	if received.IsZero() {
		received = time.Now().UTC()
	}

	// Ignoring a duplicate id makes event delivery idempotent, which matters
	// because a retried sync or a redelivered webhook must not double-count.
	_, err = s.db.Exec(`
        INSERT OR IGNORE INTO events (id, source, kind, payload, occurred_at, received_at)
        VALUES (?, ?, ?, ?, ?, ?)`,
		e.ID, string(e.Source), string(e.Type), string(payload), e.OccurredAt.UTC(), received.UTC())
	if err != nil {
		return fmt.Errorf("ledger: append event %s: %w", e.ID, err)
	}
	return nil
}

// EventRecord is a stored event as it comes back out.
type EventRecord struct {
	ID          string
	Type        events.Type
	Source      events.Source
	Payload     string
	OccurredAt  time.Time
	ReceivedAt  time.Time
	ProcessedAt sql.NullTime
}

// RecentEvents returns events that occurred in [from, to), newest first. This
// is what lets a brief say what changed since yesterday.
func (s *Store) RecentEvents(from, to time.Time, limit int) ([]EventRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
        SELECT id, kind, source, payload, occurred_at, received_at, processed_at
        FROM events
        WHERE occurred_at >= ? AND occurred_at < ?
        ORDER BY occurred_at DESC
        LIMIT ?`, from.UTC(), to.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("ledger: recent events: %w", err)
	}
	defer rows.Close()

	var out []EventRecord
	for rows.Next() {
		var r EventRecord
		if err := rows.Scan(&r.ID, &r.Type, &r.Source, &r.Payload,
			&r.OccurredAt, &r.ReceivedAt, &r.ProcessedAt); err != nil {
			return nil, fmt.Errorf("ledger: scan event: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkEventProcessed stamps an event as handled.
func (s *Store) MarkEventProcessed(id string) error {
	if _, err := s.db.Exec(
		`UPDATE events SET processed_at = ? WHERE id = ?`, time.Now().UTC(), id); err != nil {
		return fmt.Errorf("ledger: mark event %s processed: %w", id, err)
	}
	return nil
}

// CountEvents reports how many events arrived in a window, for integration
// health on the admin page.
func (s *Store) CountEvents(from, to time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE received_at >= ? AND received_at < ?`,
		from.UTC(), to.UTC()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("ledger: count events: %w", err)
	}
	return n, nil
}
