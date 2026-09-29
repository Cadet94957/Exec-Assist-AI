// Package ledger holds the operational record for one tenant: contacts,
// properties, jobs, appointments, invoices, and the raw inbound events they
// were derived from.
//
// This is deliberately separate from the memory graph. Source mirror rows can
// be re-synced, but receipts, identities, commitments, approvals, audit and
// memory metadata are authoritative and must be preserved. The moment a
// record implies a rule — "this customer always wants tempered" — that rule is
// filed through the memory package instead, with its why_matters.
package ledger

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
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
    source       TEXT NOT NULL,  -- google|inbound-email|webhook|system
    kind         TEXT NOT NULL,  -- the typed events.Type
    payload      TEXT NOT NULL,
    occurred_at  DATETIME NOT NULL,
    received_at  DATETIME NOT NULL,
    processed_at DATETIME
);
CREATE INDEX IF NOT EXISTS idx_events_occurred ON events(occurred_at);
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
	for _, stmt := range []string{schema, briefSchema, pushSchema, hubSchema, personalSchema} {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("ledger: migrate %s: %w", s.path, err)
		}
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

// RecordRaw stores an untyped inbound payload for later classification. Raw
// third-party content is data, never instruction (§15) — it is parked here and
// classified separately, not handed straight to anything that can act.
func (s *Store) RecordRaw(id, source, payload string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO events (id, source, kind, payload, occurred_at, received_at)
		 VALUES (?, ?, 'raw', ?, ?, ?)`,
		id, source, payload, now, now)
	if err != nil {
		return fmt.Errorf("ledger: record raw payload %s: %w", id, err)
	}
	return nil
}

// SearchAppointments finds stops whose title or location matches a query.
//
// This is the ledger half of "ask me anything": "when did we last do work for
// Ferguson" is a question about what happened, which memory cannot answer
// because events are deliberately kept out of it.
func (s *Store) SearchAppointments(query string, limit int) ([]Appointment, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	like := "%" + strings.ToLower(strings.TrimSpace(query)) + "%"

	rows, err := s.db.Query(`
        SELECT id, job_id, contact_id, property_id, title, starts_at, ends_at,
               location, source, external_id
        FROM appointments
        WHERE LOWER(title) LIKE ? OR LOWER(COALESCE(location, '')) LIKE ?
        ORDER BY starts_at DESC
        LIMIT ?`, like, like, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger: search appointments: %w", err)
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

// LocationText returns the appointment's location, or an empty string. It
// exists so callers do not have to reach through sql.NullString to ask a
// question as ordinary as "where is this".
func (a Appointment) LocationText() string {
	if a.Location.Valid {
		return a.Location.String
	}
	return ""
}

// Ends returns when the appointment finishes, falling back to an hour after it
// starts. An appointment with no end still occupies time; treating it as zero
// would make every day look empty and every gap look generous.
func (a Appointment) Ends() time.Time {
	if a.EndsAt.Valid {
		return a.EndsAt.Time
	}
	return a.StartsAt.Add(time.Hour)
}

// Invoice is money owed.
type Invoice struct {
	ID          string
	Number      sql.NullString
	Customer    sql.NullString
	AmountCents int64
	Status      string
	DueAt       sql.NullTime
}

// Outstanding returns invoices that have been sent but not paid, oldest first.
// "What needs collecting" is one of the seven questions a morning brief has to
// answer, and it is the one most directly about money.
func (s *Store) Outstanding(limit int) ([]Invoice, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
        SELECT i.id, i.number, c.name, i.amount_cents, i.status, i.due_at
        FROM invoices i
        LEFT JOIN contacts c ON c.id = i.contact_id
        WHERE i.status IN ('sent', 'overdue')
        ORDER BY COALESCE(i.due_at, i.issued_at, i.created_at) ASC
        LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger: outstanding invoices: %w", err)
	}
	defer rows.Close()

	var out []Invoice
	for rows.Next() {
		var inv Invoice
		if err := rows.Scan(&inv.ID, &inv.Number, &inv.Customer,
			&inv.AmountCents, &inv.Status, &inv.DueAt); err != nil {
			return nil, fmt.Errorf("ledger: scan invoice: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// Brief is a generated morning brief.
type Brief struct {
	Date        time.Time
	Headline    string
	Body        string
	GeneratedAt time.Time
	DeliveredAt sql.NullTime
	OpenedAt    sql.NullTime
}

const briefSchema = `
CREATE TABLE IF NOT EXISTS briefs (
    for_date     DATE PRIMARY KEY,
    headline     TEXT NOT NULL,
    body         TEXT NOT NULL,
    generated_at DATETIME NOT NULL,
    delivered_at DATETIME,
    opened_at    DATETIME
);`

// SaveBrief stores the brief for a day, replacing any earlier one. Regenerating
// is allowed: a brief written at 6am can be stale by 9 when a job moves.
func (s *Store) SaveBrief(date time.Time, headline, body string) error {
	_, err := s.db.Exec(`
        INSERT INTO briefs (for_date, headline, body, generated_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(for_date) DO UPDATE SET
            headline = excluded.headline,
            body = excluded.body,
            generated_at = excluded.generated_at`,
		date.Format("2006-01-02"), headline, body, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("ledger: save brief: %w", err)
	}
	return nil
}

// GetBrief loads a day's brief. A missing brief is not an error — most of the
// time there simply isn't one yet.
func (s *Store) GetBrief(date time.Time) (Brief, bool, error) {
	var b Brief
	var day string
	err := s.db.QueryRow(`
        SELECT for_date, headline, body, generated_at, delivered_at, opened_at
        FROM briefs WHERE for_date = ?`, date.Format("2006-01-02"),
	).Scan(&day, &b.Headline, &b.Body, &b.GeneratedAt, &b.DeliveredAt, &b.OpenedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Brief{}, false, nil
	}
	if err != nil {
		return Brief{}, false, fmt.Errorf("ledger: get brief: %w", err)
	}
	b.Date, _ = time.Parse("2006-01-02", day)
	return b, true, nil
}

// MarkBriefDelivered records that a brief actually reached someone. Delivery is
// tracked separately from generation because a brief that was written and never
// arrived looks identical to a working product until churn says otherwise.
func (s *Store) MarkBriefDelivered(date time.Time) error {
	_, err := s.db.Exec(`UPDATE briefs SET delivered_at = ? WHERE for_date = ?`,
		time.Now().UTC(), date.Format("2006-01-02"))
	if err != nil {
		return fmt.Errorf("ledger: mark brief delivered: %w", err)
	}
	return nil
}

// MarkBriefOpened records that someone read it.
func (s *Store) MarkBriefOpened(date time.Time) error {
	_, err := s.db.Exec(
		`UPDATE briefs SET opened_at = COALESCE(opened_at, ?) WHERE for_date = ?`,
		time.Now().UTC(), date.Format("2006-01-02"))
	if err != nil {
		return fmt.Errorf("ledger: mark brief opened: %w", err)
	}
	return nil
}
