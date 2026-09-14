// Package memory is the only path into the tenant's memoryweb knowledge graph.
//
// memoryweb is a decision log, not an event log. Its author made why_matters
// mandatory on purpose: a node without one is an event, and a graph full of
// events is noise that degrades every later recall. Operational records —
// jobs, appointments, invoices — belong in the ledger, never here.
//
// Every write in the product goes through Record or Link so that rule is
// enforced in exactly one place.
package memory

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/corbym/memoryweb/db"
)

// Kind classifies what a fact *is*, mirroring memoryweb's node kinds. The
// database rejects anything outside its own set at insert time; we validate
// first so the caller gets a useful error instead of a driver error.
type Kind string

const (
	// Standing is a durable rule about how this business operates.
	// "Never books after 3pm Tuesday — kid pickup."
	Standing Kind = "standing"
	// Reference is a stable lookup fact. "Harborview gate code 4417."
	Reference Kind = "reference"
	// Finding is something learned from experience.
	// "Downtown jobs need +45 min for parking on weekdays."
	Finding Kind = "finding"
	// Issue is an unresolved problem worth surfacing again.
	Issue Kind = "issue"
	// Decision is a choice made, ideally linked to what caused it.
	Decision Kind = "decision"
	// Goal is an intent the assistant should steer toward.
	Goal Kind = "goal"
	// Assumption is a belief held but not confirmed.
	Assumption Kind = "assumption"
	// Option is a path considered but not chosen.
	Option Kind = "option"
	// Transient is short-lived context that should age out.
	Transient Kind = "transient"
)

var validKinds = map[Kind]bool{
	Standing: true, Reference: true, Finding: true, Issue: true,
	Decision: true, Goal: true, Assumption: true, Option: true, Transient: true,
}

// Relation is a typed edge. The narrative attached to it — the "because" — is
// what makes the graph worth more than a list of facts.
type Relation string

const (
	CausedBy    Relation = "caused_by"
	LedTo       Relation = "led_to"
	BlockedBy   Relation = "blocked_by"
	Unblocks    Relation = "unblocks"
	ConnectsTo  Relation = "connects_to"
	Contradicts Relation = "contradicts"
	DependsOn   Relation = "depends_on"
	IsExampleOf Relation = "is_example_of"
	GovernedBy  Relation = "governed_by"
	Resolved    Relation = "resolved"
	ResolvedBy  Relation = "resolved_by"
	Supersedes  Relation = "supersedes"
)

var validRelations = map[Relation]bool{
	CausedBy: true, LedTo: true, BlockedBy: true, Unblocks: true,
	ConnectsTo: true, Contradicts: true, DependsOn: true, IsExampleOf: true,
	GovernedBy: true, Resolved: true, ResolvedBy: true, Supersedes: true,
}

// ErrNoWhyMatters is returned when a caller tries to file something without
// saying why it should change future behaviour. This is the guard that keeps
// the graph a decision log.
var ErrNoWhyMatters = errors.New("memory: why_matters is required — if it has no bearing on future decisions it belongs in the ledger, not the graph")

// ErrNoBecause is the edge equivalent: an untyped, unexplained connection is
// not worth storing.
var ErrNoBecause = errors.New("memory: because is required — an edge without its narrative is not recallable later")

// Fact is something learned about the business that should change what the
// assistant does next.
type Fact struct {
	// Label is the short human-readable form, shown in briefs and on the
	// "What I've Learned" page.
	Label string
	// Detail is the fuller explanation. Optional.
	Detail string
	// WhyMatters says what this changes about future decisions. Required.
	WhyMatters string
	// Domain groups facts — "scheduling", "customers", "suppliers", "money".
	Domain string
	// Kind classifies the fact. Defaults to Finding when empty.
	Kind Kind
	// Tags are free-form labels for filtering.
	Tags []string
	// OccurredAt is when the underlying thing happened, if it differs from now.
	OccurredAt *time.Time
}

// Recalled is a fact coming back out of the graph. Callers work with this
// rather than memoryweb's own types, so the storage layer stays swappable and
// the templates stay simple.
type Recalled struct {
	ID         string
	Label      string
	Detail     string
	WhyMatters string
	Domain     string
	Kind       Kind
	// Importance is how much the rest of the graph leans on this fact. It is
	// zero for facts that have not been ranked, which is most of them early on.
	Importance float64
}

