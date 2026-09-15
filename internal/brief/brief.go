// Package brief builds the morning brief.
//
// It is deliberately in two halves. Assemble reads the ledger and the memory
// graph and produces a typed Input using nothing but Go — no model, no network,
// fully testable. Render hands that to Claude, which is responsible only for
// synthesis and wording (§11).
//
// The split matters because it decides what can go wrong. A model that writes
// badly produces an awkward brief. A model asked to work out whether a day is
// feasible, or which invoice is overdue, produces a confident wrong answer —
// and the whole product is a promise to catch exactly that kind of thing.
package brief

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/llm"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	"github.com/Cadet94957/Exec-Assist-AI/internal/schedule"
	"github.com/Cadet94957/Exec-Assist-AI/internal/tenant"
)

// Money is an outstanding amount worth mentioning.
type Money struct {
	Customer string
	Amount   string
	Status   string
	DueIn    string
}

// Input is everything the brief is allowed to talk about. If it is not here,
// it does not go in the brief.
type Input struct {
	Business string
	Trade    string
	Date     time.Time
	Loc      *time.Location

	Schedule schedule.Day
	Standing []schedule.Note
	Money    []Money
	// Changes are things that moved since yesterday, from the typed event log.
	Changes []string
}

// Empty reports whether there is nothing worth saying. A quiet day deserves a
// short brief, not an invented one (§13).
func (in Input) Empty() bool {
	return len(in.Schedule.Slots) == 0 && len(in.Money) == 0 && len(in.Changes) == 0
}

// Stores is what assembling a brief needs.
type Stores interface {
	Tenant() tenant.Tenant
	Ledger() *ledger.Store
	Memory() *memory.Store
}

// Assemble gathers the day deterministically.
func Assemble(stores Stores, now time.Time) (Input, error) {
	t := stores.Tenant()
	loc := t.Location()
	start := t.StartOfDay(now)
	end := start.AddDate(0, 0, 1)

	appts, err := stores.Ledger().Between(start, end)
	if err != nil {
		return Input{}, err
	}

	// Everything the business knows. The graph is small by design — it holds
	// rules, not events — so reading it whole is cheaper than being clever.
	facts, err := stores.Memory().All("", 200)
	if err != nil {
		return Input{}, err
	}

	in := Input{
		Business: t.Name,
		Trade:    t.Trade,
		Date:     start,
		Loc:      loc,
		Schedule: schedule.Build(appts, facts, loc, start),
		Standing: schedule.Standing(facts),
	}

	invoices, err := stores.Ledger().Outstanding(10)
	if err != nil {
		return Input{}, err
	}
	for _, inv := range invoices {
		m := Money{
			Amount: fmt.Sprintf("$%.2f", float64(inv.AmountCents)/100),
			Status: inv.Status,
		}
		if inv.Customer.Valid {
			m.Customer = inv.Customer.String
		}
		if inv.DueAt.Valid {
			m.DueIn = humanDue(inv.DueAt.Time, now)
		}
		in.Money = append(in.Money, m)
	}

	// What moved since this time yesterday. This is why the sync emits typed
	// events rather than overwriting state.
	evs, err := stores.Ledger().RecentEvents(now.AddDate(0, 0, -1), now, 20)
	if err != nil {
		return Input{}, err
	}
	for _, rec := range evs {
		if desc := describe(rec, loc); desc != "" {
			in.Changes = append(in.Changes, desc)
		}
	}

	return in, nil
}

// describe renders a stored event as a sentence, or returns empty for the ones
// not worth a contractor's attention.
func describe(rec ledger.EventRecord, loc *time.Location) string {
	var a struct {
		Title    string    `json:"title"`
		StartsAt time.Time `json:"starts_at"`
		Previous *struct {
			Title    string    `json:"title"`
			StartsAt time.Time `json:"starts_at"`
		} `json:"previous"`
	}
	if err := json.Unmarshal([]byte(rec.Payload), &a); err != nil {
		return ""
	}

	switch rec.Type {
	case "appointment.cancelled":
		return fmt.Sprintf("%s was cancelled", a.Title)
	case "appointment.changed":
		if a.Previous == nil || a.Previous.StartsAt.Equal(a.StartsAt) {
			// A retitle is not news.
			return ""
		}
		return fmt.Sprintf("%s moved from %s to %s", a.Title,
			a.Previous.StartsAt.In(loc).Format("Mon 3:04pm"), a.StartsAt.In(loc).Format("Mon 3:04pm"))
	case "appointment.created":
		return fmt.Sprintf("%s was added for %s", a.Title, a.StartsAt.In(loc).Format("Mon 3:04pm"))
	}
	return ""
}

func humanDue(due, now time.Time) string {
	days := int(due.Sub(now).Hours() / 24)
	switch {
	case days < -1:
		return fmt.Sprintf("%d days overdue", -days)
	case days < 0:
		return "overdue"
	case days == 0:
		return "due today"
	case days == 1:
		return "due tomorrow"
	default:
		return fmt.Sprintf("due in %d days", days)
	}
}

// Brief is the finished thing.
type Brief struct {
	// Headline is one line, short enough for a notification.
	Headline string
	// Body is the full brief.
	Body      string
	Generated time.Time
	Usage     llm.Usage
}

// Render turns an assembled day into words.
func Render(ctx context.Context, client *llm.Client, in Input) (Brief, error) {
	out := Brief{Generated: time.Now().UTC()}

	raw, usage, err := client.CallTool(ctx, llm.ToolRequest{
		System:          systemPrompt,
		Evidence:        renderInput(in),
		ToolName:        "write_brief",
		ToolDescription: "Write this morning's brief. Call once.",
		Schema: map[string]any{
			"headline": map[string]any{
				"type":        "string",
				"description": "One line, under 100 characters, readable on a lock screen. State the day's shape, or the single thing that needs attention.",
			},
			"body": map[string]any{
				"type":        "string",
				"description": "The brief itself. Plain text with line breaks. Short.",
			},
		},
		Required:  []string{"headline", "body"},
		MaxTokens: 2000,
	})
	out.Usage = usage
	if err != nil {
		return out, err
	}
	if len(raw) == 0 {
		return out, fmt.Errorf("brief: the model returned no brief")
	}

	var parsed struct {
		Headline string `json:"headline"`
		Body     string `json:"body"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return out, fmt.Errorf("brief: decode: %w", err)
	}
	out.Headline = strings.TrimSpace(parsed.Headline)
	out.Body = strings.TrimSpace(parsed.Body)
	if out.Body == "" {
		return out, fmt.Errorf("brief: the model returned an empty brief")
	}
	return out, nil
}
