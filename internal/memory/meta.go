package memory

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status is the application-level state of a memory, kept outside the graph.
//
// memoryweb stores what the fact says; it has no opinion on whether anyone
// agreed with it. The spec is explicit that this distinction is the
// application's job, and it matters: presenting an inference as settled fact is
// how an assistant loses a contractor's trust in one wrong confident sentence.
type Status string

const (
	// Observed is a fact taken directly from source data — a gate code that
	// literally appeared in a calendar entry. True by transcription.
	Observed Status = "observed"
	// Inferred is a pattern the assistant believes it has spotted. Plausible,
	// unconfirmed, and must never be stated as settled.
	Inferred Status = "inferred"
	// Confirmed means a person said yes.
	Confirmed Status = "confirmed"
	// Rejected means a person said no. The node is archived, not deleted.
	Rejected Status = "rejected"
)

// writableStatuses are the only states a new memory may be created in.
// Confirmed and Rejected are reachable only by a person deciding.
var writableStatuses = map[Status]bool{Observed: true, Inferred: true}

var knownStatuses = map[Status]bool{
	Observed: true, Inferred: true, Confirmed: true, Rejected: true,
}

// Settled reports whether a person has ruled on this memory.
func (s Status) Settled() bool { return s == Confirmed || s == Rejected }

// Trusted reports whether the assistant may state this as fact rather than
// hedging it.
func (s Status) Trusted() bool { return s == Observed || s == Confirmed }

// SourceRef points at the ledger row a memory was drawn from, so "why do you
// think that?" resolves to real records rather than a remembered sentence.
type SourceRef struct {
	Kind  string // appointment | event | invoice | job | contact
	RefID string
}

// Provenance is the evidence behind a memory. Required on every write.
type Provenance struct {
	// SourceType is where the belief came from: calendar, email, webhook,
	// contractor, extraction.
	SourceType string
	// Integration names the connector, when one was involved.
	Integration string
	// ObservedAt is when the underlying evidence was seen.
	ObservedAt time.Time
	// Note is the human-readable summary shown under "why do you think that?".
	Note string
	// Sources are the ledger rows supporting it.
	Sources []SourceRef
}

// Meta is a memory's status and provenance together.
type Meta struct {
	NodeID    string
	Status    Status
	DecidedAt *time.Time
	Provenance
}

// ErrNoProvenance is returned when a write carries no evidence. A memory
// nobody can trace is one nobody can correct.
var ErrNoProvenance = errors.New("memory: provenance is required — a belief with no traceable source cannot be explained or corrected")

// ErrNoMeta reports that a node has no recorded status or provenance.
var ErrNoMeta = errors.New("memory: no metadata for that node")

const metaSchema = `
-- Status and provenance live beside the ledger rows they cite, so "why do you
-- think that?" is a join rather than a cross-database lookup. The graph stays
-- the graph.
CREATE TABLE IF NOT EXISTS memory_meta (
    node_id      TEXT PRIMARY KEY,
    status       TEXT NOT NULL,
    source_type  TEXT NOT NULL,
    integration  TEXT,
    observed_at  DATETIME NOT NULL,
    decided_at   DATETIME,
    note         TEXT NOT NULL,
    created_at   DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_meta_status ON memory_meta(status);

CREATE TABLE IF NOT EXISTS memory_sources (
    node_id  TEXT NOT NULL,
    kind     TEXT NOT NULL,
    ref_id   TEXT NOT NULL,
    PRIMARY KEY (node_id, kind, ref_id)
);
CREATE INDEX IF NOT EXISTS idx_memory_sources_ref ON memory_sources(kind, ref_id);
`

// metaStore persists status and provenance. It owns its tables in whatever
// database it is handed — in practice the tenant's ledger, because that is
// where the rows its provenance cites already are.
type metaStore struct{ db *sql.DB }

func newMetaStore(db *sql.DB) (*metaStore, error) {
	if db == nil {
		return nil, errors.New("memory: a database handle is required for status and provenance")
	}
	if _, err := db.Exec(metaSchema); err != nil {
		return nil, fmt.Errorf("memory: create metadata tables: %w", err)
	}
	return &metaStore{db: db}, nil
}

