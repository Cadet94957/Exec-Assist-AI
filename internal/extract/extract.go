// Package extract turns synced operational data into things worth remembering.
//
// This is the step that makes the product more than a calendar mirror. It
// reads a window of the ledger, asks Claude what general rules the data
// supports, and files the survivors in the memory graph — tagged unconfirmed,
// because an inference is not yet knowledge.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/llm"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
)

// Window is how much history one pass reasons over. Long enough for a weekday
// pattern to be visible, short enough to stay cheap.
const Window = 90 * 24 * time.Hour

// maxAppointments bounds the evidence sent in one call. A busy contractor can
// exceed this; the window is re-read on every run, so nothing is lost
// permanently, and the cost of a single call stays predictable.
const maxAppointments = 400

// maxFactsPerRun is a backstop against a run that decides everything is
// significant. The prompt asks for one to three; anything past this is a
// signal the model is inventing, so the extras are dropped and logged.
const maxFactsPerRun = 10

// validDomains keeps the graph from sprawling. memoryweb degrades when domains
// multiply — recall gets diluted across near-identical buckets — so the model
// picks from a fixed set rather than inventing names.
var validDomains = map[string]bool{
	"scheduling": true, "customers": true, "properties": true,
	"suppliers": true, "money": true, "operations": true,
}

var validKinds = map[string]memory.Kind{
	"standing":  memory.Standing,
	"reference": memory.Reference,
	"finding":   memory.Finding,
	"issue":     memory.Issue,
	"decision":  memory.Decision,
	"goal":      memory.Goal,
}

// Result reports what one pass did.
type Result struct {
	Appointments int
	Proposed     int
	Recorded     int
	Duplicates   int
	Rejected     int
	Usage        llm.Usage
}

// proposal is one candidate fact as the model returns it.
type proposal struct {
	Label      string `json:"label"`
	Detail     string `json:"detail"`
	WhyMatters string `json:"why_matters"`
	Domain     string `json:"domain"`
	Kind       string `json:"kind"`
	Evidence   string `json:"evidence"`
}

type proposals struct {
	Facts []proposal `json:"facts"`
}

// toolSchema declares the single tool the model may call.
func toolSchema() map[string]any {
	return map[string]any{
		"facts": map[string]any{
			"type":        "array",
			"description": "Facts worth remembering. Usually empty or very short.",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"label", "why_matters", "domain", "kind", "evidence"},
				"properties": map[string]any{
					"label": map[string]any{
						"type":        "string",
						"description": "One specific sentence naming the customer, place or supplier it concerns.",
					},
					"detail": map[string]any{
						"type":        "string",
						"description": "Optional fuller explanation. Omit unless there is genuinely more to say.",
					},
					"why_matters": map[string]any{
						"type":        "string",
						"description": "The concrete cost of not knowing this. Say what goes wrong, not that it is useful.",
					},
					"domain": map[string]any{
						"type": "string",
						"enum": []string{"scheduling", "customers", "properties", "suppliers", "money", "operations"},
					},
					"kind": map[string]any{
						"type": "string",
						"enum": []string{"standing", "reference", "finding", "issue", "decision", "goal"},
					},
					"evidence": map[string]any{
						"type":        "string",
						"description": "What in the given data supports this — how many occurrences, over what span, which dates.",
					},
				},
			},
		},
	}
}

