package obs_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/obs"
	_ "github.com/mattn/go-sqlite3"
)

func recorder(t *testing.T) *obs.Recorder {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	r, err := obs.New(db)
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	return r
}

func TestCountsAndSummarises(t *testing.T) {
	r := recorder(t)

	r.Count(obs.BriefDelivered)
	r.Count(obs.BriefDelivered)
	r.Count(obs.MemoryConfirmed)
	r.Value(obs.EventsIngested, 7)

	sum, err := r.Since(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if sum.Counts[obs.BriefDelivered] != 2 {
		t.Errorf("briefs delivered = %v, want 2", sum.Counts[obs.BriefDelivered])
	}
	if sum.Counts[obs.EventsIngested] != 7 {
		t.Errorf("events ingested = %v, want 7", sum.Counts[obs.EventsIngested])
	}
}

// The cost figure is the one number this table exists to make trustworthy.
func TestCostIsEstimatedPerModel(t *testing.T) {
	// One million input and one million output tokens on Opus 5 is $5 + $25.
	got := obs.CostMicros(obs.AICall{
		Model: "claude-opus-5", Input: 1_000_000, Output: 1_000_000,
	})
	if want := int64(30_000_000); got != want {
		t.Errorf("cost = %d micros ($%.2f), want %d ($%.2f)",
			got, float64(got)/1e6, want, float64(want)/1e6)
	}

	// Cached reads are billed separately and far cheaper. Counting them as
	// input would overstate the bill by roughly what caching saves, hiding the
	// saving this product's margin depends on.
	cached := obs.CostMicros(obs.AICall{Model: "claude-opus-5", CacheRead: 1_000_000})
	fresh := obs.CostMicros(obs.AICall{Model: "claude-opus-5", Input: 1_000_000})
	if cached >= fresh {
		t.Errorf("a cached read (%d) costs as much as fresh input (%d)", cached, fresh)
	}

	// An unpriced model reports zero, which is visibly wrong on the admin page
	// rather than silently plausible.
	if obs.CostMicros(obs.AICall{Model: "some-future-model", Input: 1_000_000}) != 0 {
		t.Error("an unknown model produced a confident price")
	}
}

func TestAIUsageRollsUp(t *testing.T) {
	r := recorder(t)

	r.RecordAI(obs.AICall{
		Model: "claude-opus-5", Operation: "extract",
		Input: 1200, Output: 80, CacheRead: 9000, Latency: 800 * time.Millisecond,
	})
	r.RecordAI(obs.AICall{
		Model: "claude-opus-5", Operation: "brief",
		Input: 400, Output: 200, Latency: 1200 * time.Millisecond, Failed: true,
	})

	sum, err := r.Since(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("since: %v", err)
	}
	if sum.AICalls != 2 {
		t.Errorf("ai calls = %d, want 2", sum.AICalls)
	}
	if sum.AIFailures != 1 {
		t.Errorf("failures = %d, want 1", sum.AIFailures)
	}
	if sum.CostMicros <= 0 {
		t.Error("cost did not accumulate")
	}
	if sum.MedianLatency == 0 {
		t.Error("no latency recorded")
	}

	// Cache hit rate is what shows a silent prefix invalidation as a number
	// rather than as an unexplained bill.
	if rate := sum.CacheHitRate(); rate < 0.8 || rate > 0.9 {
		t.Errorf("cache hit rate = %.2f, want ~0.85 (9000 cached of 10600)", rate)
	}
}

// Instrumentation that can fail a request is worse than instrumentation that
// occasionally misses a row.
func TestRecordingNeverPanicsOnNilRecorder(t *testing.T) {
	var r *obs.Recorder
	r.Count(obs.BriefDelivered)
	r.Value(obs.EventsIngested, 3)
	r.RecordAI(obs.AICall{Model: "claude-opus-5"})
}
