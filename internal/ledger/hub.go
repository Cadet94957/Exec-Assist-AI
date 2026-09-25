package ledger

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The ledger contains authoritative hub state as well as rebuildable mirrors.
// These tables, memory_meta and memory_sources MUST survive a source re-sync.
const hubSchema = `
CREATE TABLE IF NOT EXISTS hub_connections (
    id TEXT PRIMARY KEY, name TEXT NOT NULL, secret BLOB NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1, created_at DATETIME NOT NULL,
    last_received_at DATETIME
);
CREATE TABLE IF NOT EXISTS external_refs (
    connection_id TEXT NOT NULL REFERENCES hub_connections(id),
    kind TEXT NOT NULL CHECK(kind IN ('contact','property','job')),
    external_id TEXT NOT NULL, entity_id TEXT NOT NULL,
    PRIMARY KEY(connection_id, kind, external_id)
);
CREATE TABLE IF NOT EXISTS hub_receipts (
    connection_id TEXT NOT NULL REFERENCES hub_connections(id),
    event_id TEXT NOT NULL, digest TEXT NOT NULL, kind TEXT NOT NULL,
    job_id TEXT NOT NULL REFERENCES jobs(id), payload TEXT NOT NULL,
    occurred_at DATETIME NOT NULL, received_at DATETIME NOT NULL,
    PRIMARY KEY(connection_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_hub_receipts_job ON hub_receipts(job_id, kind, occurred_at);
CREATE TABLE IF NOT EXISTS commitments (
    id TEXT PRIMARY KEY, job_id TEXT NOT NULL REFERENCES jobs(id),
    title TEXT NOT NULL, owner TEXT NOT NULL DEFAULT 'You',
    waiting_on TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'open'
        CHECK(status IN ('open','waiting','completed')),
    due_at DATETIME NOT NULL, source_connection TEXT NOT NULL, source_event TEXT NOT NULL,
    completed_connection TEXT, completed_event TEXT, completed_at DATETIME,
    created_at DATETIME NOT NULL, UNIQUE(job_id, title),
    FOREIGN KEY(source_connection, source_event) REFERENCES hub_receipts(connection_id, event_id),
    FOREIGN KEY(completed_connection, completed_event) REFERENCES hub_receipts(connection_id, event_id)
);
CREATE TABLE IF NOT EXISTS hub_actions (
    id TEXT PRIMARY KEY, kind TEXT NOT NULL CHECK(kind = 'link_appointment'),
    job_id TEXT NOT NULL REFERENCES jobs(id), target_id TEXT NOT NULL,
    target_title TEXT NOT NULL, target_start DATETIME NOT NULL, target_location TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'proposed' CHECK(status IN ('proposed','executed','rejected')),
    reason TEXT NOT NULL, created_at DATETIME NOT NULL, decided_at DATETIME,
    UNIQUE(kind, job_id, target_id)
);
CREATE TABLE IF NOT EXISTS hub_audit (
    id TEXT PRIMARY KEY, subject_id TEXT NOT NULL, operation TEXT NOT NULL,
    detail TEXT NOT NULL, recorded_at DATETIME NOT NULL
);
`

var ErrHubConflict = errors.New("hub: conflicting or stale update; review the current record")
var ErrHubInput = errors.New("hub: invalid input")

// NewHubID is a random identifier, never derived from untrusted text.
func NewHubID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

type HubConnection struct {
	ID       string
	Name     string
	Secret   []byte
	Enabled  bool
	LastSeen sql.NullTime
}

func (s *Store) AddHubConnection(name string, sealed []byte) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || len(sealed) == 0 {
		return "", ErrHubInput
	}
	id := NewHubID()
	_, err := s.db.Exec(`INSERT INTO hub_connections(id,name,secret,created_at) VALUES(?,?,?,?)`, id, name, sealed, time.Now().UTC())
	return id, err
}

