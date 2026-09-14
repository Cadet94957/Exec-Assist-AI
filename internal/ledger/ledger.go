// Package ledger holds the operational record for one tenant: contacts,
// properties, jobs, appointments, invoices, and the raw inbound events they
// were derived from.
//
// This is deliberately separate from the memory graph. Everything here is a
// fact about a specific thing that happened, is disposable, and can be
// re-synced from the source app. Nothing here carries judgement. The moment a
// record implies a rule — "this customer always wants tempered" — that rule is
// filed through the memory package instead, with its why_matters.
package ledger

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Store is one tenant's operational database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens and migrates the ledger at path.
func Open(path string) (*Store, error) {
	handle, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("ledger: open %s: %w", path, err)
	}
	s := &Store{db: handle, path: path}
	if err := s.migrate(); err != nil {
		handle.Close()
		return nil, err
	}
	return s, nil
}

// Close checkpoints the WAL and closes the handle.
func (s *Store) Close() error {
	s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.db.Close()
}

// Path is the on-disk location of this ledger.
func (s *Store) Path() string { return s.path }

// DB exposes the handle for queries this package does not wrap yet.
func (s *Store) DB() *sql.DB { return s.db }

const schema = `
CREATE TABLE IF NOT EXISTS contacts (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL DEFAULT 'customer', -- customer|gc|supplier|insurer
    phone       TEXT,
    email       TEXT,
    notes       TEXT,
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_contacts_name ON contacts(name);

CREATE TABLE IF NOT EXISTS properties (
    id          TEXT PRIMARY KEY,
    contact_id  TEXT REFERENCES contacts(id) ON DELETE SET NULL,
    label       TEXT NOT NULL,
    address     TEXT,
    notes       TEXT,
    created_at  DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_properties_contact ON properties(contact_id);

CREATE TABLE IF NOT EXISTS jobs (
    id          TEXT PRIMARY KEY,
    contact_id  TEXT REFERENCES contacts(id) ON DELETE SET NULL,
    property_id TEXT REFERENCES properties(id) ON DELETE SET NULL,
    title       TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'lead', -- lead|quoted|scheduled|done|invoiced|paid|lost
    description TEXT,
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);

CREATE TABLE IF NOT EXISTS appointments (
    id          TEXT PRIMARY KEY,
    job_id      TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    contact_id  TEXT REFERENCES contacts(id) ON DELETE SET NULL,
    property_id TEXT REFERENCES properties(id) ON DELETE SET NULL,
    title       TEXT NOT NULL,
    starts_at   DATETIME NOT NULL,
    ends_at     DATETIME,
    location    TEXT,
    source      TEXT NOT NULL DEFAULT 'manual', -- google|receptionist|manual
    external_id TEXT,
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_appts_starts ON appointments(starts_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_appts_external ON appointments(source, external_id)
    WHERE external_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS invoices (
    id           TEXT PRIMARY KEY,
    job_id       TEXT REFERENCES jobs(id) ON DELETE SET NULL,
    contact_id   TEXT REFERENCES contacts(id) ON DELETE SET NULL,
    number       TEXT,
    amount_cents INTEGER NOT NULL DEFAULT 0,
    status       TEXT NOT NULL DEFAULT 'draft', -- draft|sent|overdue|paid|void
    issued_at    DATETIME,
    due_at       DATETIME,
    paid_at      DATETIME,
    created_at   DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_invoices_status ON invoices(status);

-- Raw inbound payloads from the universal webhook and the per-tenant inbound
-- email address, kept so extraction can be re-run after a prompt change
-- without asking the source apps to resend.
CREATE TABLE IF NOT EXISTS events (
    id           TEXT PRIMARY KEY,
    source       TEXT NOT NULL,  -- webhook|email|google
    kind         TEXT,
    payload      TEXT NOT NULL,
    received_at  DATETIME NOT NULL,
    processed_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_events_unprocessed ON events(received_at) WHERE processed_at IS NULL;

-- Linked third-party accounts. The token column holds an encrypted oauth2
-- token; this database never holds the plaintext.
CREATE TABLE IF NOT EXISTS connections (
    provider     TEXT PRIMARY KEY,
    account      TEXT,
    token        BLOB NOT NULL,
    connected_at DATETIME NOT NULL,
    last_sync_at DATETIME,
    last_error   TEXT
);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("ledger: migrate %s: %w", s.path, err)
	}
	return nil
}

// Appointment is one scheduled stop.
type Appointment struct {
	ID         string
	JobID      sql.NullString
	ContactID  sql.NullString
	PropertyID sql.NullString
	Title      string
	StartsAt   time.Time
	EndsAt     sql.NullTime
	Location   sql.NullString
	Source     string
	ExternalID sql.NullString
}

// UpsertAppointment inserts or updates by (source, external_id) when an
// external id is present, so re-syncing a calendar does not duplicate stops.
func (s *Store) UpsertAppointment(a Appointment) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`
        INSERT INTO appointments
            (id, job_id, contact_id, property_id, title, starts_at, ends_at,
             location, source, external_id, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(source, external_id) WHERE external_id IS NOT NULL
        DO UPDATE SET
            title      = excluded.title,
            starts_at  = excluded.starts_at,
            ends_at    = excluded.ends_at,
            location   = excluded.location,
            updated_at = excluded.updated_at`,
		a.ID, a.JobID, a.ContactID, a.PropertyID, a.Title, a.StartsAt, a.EndsAt,
		a.Location, a.Source, a.ExternalID, now, now)
	if err != nil {
		return fmt.Errorf("ledger: upsert appointment %s: %w", a.ID, err)
	}
	return nil
}

// Between returns appointments starting in [from, to), earliest first. This is
// what the morning brief walks.
func (s *Store) Between(from, to time.Time) ([]Appointment, error) {
	rows, err := s.db.Query(`
        SELECT id, job_id, contact_id, property_id, title, starts_at, ends_at,
               location, source, external_id
        FROM appointments
        WHERE starts_at >= ? AND starts_at < ?
        ORDER BY starts_at ASC`, from, to)
	if err != nil {
		return nil, fmt.Errorf("ledger: appointments between: %w", err)
	}
	defer rows.Close()

	var out []Appointment
	for rows.Next() {
		var a Appointment
		if err := rows.Scan(&a.ID, &a.JobID, &a.ContactID, &a.PropertyID, &a.Title,
			&a.StartsAt, &a.EndsAt, &a.Location, &a.Source, &a.ExternalID); err != nil {
			return nil, fmt.Errorf("ledger: scan appointment: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Stats is the headline count set for the dashboard.
type Stats struct {
	Contacts     int
	Jobs         int
	OpenJobs     int
	Appointments int
	UnpaidCents  int64
}

// Stats reads the dashboard counters in one pass.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	err := s.db.QueryRow(`
        SELECT (SELECT COUNT(*) FROM contacts),
               (SELECT COUNT(*) FROM jobs),
               (SELECT COUNT(*) FROM jobs WHERE status NOT IN ('paid','lost')),
               (SELECT COUNT(*) FROM appointments),
               (SELECT COALESCE(SUM(amount_cents), 0) FROM invoices
                 WHERE status IN ('sent','overdue'))`,
	).Scan(&st.Contacts, &st.Jobs, &st.OpenJobs, &st.Appointments, &st.UnpaidCents)
	if err != nil {
		return Stats{}, fmt.Errorf("ledger: stats: %w", err)
	}
	return st, nil
}

// RecordEvent stores a raw inbound payload for later extraction.
func (s *Store) RecordEvent(id, source, kind, payload string) error {
	_, err := s.db.Exec(
		`INSERT INTO events (id, source, kind, payload, received_at) VALUES (?, ?, ?, ?, ?)`,
		id, source, kind, payload, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("ledger: record event %s: %w", id, err)
	}
	return nil
}
