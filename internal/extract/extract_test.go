package extract_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/extract"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/llm"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
)

func TestMain(m *testing.M) {
	os.Setenv("MEMORYWEB_OLLAMA_ENDPOINT", "disabled")
	os.Exit(m.Run())
}

// fakeAnthropic replies with a canned tool call and captures what was sent, so
// the full SDK encoding path is exercised without network or spend.
func fakeAnthropic(t *testing.T, toolInput any) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var captured []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		captured = append(captured, req)

		content := []any{}
		if toolInput != nil {
			content = append(content, map[string]any{
				"type": "tool_use", "id": "toolu_1", "name": "record_facts", "input": toolInput,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5",
			"content": content, "stop_reason": "tool_use",
			"usage": map[string]any{"input_tokens": 1200, "output_tokens": 80, "cache_read_input_tokens": 900},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func testStores(t *testing.T) extract.Stores {
	t.Helper()
	dir := t.TempDir()
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	mem, err := memory.Open(filepath.Join(dir, "memory.db"), led.DB())
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	t.Cleanup(func() { led.Close(); mem.Close() })
	return extract.Stores{Ledger: led, Memory: mem}
}

func seedAppointments(t *testing.T, s extract.Stores, now time.Time, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		day := now.Add(-time.Duration(i) * 24 * time.Hour)
		if err := s.Ledger.UpsertAppointment(ledger.Appointment{
			ID:       "a" + string(rune('a'+i%26)) + day.Format("0102"),
			Title:    "Marriott 4th St — lobby panel",
			StartsAt: day,
			Location: sql.NullString{String: "1200 4th St", Valid: true},
			Source:   "google",
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func client(t *testing.T, baseURL string) *llm.Client {
	t.Helper()
	c, err := llm.New(llm.Config{APIKey: "test-key", BaseURL: baseURL})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func goodFact() map[string]any {
	return map[string]any{
		"label":       "Marriott 4th St bills the general contractor, not the property manager",
		"why_matters": "Invoicing the property manager gets it rejected and delays payment by a month",
		"domain":      "customers",
		"kind":        "standing",
		"basis":       "inferred",
		"evidence":    "Six jobs at 1200 4th St over three months, all titled with the GC",
		"source_refs": []any{1, 2},
	}
}

func TestRunRecordsValidFacts(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 6)

	srv, _ := fakeAnthropic(t, map[string]any{"facts": []any{goodFact()}})
	res, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Recorded != 1 {
		t.Fatalf("recorded = %d, want 1 (proposed %d, rejected %d)", res.Recorded, res.Proposed, res.Rejected)
	}
	if res.Usage.CacheRead != 900 {
		t.Errorf("cache read tokens = %d, want 900 — caching is not being reported", res.Usage.CacheRead)
	}

	all, err := stores.Memory.All("", 50)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("graph has %d facts, want 1", len(all))
	}

	// An inference is not knowledge until a person agrees with it.
	if !all[0].AwaitingReview() {
		t.Errorf("an extracted fact was recorded as %q, want inferred", all[0].Status)
	}
	if all[0].Kind != memory.Standing {
		t.Errorf("kind = %q, want standing", all[0].Kind)
	}
	// The evidence has to be traceable to real rows, or nobody can audit why
	// the assistant believes something.
	if !strings.Contains(all[0].Provenance.Note, "Six jobs at 1200 4th St") {
		t.Errorf("evidence was not kept with the fact: %q", all[0].Provenance.Note)
	}
	if len(all[0].Provenance.Sources) == 0 {
		t.Fatal("the fact cites no ledger rows — it cannot be traced back")
	}
	for _, src := range all[0].Provenance.Sources {
		if src.Kind != "appointment" || src.RefID == "" {
			t.Errorf("bad source ref %+v", src)
		}
	}
}

// §7: a claim that cites nothing real is exactly the plausible invention this
// pipeline exists to keep out.
func TestRunRejectsFactsCitingNothing(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 3)

	bad := goodFact()
	bad["source_refs"] = []any{}

	srv, _ := fakeAnthropic(t, map[string]any{"facts": []any{bad}})
	res, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Recorded != 0 || res.Rejected != 1 {
		t.Errorf("recorded=%d rejected=%d, want 0 and 1", res.Recorded, res.Rejected)
	}
}

// A reference the model invented must not resolve to anything.
func TestRunIgnoresInventedSourceRefs(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 3)

	bad := goodFact()
	bad["source_refs"] = []any{99, 100}

	srv, _ := fakeAnthropic(t, map[string]any{"facts": []any{bad}})
	res, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Recorded != 0 {
		t.Error("a fact citing appointments that do not exist was recorded")
	}
}

// §6: extraction may mark something it read verbatim as observed.
func TestObservedBasisIsHonoured(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 3)

	f := goodFact()
	f["basis"] = "observed"
	f["label"] = "Harborview gate code is 4417"

	srv, _ := fakeAnthropic(t, map[string]any{"facts": []any{f}})
	if _, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now); err != nil {
		t.Fatalf("run: %v", err)
	}

	all, err := stores.Memory.All("", 10)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d facts, want 1", len(all))
	}
	if all[0].Status != memory.Observed {
		t.Errorf("status = %q, want observed", all[0].Status)
	}
}

// The single most important test here: a fact with no evidence is exactly the
// plausible invention this whole design exists to keep out.
func TestRunRejectsUnevidencedFacts(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 3)

	bad := goodFact()
	bad["evidence"] = "   "

	srv, _ := fakeAnthropic(t, map[string]any{"facts": []any{bad}})
	res, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Recorded != 0 || res.Rejected != 1 {
		t.Errorf("recorded=%d rejected=%d, want 0 and 1", res.Recorded, res.Rejected)
	}
}

