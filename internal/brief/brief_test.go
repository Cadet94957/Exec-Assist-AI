package brief_test

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

	"github.com/Cadet94957/Exec-Assist-AI/internal/brief"
	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/llm"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

func TestMain(m *testing.M) {
	os.Setenv("MEMORYWEB_OLLAMA_ENDPOINT", "disabled")
	os.Exit(m.Run())
}

const la = "America/Los_Angeles"

func stores(t *testing.T) (*tenant.Manager, tenant.TenantStore) {
	t.Helper()
	m, err := tenant.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	if _, _, err := m.Create("dave-glass", "Dave's Glass", "glazier", la, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	s, err := m.Stores("dave-glass")
	if err != nil {
		t.Fatalf("stores: %v", err)
	}
	return m, s
}

func prov() memory.Provenance {
	return memory.Provenance{
		SourceType: "contractor", ObservedAt: time.Now().UTC(), Note: "Told to us.",
		Sources: []memory.SourceRef{{Kind: "appointment", RefID: "a1"}},
	}
}

func remember(t *testing.T, s tenant.TenantStore, label, why string, kind memory.Kind, status memory.Status) {
	t.Helper()
	n, err := s.Memory().Record(memory.Fact{
		Label: label, WhyMatters: why, Domain: "properties", Kind: kind,
		Status: memory.Observed, Provenance: prov(),
	})
	if err != nil {
		t.Fatalf("record %q: %v", label, err)
	}
	switch status {
	case memory.Confirmed:
		if err := s.Memory().Confirm(n.ID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
	case memory.Inferred:
		// Re-record as inferred by rejecting nothing; the default write status
		// for an inference is set at creation, so do that instead.
	}
}

// rememberInferred files something the assistant worked out rather than was told.
func rememberInferred(t *testing.T, s tenant.TenantStore, label, why string, kind memory.Kind) {
	t.Helper()
	if _, err := s.Memory().Record(memory.Fact{
		Label: label, WhyMatters: why, Domain: "scheduling", Kind: kind,
		Status: memory.Inferred, Provenance: prov(),
	}); err != nil {
		t.Fatalf("record %q: %v", label, err)
	}
}

func seedDay(t *testing.T, s tenant.TenantStore, day time.Time) {
	t.Helper()
	loc, _ := time.LoadLocation(la)
	at := func(h int) time.Time {
		local := day.In(loc)
		return time.Date(local.Year(), local.Month(), local.Day(), h, 0, 0, 0, loc).UTC()
	}
	for _, a := range []ledger.Appointment{
		{ID: "a1", Title: "Harborview — storefront reglaze", StartsAt: at(8),
			EndsAt:   sql.NullTime{Time: at(10), Valid: true},
			Location: sql.NullString{String: "88 Harbor Way", Valid: true}, Source: "google"},
		{ID: "a2", Title: "Marriott 4th St — lobby panel", StartsAt: at(11),
			EndsAt:   sql.NullTime{Time: at(12), Valid: true},
			Location: sql.NullString{String: "1200 4th St", Valid: true}, Source: "google"},
	} {
		if err := s.Ledger().UpsertAppointment(a); err != nil {
			t.Fatalf("seed appointment: %v", err)
		}
	}
}

// Assemble touches no model at all, so the expensive half of the brief is
// testable for free.
func TestAssembleIsDeterministic(t *testing.T) {
	_, s := stores(t)
	now := time.Date(2026, 9, 16, 15, 0, 0, 0, time.UTC) // 8am in Los Angeles
	seedDay(t, s, now)
	remember(t, s, "Harborview gate code is 4417",
		"Without it the crew waits at the gate.", memory.Reference, memory.Confirmed)

	in, err := brief.Assemble(s, now)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	if in.Business != "Dave's Glass" {
		t.Errorf("business = %q", in.Business)
	}
	if len(in.Schedule.Slots) != 2 {
		t.Fatalf("got %d stops, want 2", len(in.Schedule.Slots))
	}
	if in.Schedule.Slots[0].Title != "Harborview — storefront reglaze" {
		t.Errorf("first stop = %q", in.Schedule.Slots[0].Title)
	}
	// The gate code must have attached itself to the Harborview stop.
	if len(in.Schedule.Slots[0].Notes) != 1 {
		t.Fatalf("Harborview has %d notes, want 1", len(in.Schedule.Slots[0].Notes))
	}
	if in.Empty() {
		t.Error("a day with two stops reported itself empty")
	}
}

func TestAssembleOnAQuietDay(t *testing.T) {
	_, s := stores(t)
	now := time.Date(2026, 9, 16, 15, 0, 0, 0, time.UTC)

	in, err := brief.Assemble(s, now)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if !in.Empty() {
		t.Error("an empty day did not report itself empty")
	}
}

// fakeAnthropic echoes a canned brief and captures the prompt, so what the
// model is actually told can be asserted.
func fakeAnthropic(t *testing.T, headline, body string) (*httptest.Server, *[]byte) {
	t.Helper()
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5",
			"content": []any{map[string]any{
				"type": "tool_use", "id": "toolu_1", "name": "write_brief",
				"input": map[string]any{"headline": headline, "body": body},
			}},
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": 900, "output_tokens": 120, "cache_read_input_tokens": 700},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &captured
}

func client(t *testing.T, base string) *llm.Client {
	t.Helper()
	c, err := llm.New(llm.Config{APIKey: "test-key", BaseURL: base})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return c
}

// §6 is enforced here or nowhere: the model must be told which facts are
// unproven, or it cannot possibly hedge them.
func TestInferredFactsReachThePromptMarkedInferred(t *testing.T) {
	_, s := stores(t)
	now := time.Date(2026, 9, 16, 15, 0, 0, 0, time.UTC)
	seedDay(t, s, now)

	remember(t, s, "Harborview gate code is 4417",
		"Without it the crew waits at the gate.", memory.Reference, memory.Confirmed)
	rememberInferred(t, s, "Marriott 4th St needs an extra 45 minutes for parking",
		"Back-to-back stops there run late without it.", memory.Finding)

	in, err := brief.Assemble(s, now)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	srv, captured := fakeAnthropic(t, "Two stops today.", "7:30 Harborview...")
	if _, err := brief.Render(context.Background(), client(t, srv.URL), in); err != nil {
		t.Fatalf("render: %v", err)
	}

	var req map[string]any
	if err := json.Unmarshal(*captured, &req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	msgs, _ := json.Marshal(req["messages"])
	prompt := string(msgs)

	if !strings.Contains(prompt, "[confirmed] Harborview gate code is 4417") {
		t.Error("a confirmed fact was not marked confirmed in the prompt")
	}
	if !strings.Contains(prompt, "[inferred] Marriott 4th St needs an extra 45 minutes") {
		t.Error("an inferred fact was not marked inferred — the model cannot hedge what it is not told")
	}

	// And the instruction to hedge has to actually be in the system prompt.
	sys, _ := json.Marshal(req["system"])
	if !strings.Contains(string(sys), "Hedge what is not confirmed") {
		t.Error("the hedging rule is missing from the system prompt")
	}
	if !strings.Contains(string(sys), "cache_control") {
		t.Error("the brief prompt is not cached — this runs for every tenant every morning")
	}
}

// The schedule arithmetic must reach the model as a conclusion, not as raw
// times for it to work out.
func TestTightSchedulingIsStatedNotImplied(t *testing.T) {
	_, s := stores(t)
	now := time.Date(2026, 9, 16, 15, 0, 0, 0, time.UTC)
	loc, _ := time.LoadLocation(la)
	local := now.In(loc)
	at := func(h, m int) time.Time {
		return time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, loc).UTC()
	}

	for _, a := range []ledger.Appointment{
		{ID: "a1", Title: "Harborview reglaze", StartsAt: at(8, 0),
			EndsAt: sql.NullTime{Time: at(10, 0), Valid: true}, Source: "google"},
		{ID: "a2", Title: "Marriott 4th St panel", StartsAt: at(10, 20),
			EndsAt: sql.NullTime{Time: at(11, 20), Valid: true}, Source: "google"},
	} {
		if err := s.Ledger().UpsertAppointment(a); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	remember(t, s, "Marriott 4th St needs an extra 45 minutes for downtown parking",
		"Parking is what makes back-to-back downtown jobs run late.", memory.Finding, memory.Confirmed)

	in, err := brief.Assemble(s, now)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	srv, captured := fakeAnthropic(t, "Tight morning.", "...")
	if _, err := brief.Render(context.Background(), client(t, srv.URL), in); err != nil {
		t.Fatalf("render: %v", err)
	}

	var req map[string]any
	json.Unmarshal(*captured, &req)
	msgs, _ := json.Marshal(req["messages"])
	if !strings.Contains(string(msgs), "TIGHT") {
		t.Error("the schedule was tight but the model was left to work that out itself")
	}
}

func TestRenderReturnsHeadlineAndBody(t *testing.T) {
	_, s := stores(t)
	now := time.Date(2026, 9, 16, 15, 0, 0, 0, time.UTC)
	seedDay(t, s, now)

	in, err := brief.Assemble(s, now)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	srv, _ := fakeAnthropic(t, "2 stops today, Harborview first.", "8:00 — Harborview\n11:00 — Marriott")
	out, err := brief.Render(context.Background(), client(t, srv.URL), in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if out.Headline == "" || out.Body == "" {
		t.Fatalf("empty brief: %+v", out)
	}
	// The headline has to fit a lock screen.
	if len(out.Headline) > 120 {
		t.Errorf("headline is %d chars, too long for a notification", len(out.Headline))
	}
	if out.Usage.CacheRead != 700 {
		t.Errorf("cache read not reported: %+v", out.Usage)
	}
}

// An empty brief is a bug, not a quiet day — a quiet day still gets a sentence.
func TestEmptyBriefIsAnError(t *testing.T) {
	_, s := stores(t)
	now := time.Date(2026, 9, 16, 15, 0, 0, 0, time.UTC)
	in, _ := brief.Assemble(s, now)

	srv, _ := fakeAnthropic(t, "Nothing on.", "   ")
	if _, err := brief.Render(context.Background(), client(t, srv.URL), in); err == nil {
		t.Error("an empty body was accepted as a brief")
	}
}

// Money and changes must survive assembly, since they answer two of the seven
// questions §11 asks.
func TestOutstandingMoneyReachesTheBrief(t *testing.T) {
	_, s := stores(t)
	now := time.Date(2026, 9, 16, 15, 0, 0, 0, time.UTC)

	db := s.Ledger().DB()
	if _, err := db.Exec(
		`INSERT INTO contacts (id, name, kind, created_at, updated_at) VALUES ('c1','Marriott 4th St','gc',?,?)`,
		now, now); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := db.Exec(`
        INSERT INTO invoices (id, contact_id, amount_cents, status, due_at, created_at)
        VALUES ('i1','c1',125000,'overdue',?,?)`, now.AddDate(0, 0, -9), now); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	in, err := brief.Assemble(s, now)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if len(in.Money) != 1 {
		t.Fatalf("got %d money items, want 1", len(in.Money))
	}
	if in.Money[0].Customer != "Marriott 4th St" {
		t.Errorf("customer = %q", in.Money[0].Customer)
	}
	if in.Money[0].Amount != "$1250.00" {
		t.Errorf("amount = %q, want $1250.00", in.Money[0].Amount)
	}
	if !strings.Contains(in.Money[0].DueIn, "overdue") {
		t.Errorf("due = %q, want it to say overdue", in.Money[0].DueIn)
	}
	if in.Empty() {
		t.Error("a day with outstanding money is not empty")
	}
}

var _ = filepath.Join