// Summary is what the assistant knows, arranged for reading.
type Summary struct {
	// Important are the facts the rest of the graph depends on. Empty until
	// enough connections exist to rank anything.
	Important []Recalled
	// Recent is everything else, newest first.
	Recent []Recalled
}

// allKinds lists every node kind, for queries that want everything.
var allKinds = []string{
	string(Transient), string(Reference), string(Issue), string(Decision),
	string(Option), string(Assumption), string(Finding), string(Standing), string(Goal),
}

// Store wraps one tenant's memoryweb graph. It is safe for concurrent use —
// the underlying *sql.DB pools connections and the schema runs in WAL mode.
type Store struct {
	st   *db.Store
	path string
}

// Open opens (creating and migrating if needed) the graph at path.
func Open(path string) (*Store, error) {
	st, err := db.New(path)
	if err != nil {
		return nil, fmt.Errorf("memory: open %s: %w", path, err)
	}
	return &Store{st: st, path: path}, nil
}

// Close checkpoints the WAL and closes the handle. Always call it before
// copying the file — memoryweb leaves recent writes in the -wal sidecar until
// this runs.
func (s *Store) Close() error { return s.st.Close() }

// Path is the on-disk location of this tenant's graph.
func (s *Store) Path() string { return s.path }

// SemanticSearchAvailable reports whether sqlite-vec loaded. When false,
// memoryweb silently falls back to literal LIKE matching — recall still works,
// it is just less associative.
func (s *Store) SemanticSearchAvailable() bool { return s.st.VecAvailable() }

// Record files a fact. It refuses anything without a why_matters, which is the
// single rule that keeps this graph useful.
func (s *Store) Record(f Fact) (*db.Node, error) {
	if strings.TrimSpace(f.Label) == "" {
		return nil, errors.New("memory: label is required")
	}
	if strings.TrimSpace(f.WhyMatters) == "" {
		return nil, ErrNoWhyMatters
	}
	if strings.TrimSpace(f.Domain) == "" {
		return nil, errors.New("memory: domain is required")
	}
	kind := f.Kind
	if kind == "" {
		kind = Finding
	}
	if !validKinds[kind] {
		return nil, fmt.Errorf("memory: unknown kind %q", kind)
	}

	node, err := s.st.AddNode(
		f.Label,
		f.Detail,
		f.WhyMatters,
		f.Domain,
		f.OccurredAt,
		strings.Join(f.Tags, ","),
		string(kind),
	)
	if err != nil {
		return nil, fmt.Errorf("memory: record %q: %w", f.Label, err)
	}
	return node, nil
}

// Link connects two facts with a typed relation and the narrative explaining
// it. "Moved IG units to Northwest Glass" --caused_by--> "Two late units from
// Pacific cost us the Marriott job".
func (s *Store) Link(fromID, toID string, rel Relation, because string) (*db.Edge, error) {
	if fromID == "" || toID == "" {
		return nil, errors.New("memory: both fromID and toID are required")
	}
	if fromID == toID {
		return nil, errors.New("memory: cannot link a fact to itself")
	}
	if !validRelations[rel] {
		return nil, fmt.Errorf("memory: unknown relation %q", rel)
	}
	if strings.TrimSpace(because) == "" {
		return nil, ErrNoBecause
	}
	edge, err := s.st.AddEdge(fromID, toID, string(rel), because)
	if err != nil {
		return nil, fmt.Errorf("memory: link %s->%s: %w", fromID, toID, err)
	}
	return edge, nil
}

// Recall searches the graph. It uses semantic search when sqlite-vec and an
// embedding endpoint are both available, and literal matching otherwise.
// Pass an empty domain to search everything.
func (s *Store) Recall(query, domain string, limit int) ([]Recalled, error) {
	if limit <= 0 {
		limit = 10
	}
	res, err := s.st.SearchNodes(query, domain, limit, "", nil)
	if err != nil {
		return nil, fmt.Errorf("memory: recall %q: %w", query, err)
	}
	return flatten(res), nil
}

