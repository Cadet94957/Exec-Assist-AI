package memory_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
)

// Embeddings are irrelevant to these tests and a live endpoint would only make
// them slow and flaky. memoryweb treats "disabled" as a signal to use literal
// matching, which is exactly what we want to assert against.
func TestMain(m *testing.M) {
	os.Setenv("MEMORYWEB_OLLAMA_ENDPOINT", "disabled")
	os.Exit(m.Run())
}

func open(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memory.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// The guard: memoryweb is a decision log. Anything filed without a reason it
// should change future behaviour is an event, and events belong in the ledger.
// If this test ever stops passing, the graph will fill with noise and recall
// will quietly get worse — which is the failure mode that kills the product.
func TestRecordRequiresWhyMatters(t *testing.T) {
	s := open(t)

	for _, why := range []string{"", "   ", "\t\n"} {
		_, err := s.Record(memory.Fact{
			Label:      "Job 4432 booked for Tuesday 2pm",
			Domain:     "scheduling",
			WhyMatters: why,
		})
		if !errors.Is(err, memory.ErrNoWhyMatters) {
			t.Fatalf("why_matters %q: got %v, want ErrNoWhyMatters", why, err)
		}
	}
}

func TestRecordRequiresLabelAndDomain(t *testing.T) {
	s := open(t)

	if _, err := s.Record(memory.Fact{Domain: "scheduling", WhyMatters: "x"}); err == nil {
		t.Error("expected an error for a missing label")
	}
	if _, err := s.Record(memory.Fact{Label: "something", WhyMatters: "x"}); err == nil {
		t.Error("expected an error for a missing domain")
	}
}

func TestRecordRejectsUnknownKind(t *testing.T) {
	s := open(t)

	_, err := s.Record(memory.Fact{
		Label: "x", Domain: "scheduling", WhyMatters: "y", Kind: memory.Kind("invoice"),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("got %v, want an unknown-kind error", err)
	}
}

func TestRecordDefaultsToFinding(t *testing.T) {
	s := open(t)

	n, err := s.Record(memory.Fact{
		Label:      "Downtown jobs need an extra 45 minutes for parking",
		Domain:     "scheduling",
		WhyMatters: "Back-to-back downtown stops run late without the buffer.",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if n.NodeKind != string(memory.Finding) {
		t.Errorf("node_kind = %q, want %q", n.NodeKind, memory.Finding)
	}
}

func TestRecordThenRecall(t *testing.T) {
	s := open(t)

	if _, err := s.Record(memory.Fact{
		Label:      "Harborview gate code is 4417",
		Detail:     "North entrance keypad; the dock is locked after 3pm.",
		WhyMatters: "Without it the crew waits at the gate and the stop runs 30 minutes late.",
		Domain:     "properties",
		Kind:       memory.Reference,
		Tags:       []string{"access", "harborview"},
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := s.Recall("Harborview", "", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("recall returned nothing")
	}
	if !strings.Contains(got[0].Label, "Harborview") {
		t.Errorf("first result = %q, want it to mention Harborview", got[0].Label)
	}
	if got[0].WhyMatters == "" {
		t.Error("recalled fact lost its why_matters")
	}
}

// The narrative on an edge is the whole point of using a graph rather than a
// list: it is what makes a fact findable by association later.
func TestLinkRequiresBecause(t *testing.T) {
	s := open(t)

	a, err := s.Record(memory.Fact{
		Label: "Moved IG units to Northwest Glass", Domain: "suppliers",
		WhyMatters: "Changes who to call for insulated glass lead times.", Kind: memory.Decision,
	})
	if err != nil {
		t.Fatalf("record a: %v", err)
	}
	b, err := s.Record(memory.Fact{
		Label: "Two late units from Pacific cost us the Marriott job", Domain: "suppliers",
		WhyMatters: "Explains why we stopped using Pacific for anything time-critical.", Kind: memory.Issue,
	})
	if err != nil {
		t.Fatalf("record b: %v", err)
	}

	if _, err := s.Link(a.ID, b.ID, memory.CausedBy, "  "); !errors.Is(err, memory.ErrNoBecause) {
		t.Fatalf("got %v, want ErrNoBecause", err)
	}
	if _, err := s.Link(a.ID, a.ID, memory.CausedBy, "self"); err == nil {
		t.Error("expected an error linking a fact to itself")
	}
	if _, err := s.Link(a.ID, b.ID, memory.Relation("because_of"), "x"); err == nil {
		t.Error("expected an error for an unknown relation")
	}

	edge, err := s.Link(a.ID, b.ID, memory.CausedBy, "Pacific missed two IG deliveries in six weeks.")
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if edge.ID == "" {
		t.Error("link returned an edge with no id")
	}

	// The connection must come back attached to the fact, or the brief can
	// never explain *why* a rule exists.
	withEdges, err := s.Get(a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(withEdges.Edges) == 0 {
		t.Fatal("fact came back with no edges")
	}
}

func TestForgetRequiresReason(t *testing.T) {
	s := open(t)

	n, err := s.Record(memory.Fact{
		Label: "Dave never books after 3pm on Tuesdays", Domain: "scheduling",
		WhyMatters: "Kid pickup — booking over it means cancelling on a customer.",
		Kind:       memory.Standing,
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	if err := s.Forget(n.ID, ""); err == nil {
		t.Error("expected an error forgetting without a reason")
	}
	if err := s.Forget(n.ID, "School run moved to Thursdays."); err != nil {
		t.Fatalf("forget: %v", err)
	}

	live, archived, err := s.Counts()
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if live != 0 || archived != 1 {
		t.Errorf("counts = %d live / %d archived, want 0/1", live, archived)
	}
}

func TestLearnedSurfacesFacts(t *testing.T) {
	s := open(t)

	for _, f := range []memory.Fact{
		{Label: "Marriott 4th St bills the GC, not the property manager", Domain: "customers",
			WhyMatters: "Invoicing the property manager gets it rejected and delays payment by a month.", Kind: memory.Standing},
		{Label: "Marriott 4th St always wants tempered", Domain: "customers",
			WhyMatters: "Quoting annealed there means requoting and losing a day.", Kind: memory.Standing},
	} {
		if _, err := s.Record(f); err != nil {
			t.Fatalf("record %q: %v", f.Label, err)
		}
	}

	// A young graph has no inbound edges, so significance ranks nothing. The
	// page still has to show what has been learned — this is the regression
	// guard for a "What I've learned" page that renders empty on day one,
	// which is exactly the day it has to earn the subscription.
	sum, err := s.Learned("customers", 10)
	if err != nil {
		t.Fatalf("learned: %v", err)
	}
	if len(sum.Important)+len(sum.Recent) != 2 {
		t.Fatalf("learned surfaced %d important + %d recent, want 2 facts in total",
			len(sum.Important), len(sum.Recent))
	}
	for _, f := range sum.Recent {
		if f.WhyMatters == "" {
			t.Errorf("fact %q came back with no why_matters", f.Label)
		}
		if f.Kind != memory.Standing {
			t.Errorf("fact %q has kind %q, want standing", f.Label, f.Kind)
		}
	}

	// Scoping to a domain with nothing in it must be empty, not everything.
	empty, err := s.Learned("suppliers", 10)
	if err != nil {
		t.Fatalf("learned(suppliers): %v", err)
	}
	if len(empty.Important)+len(empty.Recent) != 0 {
		t.Errorf("an empty domain returned %d facts", len(empty.Important)+len(empty.Recent))
	}

	all, err := s.All("", 50)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("All returned %d facts, want 2", len(all))
	}

	domains, err := s.Domains()
	if err != nil {
		t.Fatalf("domains: %v", err)
	}
	if len(domains) != 1 || domains[0] != "customers" {
		t.Errorf("domains = %v, want [customers]", domains)
	}
}