// Run reads a window of the ledger and files what it finds worth remembering.
func Run(ctx context.Context, client *llm.Client, stores Stores, loc *time.Location, now time.Time) (Result, error) {
	from := now.Add(-Window)

	appts, err := stores.Ledger.Between(from, now.Add(24*time.Hour))
	if err != nil {
		return Result{}, err
	}
	if len(appts) == 0 {
		return Result{}, nil
	}
	if len(appts) > maxAppointments {
		appts = appts[len(appts)-maxAppointments:]
	}

	known, err := stores.Memory.All("", 200)
	if err != nil {
		return Result{}, err
	}

	evidence := buildEvidence(appts, known, loc, from, now)

	raw, usage, err := client.CallTool(ctx, llm.ToolRequest{
		System:          systemPrompt,
		Evidence:        evidence,
		ToolName:        "record_facts",
		ToolDescription: "File the things worth remembering from this batch. Call once. An empty list is normal.",
		Schema:          toolSchema(),
		Required:        []string{"facts"},
		MaxTokens:       4000,
	})
	res := Result{Appointments: len(appts), Usage: usage}
	if err != nil {
		return res, err
	}
	if len(raw) == 0 {
		// The model chose not to call the tool: nothing worth recording.
		return res, nil
	}

	var parsed proposals
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return res, fmt.Errorf("extract: decode proposals: %w", err)
	}
	res.Proposed = len(parsed.Facts)

	if len(parsed.Facts) > maxFactsPerRun {
		log.Printf("[extract] %d facts proposed in one run — keeping the first %d",
			len(parsed.Facts), maxFactsPerRun)
		parsed.Facts = parsed.Facts[:maxFactsPerRun]
	}

	for _, p := range parsed.Facts {
		fact, ok := validate(p)
		if !ok {
			res.Rejected++
			continue
		}

		dupes, err := stores.Memory.SimilarTo(fact.Label, fact.Domain)
		if err != nil {
			return res, err
		}
		if len(dupes) > 0 {
			res.Duplicates++
			continue
		}

		if _, err := stores.Memory.Record(fact); err != nil {
			// One bad proposal must not abandon the rest of the batch.
			log.Printf("[extract] rejected %q: %v", p.Label, err)
			res.Rejected++
			continue
		}
		res.Recorded++
	}

	return res, nil
}

// Stores is the pair of per-tenant stores this package needs.
type Stores struct {
	Ledger *ledger.Store
	Memory *memory.Store
}

// validate turns a proposal into a Fact, dropping anything malformed. The
// model is strongly schema-constrained, but a fact that reaches the graph is
// expensive to remove later, so this checks rather than trusts.
func validate(p proposal) (memory.Fact, bool) {
	label := strings.TrimSpace(p.Label)
	why := strings.TrimSpace(p.WhyMatters)
	evidence := strings.TrimSpace(p.Evidence)

	if label == "" || why == "" {
		return memory.Fact{}, false
	}
	// An unevidenced fact is exactly the kind of plausible invention this
	// whole design exists to keep out.
	if evidence == "" {
		return memory.Fact{}, false
	}
	if !validDomains[p.Domain] {
		return memory.Fact{}, false
	}
	kind, ok := validKinds[p.Kind]
	if !ok {
		return memory.Fact{}, false
	}

	detail := strings.TrimSpace(p.Detail)
	if detail != "" {
		detail += "\n\n"
	}
	detail += "Noticed from: " + evidence

	return memory.Fact{
		Label:       label,
		Detail:      detail,
		WhyMatters:  why,
		Domain:      p.Domain,
		Kind:        kind,
		Unconfirmed: true,
	}, true
}

// buildEvidence renders the ledger window and what is already known.
//
// Weekday is spelled out because day-of-week patterns are among the most
// useful things a calendar can reveal, and a bare date hides them.
func buildEvidence(appts []ledger.Appointment, known []memory.Recalled, loc *time.Location, from, to time.Time) string {
	var b strings.Builder

	fmt.Fprintf(&b, "APPOINTMENTS (%s to %s, %d shown, times local)\n\n",
		from.In(loc).Format("2006-01-02"), to.In(loc).Format("2006-01-02"), len(appts))

	for _, a := range appts {
		start := a.StartsAt.In(loc)
		fmt.Fprintf(&b, "%s %s  %s", start.Format("Mon 2006-01-02"), start.Format("15:04"), a.Title)
		if a.Location.Valid && a.Location.String != "" {
			fmt.Fprintf(&b, "  @ %s", a.Location.String)
		}
		b.WriteString("\n")
	}

	if len(known) == 0 {
		b.WriteString("\nALREADY KNOWN\n\n(nothing yet)\n")
		return b.String()
	}

	b.WriteString("\nALREADY KNOWN — do not restate any of these, in any wording\n\n")
	for _, k := range known {
		fmt.Fprintf(&b, "- [%s] %s\n", k.Domain, k.Label)
	}
	return b.String()
}
