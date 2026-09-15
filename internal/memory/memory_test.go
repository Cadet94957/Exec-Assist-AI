package memory_test

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	_ "github.com/mattn/go-sqlite3"
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
	dir := t.TempDir()
	// Status and provenance live in the tenant's ledger database.
	meta, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("open meta db: %v", err)
	}
	s, err := memory.Open(filepath.Join(dir, "memory.db"), meta)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close(); meta.Close() })
	return s
}

// evidence is valid provenance for tests that are not about provenance.
func evidence(refs ...string) memory.Provenance {
	if len(refs) == 0 {
		refs = []string{"appt-1"}
	}
	srcs := make([]memory.SourceRef, len(refs))
	for i, r := range refs {
		srcs[i] = memory.SourceRef{Kind: "appointment", RefID: r}
	}
	return memory.Provenance{
		SourceType: "extraction",
		ObservedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		Note:       "Seen across several jobs in the window.",
		Sources:    srcs,
	}
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
			Provenance: evidence(),
		})
		if !errors.Is(err, memory.ErrNoWhyMatters) {
			t.Fatalf("why_matters %q: got %v, want ErrNoWhyMatters", why, err)
		}
	}
}

func TestRecordRequiresLabelAndDomain(t *testing.T) {
	s := open(t)

	if _, err := s.Record(memory.Fact{Domain: "scheduling", WhyMatters: "x", Provenance: evidence()}); err == nil {
		t.Error("expected an error for a missing label")
	}
	if _, err := s.Record(memory.Fact{Label: "something", WhyMatters: "x", Provenance: evidence()}); err == nil {
		t.Error("expected an error for a missing domain")
	}
}

