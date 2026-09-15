// Package obs records what the product actually does and what it costs.
//
// §25 asks for instrumentation from day one, and gives the reason: the
// economics and the usefulness of this product are both still hypotheses. That
// only helps if the numbers end up somewhere a person will look, so these are
// rows in the tenant's own database rather than log lines nobody greps.
package obs

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Metric names. Kept as constants because a typo in a metric name produces a
// silently empty chart rather than an error.
const (
	BriefGenerated   = "brief.generated"
	BriefDelivered   = "brief.delivered"
	BriefFailed      = "brief.failed"
	BriefOpened      = "brief.opened"
	QuestionAsked    = "question.asked"
	ConflictDetected = "conflict.detected"
	MemoryProposed   = "memory.proposed"
	MemoryConfirmed  = "memory.confirmed"
	MemoryRejected   = "memory.rejected"
	SyncSucceeded    = "integration.sync.ok"
	SyncFailed       = "integration.sync.failed"
	EventsIngested   = "integration.events"
)

const schema = `
CREATE TABLE IF NOT EXISTS metrics (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    at     DATETIME NOT NULL,
    name   TEXT NOT NULL,
    value  REAL NOT NULL DEFAULT 1,
    detail TEXT
);
CREATE INDEX IF NOT EXISTS idx_metrics_name_at ON metrics(name, at);

-- Money is tracked in millionths of a dollar as an integer. Storing currency
-- as a float invites rounding drift into the one number this table exists to
-- make trustworthy.
CREATE TABLE IF NOT EXISTS ai_usage (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    at            DATETIME NOT NULL,
    model         TEXT NOT NULL,
    operation     TEXT NOT NULL,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read    INTEGER NOT NULL DEFAULT 0,
    cache_write   INTEGER NOT NULL DEFAULT 0,
    latency_ms    INTEGER NOT NULL DEFAULT 0,
    failed        INTEGER NOT NULL DEFAULT 0,
    cost_micros   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ai_usage_at ON ai_usage(at);
`

// Recorder writes metrics for one tenant.
type Recorder struct{ db *sql.DB }

// New prepares the metrics tables in the tenant's database.
func New(db *sql.DB) (*Recorder, error) {
	if db == nil {
		return nil, fmt.Errorf("obs: a database handle is required")
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("obs: create metrics tables: %w", err)
	}
	return &Recorder{db: db}, nil
}

// Count records one occurrence of a named product event.
func (r *Recorder) Count(name string) { r.record(name, 1, nil) }

// CountWith records an occurrence along with structured detail.
func (r *Recorder) CountWith(name string, detail map[string]any) { r.record(name, 1, detail) }

// Value records a measurement rather than an occurrence.
func (r *Recorder) Value(name string, v float64) { r.record(name, v, nil) }

// record never returns an error. Instrumentation that can fail a request is
// worse than instrumentation that occasionally misses a row.
func (r *Recorder) record(name string, value float64, detail map[string]any) {
	if r == nil {
		return
	}
	var encoded any
	if len(detail) > 0 {
		if b, err := json.Marshal(detail); err == nil {
			encoded = string(b)
		}
	}
	r.db.Exec(`INSERT INTO metrics (at, name, value, detail) VALUES (?, ?, ?, ?)`,
		time.Now().UTC(), name, value, encoded) //nolint:errcheck
}

// AICall is one model call's cost and outcome.
type AICall struct {
	Model      string
	Operation  string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Latency    time.Duration
	Failed     bool
}