func (s *Store) HubConnection(id string) (HubConnection, error) {
	var c HubConnection
	err := s.db.QueryRow(`SELECT id,name,secret,enabled,last_received_at FROM hub_connections WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.Secret, &c.Enabled, &c.LastSeen)
	return c, err
}

func (s *Store) HubConnections() ([]HubConnection, error) {
	rows, err := s.db.Query(`SELECT id,name,enabled,last_received_at FROM hub_connections ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HubConnection
	for rows.Next() {
		var c HubConnection
		if err := rows.Scan(&c.ID, &c.Name, &c.Enabled, &c.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DisableHubConnection(id string) error {
	_, err := s.db.Exec(`UPDATE hub_connections SET enabled=0 WHERE id=?`, id)
	return err
}

// HubEvent is the v1 receptionist adapter contract. Data cannot request an
// action, select an arbitrary canonical ID, or grant execution permission.
type HubEvent struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	OccurredAt   time.Time `json:"occurred_at"`
	JobID        string    `json:"job_id"`
	Title        string    `json:"title"`
	CustomerID   string    `json:"customer_id"`
	CustomerName string    `json:"customer_name"`
	PropertyID   string    `json:"property_id"`
	Address      string    `json:"address"`
	DueAt        time.Time `json:"due_at,omitempty"`
	Evidence     string    `json:"evidence"`
}

func (e HubEvent) Validate(now time.Time) error {
	if e.Type != "booking.created" && e.Type != "measurement.completed" && e.Type != "estimate.sent" {
		return fmt.Errorf("%w: unsupported event type", ErrHubInput)
	}
	for _, v := range []string{e.ID, e.JobID, e.Title, e.CustomerID, e.CustomerName, e.PropertyID, e.Address, e.Evidence} {
		if strings.TrimSpace(v) == "" || len(v) > 1000 || strings.ContainsAny(v, "\x00\r\n") {
			return fmt.Errorf("%w: identifiers, title, customer, property and evidence are required single-line fields (max 1000 bytes)", ErrHubInput)
		}
	}
	if e.OccurredAt.IsZero() || e.OccurredAt.After(now.Add(5*time.Minute)) {
		return fmt.Errorf("%w: occurred_at is required and cannot be in the future", ErrHubInput)
	}
	if !e.DueAt.IsZero() && e.DueAt.Before(e.OccurredAt) {
		return fmt.Errorf("%w: due_at precedes the event", ErrHubInput)
	}
	return nil
}

type IntakeResult struct {
	JobID     string `json:"job_id"`
	Duplicate bool   `json:"duplicate"`
}

// IngestHubEvent atomically records the receipt, identities and commitments.
// Retries are harmless; reuse of an ID for different content is rejected.
func (s *Store) IngestHubEvent(connection string, e HubEvent, now time.Time) (IntakeResult, error) {
	if err := e.Validate(now); err != nil {
		return IntakeResult{}, err
	}
	e.OccurredAt = e.OccurredAt.UTC()
	e.DueAt = e.DueAt.UTC()
	raw, err := json.Marshal(e)
	if err != nil {
		return IntakeResult{}, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	tx, err := s.db.Begin()
	if err != nil {
		return IntakeResult{}, err
	}
	defer tx.Rollback()
	// Acquire a write lock before reading dedupe state, and reject disabled
	// connections in this same transaction (not only in the HTTP handler).
	res, err := tx.Exec(`UPDATE hub_connections SET last_received_at=? WHERE id=? AND enabled=1`, now.UTC(), connection)
	if err != nil {
		return IntakeResult{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return IntakeResult{}, ErrHubInput
	}
	var prior, jobID string
	err = tx.QueryRow(`SELECT digest,job_id FROM hub_receipts WHERE connection_id=? AND event_id=?`, connection, e.ID).Scan(&prior, &jobID)
	if err == nil {
		if prior != digest {
			return IntakeResult{}, ErrHubConflict
		}
		return IntakeResult{JobID: jobID, Duplicate: true}, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return IntakeResult{}, err
	}
	contactID, err := resolveHubEntity(tx, connection, "contact", e.CustomerID, func(id string) error {
		_, err := tx.Exec(`INSERT INTO contacts(id,name,created_at,updated_at) VALUES(?,?,?,?)`, id, e.CustomerName, now.UTC(), now.UTC())
		return err
	})
	if err != nil {
		return IntakeResult{}, err
	}
	propertyID, err := resolveHubEntity(tx, connection, "property", e.PropertyID, func(id string) error {
		_, err := tx.Exec(`INSERT INTO properties(id,contact_id,label,address,created_at) VALUES(?,?,?,?,?)`, id, contactID, e.Address, e.Address, now.UTC())
		return err
	})
	if err != nil {
		return IntakeResult{}, err
	}
	jobID, err = resolveHubEntity(tx, connection, "job", e.JobID, func(id string) error {
		_, err := tx.Exec(`INSERT INTO jobs(id,contact_id,property_id,title,status,created_at,updated_at) VALUES(?,?,?,?,'lead',?,?)`, id, contactID, propertyID, e.Title, now.UTC(), now.UTC())
		return err
	})
	if err != nil {
		return IntakeResult{}, err
	}
	// A changed external customer/property ID requires a person's review.
	// Never quietly move an existing job to a different customer or property.
	var storedContact, storedProperty string
	if err := tx.QueryRow(`SELECT contact_id,property_id FROM jobs WHERE id=?`, jobID).Scan(&storedContact, &storedProperty); err != nil {
		return IntakeResult{}, err
	}
	if storedContact != contactID || storedProperty != propertyID {
		return IntakeResult{}, ErrHubConflict
	}
	_, err = tx.Exec(`INSERT INTO hub_receipts(connection_id,event_id,digest,kind,job_id,payload,occurred_at,received_at) VALUES(?,?,?,?,?,?,?,?)`, connection, e.ID, digest, e.Type, jobID, string(raw), e.OccurredAt, now.UTC())
	if err != nil {
		return IntakeResult{}, err
	}
	if e.Type == "measurement.completed" {
		due := e.DueAt
		if due.IsZero() {
			due = e.OccurredAt.Add(48 * time.Hour)
		}
		_, err = tx.Exec(`INSERT INTO commitments(id,job_id,title,due_at,source_connection,source_event,created_at)
            VALUES(?,?,'Send estimate',?,?,?,?) ON CONFLICT(job_id,title) DO NOTHING`, NewHubID(), jobID, due, connection, e.ID, now.UTC())
		if err != nil {
			return IntakeResult{}, err
		}
	}
	// Reconcile from receipts in either delivery order. A sent receipt must
	// refer to this exact job and occur after the measurement it resolves.
	_, err = tx.Exec(`UPDATE commitments SET status='completed',waiting_on='',
        completed_connection=(SELECT r.connection_id FROM hub_receipts r JOIN hub_receipts m
            ON m.connection_id=commitments.source_connection AND m.event_id=commitments.source_event
            WHERE r.job_id=commitments.job_id AND r.kind='estimate.sent' AND r.occurred_at>=m.occurred_at
            ORDER BY r.occurred_at,r.event_id LIMIT 1),
        completed_event=(SELECT r.event_id FROM hub_receipts r JOIN hub_receipts m
            ON m.connection_id=commitments.source_connection AND m.event_id=commitments.source_event
            WHERE r.job_id=commitments.job_id AND r.kind='estimate.sent' AND r.occurred_at>=m.occurred_at
            ORDER BY r.occurred_at,r.event_id LIMIT 1),completed_at=?
        WHERE job_id=? AND status!='completed' AND EXISTS (
            SELECT 1 FROM hub_receipts r JOIN hub_receipts m
            ON m.connection_id=commitments.source_connection AND m.event_id=commitments.source_event
            WHERE r.job_id=commitments.job_id AND r.kind='estimate.sent' AND r.occurred_at>=m.occurred_at)`, now.UTC(), jobID)
	if err != nil {
		return IntakeResult{}, err
	}
	if err := auditHub(tx, jobID, "event.received", connection+":"+e.ID, now); err != nil {
		return IntakeResult{}, err
	}
	return IntakeResult{JobID: jobID}, tx.Commit()
}

func resolveHubEntity(tx *sql.Tx, connection, kind, external string, create func(string) error) (string, error) {
	var id string
	err := tx.QueryRow(`SELECT entity_id FROM external_refs WHERE connection_id=? AND kind=? AND external_id=?`, connection, kind, external).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = NewHubID()
	if err := create(id); err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO external_refs(connection_id,kind,external_id,entity_id) VALUES(?,?,?,?)`, connection, kind, external, id)
	return id, err
}

func auditHub(tx *sql.Tx, subject, operation, detail string, now time.Time) error {
	_, err := tx.Exec(`INSERT INTO hub_audit(id,subject_id,operation,detail,recorded_at) VALUES(?,?,?,?,?)`, NewHubID(), subject, operation, detail, now.UTC())
	return err
}

type Commitment struct {
	ID             string
	JobID          string
	Title          string
	Job            string
	Customer       string
	Address        string
	Owner          string
	WaitingOn      string
	Status         string
	DueAt          time.Time
	SourceEvent    string
	SourceName     string
	Evidence       string
	CompletedEvent string
}

func (s *Store) Commitments(includeCompleted bool) ([]Commitment, error) {
	rows, err := s.db.Query(`SELECT t.id,t.job_id,t.title,j.title,c.name,p.address,t.owner,t.waiting_on,t.status,t.due_at,
        t.source_event,h.name,r.payload,COALESCE(t.completed_event,'')
        FROM commitments t JOIN jobs j ON j.id=t.job_id JOIN contacts c ON c.id=j.contact_id
        JOIN properties p ON p.id=j.property_id JOIN hub_connections h ON h.id=t.source_connection
        JOIN hub_receipts r ON r.connection_id=t.source_connection AND r.event_id=t.source_event
        WHERE (? OR t.status!='completed') ORDER BY t.due_at,t.id LIMIT 200`, includeCompleted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Commitment
	for rows.Next() {
		var c Commitment
		var raw string
		if err := rows.Scan(&c.ID, &c.JobID, &c.Title, &c.Job, &c.Customer, &c.Address, &c.Owner, &c.WaitingOn, &c.Status, &c.DueAt, &c.SourceEvent, &c.SourceName, &raw, &c.CompletedEvent); err != nil {
			return nil, err
		}
		var e HubEvent
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, err
		}
		c.Evidence = e.Evidence
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCommitment cannot manufacture completion. Only a matching receipt
// closes the estimate loop; the user may change ownership and waiting state.
func (s *Store) UpdateCommitment(id, owner, waiting string) error {
	owner, waiting = strings.TrimSpace(owner), strings.TrimSpace(waiting)
	if owner == "" || len(owner) > 100 || len(waiting) > 200 {
		return ErrHubInput
	}
	status := "open"
	if waiting != "" {
		status = "waiting"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE commitments SET owner=?,waiting_on=?,status=? WHERE id=? AND status!='completed'`, owner, waiting, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrHubConflict
	}
	if err := auditHub(tx, id, "commitment.updated", owner+"; waiting on: "+waiting, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

type HubJob struct {
	ID       string
	Title    string
	Customer string
	Address  string
}

func (s *Store) HubJobs() ([]HubJob, error) {
	rows, err := s.db.Query(`SELECT j.id,j.title,c.name,p.address FROM jobs j
        JOIN contacts c ON c.id=j.contact_id JOIN properties p ON p.id=j.property_id
        WHERE EXISTS(SELECT 1 FROM external_refs r WHERE r.kind='job' AND r.entity_id=j.id)
        ORDER BY j.created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HubJob
	for rows.Next() {
		var j HubJob
		if err := rows.Scan(&j.ID, &j.Title, &j.Customer, &j.Address); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

type HubAction struct {
	ID          string
	Job         string
	Appointment string
	StartsAt    time.Time
	Location    string
	Customer    string
	Address     string
	Reason      string
	Status      string
}

// ProposeAppointmentLink creates a specific, immutable action for review.
// Proposal and approval are separate requests; source data cannot approve it.
func (s *Store) ProposeAppointmentLink(jobID, appointmentID string) error {
	res, err := s.db.Exec(`INSERT INTO hub_actions(id,kind,job_id,target_id,target_title,target_start,target_location,reason,created_at)
        SELECT ?,'link_appointment',j.id,a.id,a.title,a.starts_at,COALESCE(a.location,''),'Link these records to share job context; the calendar will not be changed.',?
        FROM jobs j, appointments a WHERE j.id=? AND a.id=? AND a.job_id IS NULL
        AND EXISTS(SELECT 1 FROM external_refs r WHERE r.kind='job' AND r.entity_id=j.id)
        ON CONFLICT(kind,job_id,target_id) DO UPDATE SET status='proposed',decided_at=NULL,
            target_title=excluded.target_title,target_start=excluded.target_start,target_location=excluded.target_location,
            created_at=excluded.created_at WHERE hub_actions.status='rejected' `, NewHubID(), time.Now().UTC(), jobID, appointmentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrHubConflict
	}
	return nil
}

func (s *Store) HubActions() ([]HubAction, error) {
	rows, err := s.db.Query(`SELECT x.id,j.title,x.target_title,x.target_start,x.target_location,c.name,p.address,x.reason,x.status
        FROM hub_actions x JOIN jobs j ON j.id=x.job_id JOIN contacts c ON c.id=j.contact_id JOIN properties p ON p.id=j.property_id
        WHERE x.status='proposed' ORDER BY x.created_at LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HubAction
	for rows.Next() {
		var a HubAction
		if err := rows.Scan(&a.ID, &a.Job, &a.Appointment, &a.StartsAt, &a.Location, &a.Customer, &a.Address, &a.Reason, &a.Status); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DecideHubAction checks current state and records execution in the same
// transaction as the link. A repeated approval never executes a second time.
func (s *Store) DecideHubAction(id string, approve bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	status := "rejected"
	if approve {
		status = "executed"
	}
	res, err := tx.Exec(`UPDATE hub_actions SET status=?,decided_at=? WHERE id=? AND status='proposed'`, status, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrHubConflict
	}
	if approve {
		res, err = tx.Exec(`UPDATE appointments SET job_id=(SELECT job_id FROM hub_actions WHERE id=?),
            contact_id=(SELECT j.contact_id FROM jobs j JOIN hub_actions x ON x.job_id=j.id WHERE x.id=?),
            property_id=(SELECT j.property_id FROM jobs j JOIN hub_actions x ON x.job_id=j.id WHERE x.id=?)
            WHERE id=(SELECT target_id FROM hub_actions WHERE id=?) AND job_id IS NULL
            AND title=(SELECT target_title FROM hub_actions WHERE id=?)
            AND starts_at=(SELECT target_start FROM hub_actions WHERE id=?)
            AND COALESCE(location,'')=(SELECT target_location FROM hub_actions WHERE id=?)`, id, id, id, id, id, id, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrHubConflict
		}
	}
	if err := auditHub(tx, id, "action."+status, "Explicit account-owner decision", time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}