// All lists live facts, newest first. Pass an empty domain for everything.
func (s *Store) All(domain string, limit int) ([]Recalled, error) {
	if limit <= 0 {
		limit = 50
	}
	// An empty query with an explicit kind list is memoryweb's plain listing
	// path; passing every kind means "everything".
	res, err := s.st.SearchNodes("", domain, limit, "", allKinds)
	if err != nil {
		return nil, fmt.Errorf("memory: list: %w", err)
	}
	return flatten(res), nil
}

// Get returns one fact with everything it is connected to.
func (s *Store) Get(id string) (*db.NodeWithEdges, error) {
	n, err := s.st.GetNode(id)
	if err != nil {
		return nil, fmt.Errorf("memory: get %s: %w", id, err)
	}
	return n, nil
}

// Learned returns what the assistant considers the most significant things it
// knows, for the "What I've Learned" page. This page is the demo that sells
// the product, so it reads from memoryweb's own significance scoring rather
// than just listing recent rows.
func (s *Store) Learned(domain string, limit int) (Summary, error) {
	if limit <= 0 {
		limit = 25
	}

	// Significance ranks by how much the rest of the graph leans on a fact. On
	// a young graph nothing has inbound edges yet and this is legitimately
	// empty — which is why it can never be the only source for this page.
	sig, err := s.st.GetSignificance(domain, limit, 30, nil, nil, limit)
	if err != nil {
		return Summary{}, fmt.Errorf("memory: significance: %w", err)
	}

	var out Summary
	ranked := make(map[string]bool, len(sig.Structural))
	for _, n := range sig.Structural {
		ranked[n.ID] = true
		out.Important = append(out.Important, recalled(n.Node, n.ImportanceScore))
	}

	// Everything not already shown above, newest first. This is what makes the
	// page worth looking at on day one.
	rest, err := s.All(domain, limit)
	if err != nil {
		return Summary{}, err
	}
	for _, r := range rest {
		if !ranked[r.ID] {
			out.Recent = append(out.Recent, r)
		}
	}
	return out, nil
}

// Domains lists the areas the graph currently has facts about.
func (s *Store) Domains() ([]string, error) {
	d, err := s.st.ListDomains()
	if err != nil {
		return nil, fmt.Errorf("memory: domains: %w", err)
	}
	sort.Strings(d)
	return d, nil
}

// Counts reports live and archived fact counts, for the dashboard.
func (s *Store) Counts() (live, archived int, err error) {
	live, archived, err = s.st.NodeCounts()
	if err != nil {
		return 0, 0, fmt.Errorf("memory: counts: %w", err)
	}
	return live, archived, nil
}

// Forget archives a fact with a reason. memoryweb soft-deletes, so this is
// reversible — which matters when the assistant learns something wrong and the
// contractor corrects it.
func (s *Store) Forget(id, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("memory: a reason is required to forget something")
	}
	if err := s.st.ArchiveNode(id, reason); err != nil {
		return fmt.Errorf("memory: forget %s: %w", id, err)
	}
	return nil
}

// Backup writes a consistent snapshot next to the live database and returns
// its path. memoryweb's Backup refuses a destination outside the source
// directory, so callers upload the returned file and then remove it.
func Backup(srcPath string) (string, error) {
	dest := fmt.Sprintf("%s.%s.bak", srcPath, time.Now().UTC().Format("20060102T150405Z"))
	if err := db.Backup(srcPath, dest); err != nil {
		return "", fmt.Errorf("memory: backup %s: %w", srcPath, err)
	}
	return dest, nil
}

// flatten converts memoryweb's search result into our own type, preserving the
// order the search returned them in.
func flatten(res *db.SearchResult) []Recalled {
	if res == nil {
		return nil
	}
	out := make([]Recalled, 0, len(res.Nodes))
	for _, r := range res.Nodes {
		out = append(out, recalled(r.Node, 0))
	}
	return out
}

func recalled(n db.Node, importance float64) Recalled {
	return Recalled{
		ID:         n.ID,
		Label:      n.Label,
		Detail:     n.Description,
		WhyMatters: n.WhyMatters,
		Domain:     n.Domain,
		Kind:       Kind(n.NodeKind),
		Importance: importance,
	}
}