func TestRunRejectsMalformedFacts(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	cases := map[string]func(map[string]any){
		"no why_matters":  func(f map[string]any) { f["why_matters"] = "" },
		"no label":        func(f map[string]any) { f["label"] = "  " },
		"invented domain": func(f map[string]any) { f["domain"] = "vibes" },
		"invalid kind":    func(f map[string]any) { f["kind"] = "invoice" },
	}

	for name, mangle := range cases {
		t.Run(name, func(t *testing.T) {
			stores := testStores(t)
			seedAppointments(t, stores, now, 3)

			f := goodFact()
			mangle(f)
			srv, _ := fakeAnthropic(t, map[string]any{"facts": []any{f}})

			res, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.Recorded != 0 {
				t.Errorf("recorded %d facts, want 0", res.Recorded)
			}
		})
	}
}

// Extraction re-reads the same 90-day window on every run. Without dedupe the
// graph would accumulate a fresh copy of every rule on every pass.
func TestRepeatedRunsDoNotDuplicate(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 6)

	srv, _ := fakeAnthropic(t, map[string]any{"facts": []any{goodFact()}})
	c := client(t, srv.URL)

	for i := 0; i < 3; i++ {
		if _, err := extract.Run(context.Background(), c, stores, time.UTC, now); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	all, err := stores.Memory.All("", 50)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("after 3 runs the graph has %d facts, want 1", len(all))
	}
}

func TestRunCapsRunawayProposals(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 10)

	var many []any
	for i := 0; i < 25; i++ {
		f := goodFact()
		f["label"] = "Distinct rule number " + string(rune('a'+i))
		many = append(many, f)
	}

	srv, _ := fakeAnthropic(t, map[string]any{"facts": many})
	res, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Recorded > 10 {
		t.Errorf("recorded %d facts from one run, want the cap to hold at 10", res.Recorded)
	}
}

// Returning nothing is the correct answer most of the time and must not be an
// error, or the logs fill with failures for normal operation.
func TestEmptyResultIsNotAnError(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 3)

	srv, _ := fakeAnthropic(t, map[string]any{"facts": []any{}})
	res, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now)
	if err != nil {
		t.Fatalf("empty result returned an error: %v", err)
	}
	if res.Recorded != 0 {
		t.Errorf("recorded = %d, want 0", res.Recorded)
	}
}

func TestNoToolCallIsNotAnError(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 3)

	srv, _ := fakeAnthropic(t, nil) // model replies without calling the tool
	if _, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now); err != nil {
		t.Fatalf("no tool call returned an error: %v", err)
	}
}

func TestNoAppointmentsSkipsTheCallEntirely(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)

	srv, captured := fakeAnthropic(t, map[string]any{"facts": []any{goodFact()}})
	if _, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*captured) != 0 {
		t.Error("called the API with no appointments to reason about — that is pure spend")
	}
}

// The request shape is what makes this affordable and grounded: a cached
// system prompt, weekday-bearing evidence, and the facts already known.
func TestRequestIsCachedAndGrounded(t *testing.T) {
	stores := testStores(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	seedAppointments(t, stores, now, 4)

	if _, err := stores.Memory.Record(memory.Fact{
		Label: "Nothing booked after 3pm on Tuesdays", Domain: "scheduling",
		WhyMatters: "Booking over it means cancelling on a customer", Kind: memory.Standing,
		Status: memory.Inferred,
		Provenance: memory.Provenance{
			SourceType: "extraction", ObservedAt: time.Now(), Note: "Seen repeatedly.",
			Sources: []memory.SourceRef{{Kind: "appointment", RefID: "seed"}},
		},
	}); err != nil {
		t.Fatalf("seed known fact: %v", err)
	}

	srv, captured := fakeAnthropic(t, map[string]any{"facts": []any{}})
	if _, err := extract.Run(context.Background(), client(t, srv.URL), stores, time.UTC, now); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("made %d requests, want 1", len(*captured))
	}
	req := (*captured)[0]

	if got := req["model"]; got != "claude-opus-5" {
		t.Errorf("model = %v, want claude-opus-5", got)
	}

	// The system prompt is identical every call; caching it is most of the
	// cost saving, and a silent regression here would just cost money.
	sys, _ := json.Marshal(req["system"])
	if !strings.Contains(string(sys), "cache_control") {
		t.Error("system prompt is not marked for caching")
	}
	if !strings.Contains(string(sys), "empty list is the correct answer") {
		t.Error("the calibration instruction is missing from the system prompt")
	}

	msgs, _ := json.Marshal(req["messages"])
	evidence := string(msgs)
	if !strings.Contains(evidence, "Mon 2026-09-14") {
		t.Error("evidence does not spell out the weekday — day-of-week patterns are invisible without it")
	}
	if !strings.Contains(evidence, "1200 4th St") {
		t.Error("evidence is missing appointment locations")
	}
	if !strings.Contains(evidence, "Nothing booked after 3pm on Tuesdays") {
		t.Error("already-known facts were not sent, so the model will restate them")
	}

	tools, _ := json.Marshal(req["tools"])
	if !strings.Contains(string(tools), "record_facts") {
		t.Error("the record_facts tool was not declared")
	}
	if !strings.Contains(string(tools), "additionalProperties") {
		t.Error("the tool schema is not strict")
	}
}