// RecordAI stores a model call with its estimated cost.
func (r *Recorder) RecordAI(c AICall) {
	if r == nil {
		return
	}
	failed := 0
	if c.Failed {
		failed = 1
	}
	r.db.Exec(`
        INSERT INTO ai_usage
            (at, model, operation, input_tokens, output_tokens, cache_read, cache_write,
             latency_ms, failed, cost_micros)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UTC(), c.Model, c.Operation, c.Input, c.Output, c.CacheRead, c.CacheWrite,
		c.Latency.Milliseconds(), failed, CostMicros(c)) //nolint:errcheck
}

// price is per million tokens, in millionths of a dollar.
type price struct{ input, output, cacheRead, cacheWrite int64 }

// prices covers the models this product uses. A model missing here costs zero,
// which is visibly wrong on the admin page rather than silently plausible.
var prices = map[string]price{
	// Cache reads are a tenth of input; cache writes carry a 25% premium.
	"claude-opus-5":    {input: 5_000_000, output: 25_000_000, cacheRead: 500_000, cacheWrite: 6_250_000},
	"claude-sonnet-5":  {input: 2_000_000, output: 10_000_000, cacheRead: 200_000, cacheWrite: 2_500_000},
	"claude-haiku-4-5": {input: 1_000_000, output: 5_000_000, cacheRead: 100_000, cacheWrite: 1_250_000},
}

// CostMicros estimates a call's cost in millionths of a dollar.
//
// Cached reads are billed separately from fresh input, so counting them as
// input would overstate the bill by roughly the amount prompt caching saves —
// which would hide the saving this product depends on.
func CostMicros(c AICall) int64 {
	p, ok := prices[c.Model]
	if !ok {
		return 0
	}
	const perMillion = 1_000_000
	return (c.Input*p.input +
		c.Output*p.output +
		c.CacheRead*p.cacheRead +
		c.CacheWrite*p.cacheWrite) / perMillion
}

// Summary is one tenant's rollup over a window.
type Summary struct {
	Since           time.Time
	Counts          map[string]float64
	AICalls         int
	AIFailures      int
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
	CostMicros      int64
	MedianLatency   time.Duration
}

// CostDollars renders the estimated spend.
func (s Summary) CostDollars() float64 { return float64(s.CostMicros) / 1_000_000 }

// CacheHitRate is the share of input tokens served from cache. A rate that
// collapses is the signal that something is silently invalidating the prefix,
// which shows up as a bill before it shows up as a bug.
func (s Summary) CacheHitRate() float64 {
	total := s.InputTokens + s.CacheReadTokens
	if total == 0 {
		return 0
	}
	return float64(s.CacheReadTokens) / float64(total)
}

// Since summarises activity from a point in time.
func (r *Recorder) Since(t time.Time) (Summary, error) {
	out := Summary{Since: t, Counts: map[string]float64{}}

	rows, err := r.db.Query(
		`SELECT name, SUM(value) FROM metrics WHERE at >= ? GROUP BY name`, t.UTC())
	if err != nil {
		return out, fmt.Errorf("obs: summarise metrics: %w", err)
	}
	for rows.Next() {
		var name string
		var v float64
		if err := rows.Scan(&name, &v); err != nil {
			rows.Close()
			return out, fmt.Errorf("obs: scan metric: %w", err)
		}
		out.Counts[name] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	err = r.db.QueryRow(`
        SELECT COUNT(*), COALESCE(SUM(failed),0), COALESCE(SUM(input_tokens),0),
               COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read),0),
               COALESCE(SUM(cost_micros),0)
        FROM ai_usage WHERE at >= ?`, t.UTC(),
	).Scan(&out.AICalls, &out.AIFailures, &out.InputTokens,
		&out.OutputTokens, &out.CacheReadTokens, &out.CostMicros)
	if err != nil {
		return out, fmt.Errorf("obs: summarise ai usage: %w", err)
	}

	// Median rather than mean: one 90-second retry should not make a fast
	// service look slow.
	var median sql.NullInt64
	r.db.QueryRow(`
        SELECT latency_ms FROM ai_usage WHERE at >= ?
        ORDER BY latency_ms LIMIT 1 OFFSET (SELECT COUNT(*)/2 FROM ai_usage WHERE at >= ?)`,
		t.UTC(), t.UTC()).Scan(&median) //nolint:errcheck
	if median.Valid {
		out.MedianLatency = time.Duration(median.Int64) * time.Millisecond
	}
	return out, nil
}