// put writes a memory's status and provenance atomically with its sources.
func (m *metaStore) put(meta Meta) error {
	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("memory: record metadata: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := time.Now().UTC()
	if _, err := tx.Exec(`
        INSERT INTO memory_meta
            (node_id, status, source_type, integration, observed_at, decided_at, note, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(node_id) DO UPDATE SET
            status      = excluded.status,
            source_type = excluded.source_type,
            integration = excluded.integration,
            observed_at = excluded.observed_at,
            note        = excluded.note`,
		meta.NodeID, string(meta.Status), meta.SourceType, nullable(meta.Integration),
		meta.ObservedAt.UTC(), meta.DecidedAt, meta.Note, now); err != nil {
		return fmt.Errorf("memory: record metadata for %s: %w", meta.NodeID, err)
	}

	for _, src := range meta.Sources {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO memory_sources (node_id, kind, ref_id) VALUES (?, ?, ?)`,
			meta.NodeID, src.Kind, src.RefID); err != nil {
			return fmt.Errorf("memory: record source for %s: %w", meta.NodeID, err)
		}
	}
	return tx.Commit()
}

// get loads a memory's status and provenance.
func (m *metaStore) get(nodeID string) (Meta, error) {
	var (
		meta        Meta
		integration sql.NullString
		decidedAt   sql.NullTime
	)
	err := m.db.QueryRow(`
        SELECT node_id, status, source_type, integration, observed_at, decided_at, note
        FROM memory_meta WHERE node_id = ?`, nodeID,
	).Scan(&meta.NodeID, &meta.Status, &meta.SourceType, &integration,
		&meta.ObservedAt, &decidedAt, &meta.Note)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, ErrNoMeta
	}
	if err != nil {
		return Meta{}, fmt.Errorf("memory: read metadata for %s: %w", nodeID, err)
	}
	if integration.Valid {
		meta.Integration = integration.String
	}
	if decidedAt.Valid {
		t := decidedAt.Time
		meta.DecidedAt = &t
	}

	rows, err := m.db.Query(
		`SELECT kind, ref_id FROM memory_sources WHERE node_id = ? ORDER BY kind, ref_id`, nodeID)
	if err != nil {
		return Meta{}, fmt.Errorf("memory: read sources for %s: %w", nodeID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var s SourceRef
		if err := rows.Scan(&s.Kind, &s.RefID); err != nil {
			return Meta{}, fmt.Errorf("memory: scan source: %w", err)
		}
		meta.Sources = append(meta.Sources, s)
	}
	return meta, rows.Err()
}

// getMany loads metadata for a set of nodes in one query, so listing a page of
// memories does not become one query per row.
func (m *metaStore) getMany(nodeIDs []string) (map[string]Meta, error) {
	out := make(map[string]Meta, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}

	args := make([]any, len(nodeIDs))
	for i, id := range nodeIDs {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(nodeIDs)), ",")

	rows, err := m.db.Query(`
        SELECT node_id, status, source_type, integration, observed_at, decided_at, note
        FROM memory_meta WHERE node_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: read metadata: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			meta        Meta
			integration sql.NullString
			decidedAt   sql.NullTime
		)
		if err := rows.Scan(&meta.NodeID, &meta.Status, &meta.SourceType, &integration,
			&meta.ObservedAt, &decidedAt, &meta.Note); err != nil {
			return nil, fmt.Errorf("memory: scan metadata: %w", err)
		}
		if integration.Valid {
			meta.Integration = integration.String
		}
		if decidedAt.Valid {
			t := decidedAt.Time
			meta.DecidedAt = &t
		}
		out[meta.NodeID] = meta
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Sources in the same batch. Without this the list view shows provenance
	// with no citations, which is provenance that cannot be checked.
	srcRows, err := m.db.Query(
		`SELECT node_id, kind, ref_id FROM memory_sources WHERE node_id IN (`+placeholders+`)
		 ORDER BY node_id, kind, ref_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: read sources: %w", err)
	}
	defer srcRows.Close()

	for srcRows.Next() {
		var nodeID string
		var src SourceRef
		if err := srcRows.Scan(&nodeID, &src.Kind, &src.RefID); err != nil {
			return nil, fmt.Errorf("memory: scan source: %w", err)
		}
		if meta, ok := out[nodeID]; ok {
			meta.Sources = append(meta.Sources, src)
			out[nodeID] = meta
		}
	}
	return out, srcRows.Err()
}

// setStatus records a person's decision.
func (m *metaStore) setStatus(nodeID string, s Status, at time.Time) error {
	res, err := m.db.Exec(
		`UPDATE memory_meta SET status = ?, decided_at = ? WHERE node_id = ?`,
		string(s), at.UTC(), nodeID)
	if err != nil {
		return fmt.Errorf("memory: set status for %s: %w", nodeID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: set status for %s: %w", nodeID, err)
	}
	if n == 0 {
		return ErrNoMeta
	}
	return nil
}

// listByStatus returns node IDs in a given state, newest first.
func (m *metaStore) listByStatus(s Status, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := m.db.Query(
		`SELECT node_id FROM memory_meta WHERE status = ? ORDER BY created_at DESC LIMIT ?`,
		string(s), limit)
	if err != nil {
		return nil, fmt.Errorf("memory: list %s memories: %w", s, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("memory: scan node id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// validate checks provenance before anything reaches the graph.
func (p Provenance) validate() error {
	if strings.TrimSpace(p.SourceType) == "" {
		return ErrNoProvenance
	}
	if strings.TrimSpace(p.Note) == "" {
		return ErrNoProvenance
	}
	if p.ObservedAt.IsZero() {
		return ErrNoProvenance
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