func TestRecordRejectsUnknownKind(t *testing.T) {
	s := open(t)

	_, err := s.Record(memory.Fact{
		Label: "x", Domain: "scheduling", WhyMatters: "y", Kind: memory.Kind("invoice"),
		Provenance: evidence(),
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
		Provenance: evidence(),
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
		Status:     memory.Observed,
		Provenance: evidence("appt-harborview"),
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
		Provenance: evidence(),
	})
	if err != nil {
		t.Fatalf("record a: %v", err)
	}
	b, err := s.Record(memory.Fact{
		Label: "Two late units from Pacific cost us the Marriott job", Domain: "suppliers",
		WhyMatters: "Explains why we stopped using Pacific for anything time-critical.", Kind: memory.Issue,
		Provenance: evidence(),
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
		Provenance: evidence(),
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
			WhyMatters: "Invoicing the property manager gets it rejected and delays payment by a month.",
			Kind:       memory.Standing, Status: memory.Inferred, Provenance: evidence()},
		{Label: "Marriott 4th St always wants tempered", Domain: "customers",
			WhyMatters: "Quoting annealed there means requoting and losing a day.",
			Kind:       memory.Standing, Status: memory.Inferred, Provenance: evidence()},
	} {
		n, err := s.Record(f)
		if err != nil {
			t.Fatalf("record %q: %v", f.Label, err)
		}
		// A person agreed, which is the only route to confirmed.
		if err := s.Confirm(n.ID); err != nil {
			t.Fatalf("confirm %q: %v", f.Label, err)
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

// §5: a belief with no traceable source cannot be explained or corrected, so
// it must never reach the graph.
func TestRecordRequiresProvenance(t *testing.T) {
	s := open(t)

	base := memory.Fact{
		Label:      "Downtown jobs need an extra 45 minutes",
		Domain:     "scheduling",
		WhyMatters: "Back-to-back downtown stops run late without the buffer.",
	}

	for name, prov := range map[string]memory.Provenance{
		"none":           {},
		"no source type": {ObservedAt: time.Now(), Note: "seen a few times"},
		"no note":        {SourceType: "extraction", ObservedAt: time.Now()},
		"no date":        {SourceType: "extraction", Note: "seen a few times"},
	} {
		t.Run(name, func(t *testing.T) {
			f := base
			f.Provenance = prov
			if _, err := s.Record(f); !errors.Is(err, memory.ErrNoProvenance) {
				t.Errorf("got %v, want ErrNoProvenance", err)
			}
		})
	}
}

// §6: confirmed and rejected are things a person decides. The assistant may
// only ever propose.
func TestRecordCannotCreateSettledStatus(t *testing.T) {
	s := open(t)

	for _, status := range []memory.Status{memory.Confirmed, memory.Rejected, memory.Status("guessed")} {
		_, err := s.Record(memory.Fact{
			Label: "Something", Domain: "scheduling", WhyMatters: "because",
			Status: status, Provenance: evidence(),
		})
		if err == nil {
			t.Errorf("creating a memory as %q was allowed", status)
		}
	}
}

// An unmarked belief must be treated as unproven, not quietly trusted.
func TestRecordDefaultsToInferred(t *testing.T) {
	s := open(t)

	n, err := s.Record(memory.Fact{
		Label: "Downtown stops run long", Domain: "scheduling",
		WhyMatters: "Back-to-back downtown jobs slip without a buffer.",
		Provenance: evidence(),
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	meta, err := s.Why(n.ID)
	if err != nil {
		t.Fatalf("why: %v", err)
	}
	if meta.Status != memory.Inferred {
		t.Errorf("status = %q, want inferred", meta.Status)
	}
	if meta.Status.Trusted() {
		t.Error("an unmarked belief should not be trusted")
	}
}

// §7: "why do you think that?" must resolve to the real rows, not a sentence.
func TestProvenanceRoundTrips(t *testing.T) {
	s := open(t)

	n, err := s.Record(memory.Fact{
		Label: "Marriott 4th St bills the GC", Domain: "customers",
		WhyMatters: "Invoicing the property manager delays payment by a month.",
		Kind:       memory.Standing, Status: memory.Inferred,
		Provenance: memory.Provenance{
			SourceType:  "extraction",
			Integration: "google",
			ObservedAt:  time.Date(2026, 6, 14, 9, 0, 0, 0, time.UTC),
			Note:        "Six jobs at 1200 4th St between May 3 and June 14.",
			Sources: []memory.SourceRef{
				{Kind: "appointment", RefID: "google-evt-183"},
				{Kind: "appointment", RefID: "google-evt-191"},
			},
		},
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	meta, err := s.Why(n.ID)
	if err != nil {
		t.Fatalf("why: %v", err)
	}
	if meta.SourceType != "extraction" || meta.Integration != "google" {
		t.Errorf("source = %q/%q", meta.SourceType, meta.Integration)
	}
	if !strings.Contains(meta.Note, "Six jobs at 1200 4th St") {
		t.Errorf("note = %q", meta.Note)
	}
	if len(meta.Sources) != 2 {
		t.Fatalf("got %d sources, want 2 — the memory cannot be traced back", len(meta.Sources))
	}
	if meta.Sources[0].RefID != "google-evt-183" {
		t.Errorf("first source = %+v", meta.Sources[0])
	}
	if meta.ObservedAt.UTC().Format("2006-01-02") != "2026-06-14" {
		t.Errorf("observed_at = %v", meta.ObservedAt)
	}
}

// §6: the full lifecycle, and the guarantee that a rejection actually stops
// the fact influencing anything.
func TestStatusLifecycle(t *testing.T) {
	s := open(t)

	keep, err := s.Record(memory.Fact{
		Label: "Nothing booked after 3pm on Tuesdays", Domain: "scheduling",
		WhyMatters: "Kid pickup — booking over it means cancelling on a customer.",
		Kind:       memory.Standing, Status: memory.Inferred, Provenance: evidence("a1"),
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	drop, err := s.Record(memory.Fact{
		Label: "Prefers morning starts", Domain: "scheduling",
		WhyMatters: "Would change how the day is ordered.",
		Status:     memory.Inferred, Provenance: evidence("a2"),
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	pending, err := s.Pending(10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("got %d awaiting review, want 2", len(pending))
	}

	if err := s.Confirm(keep.ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := s.Reject(drop.ID, "Not true — mornings are whenever."); err != nil {
		t.Fatalf("reject: %v", err)
	}

	pending, err = s.Pending(10)
	if err != nil {
		t.Fatalf("pending after decisions: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d facts still awaiting review after both were decided", len(pending))
	}

	// The confirmed one is now trusted and still recallable.
	got, err := s.Recall("Tuesdays", "", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the confirmed fact is no longer recallable")
	}
	if got[0].Status != memory.Confirmed {
		t.Errorf("status = %q, want confirmed", got[0].Status)
	}
	if !got[0].Status.Trusted() {
		t.Error("a confirmed fact should be trusted")
	}

	// The rejected one must be gone from recall entirely.
	rejected, err := s.Recall("morning starts", "", 10)
	if err != nil {
		t.Fatalf("recall rejected: %v", err)
	}
	for _, r := range rejected {
		if r.ID == drop.ID {
			t.Error("a rejected fact is still being recalled")
		}
	}
}

// Status must survive retrieval, or the assistant cannot know what to hedge.
func TestStatusTravelsWithRecall(t *testing.T) {
	s := open(t)

	if _, err := s.Record(memory.Fact{
		Label: "Harborview gate code is 4417", Domain: "properties",
		WhyMatters: "Without it the crew waits at the gate.",
		Kind:       memory.Reference, Status: memory.Observed,
		Provenance: evidence("a1"),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := s.Recall("Harborview", "", 10)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("nothing recalled")
	}
	if got[0].Status != memory.Observed {
		t.Errorf("status = %q, want observed", got[0].Status)
	}
	if got[0].AwaitingReview() {
		t.Error("an observed fact should not be in the review queue")
	}
	if got[0].Provenance.Note == "" {
		t.Error("provenance did not survive recall — the UI cannot explain this fact")
	}
}
