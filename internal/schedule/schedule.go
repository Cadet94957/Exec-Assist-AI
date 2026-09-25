// Package schedule turns a day's appointments and the business's standing
// rules into an ordered, inspectable plan.
//
// Everything here is arithmetic on data. The spec is explicit (§14) that
// whether a day is physically possible is not a question to ask a language
// model: a model that is confidently wrong about whether you can be in two
// places at once produces exactly the failure the product exists to prevent.
// Phase 3 adds travel time and a feasible/warning/conflict verdict on top of
// this structure; this is the structure.
package schedule

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
)

// Note is something the business knows that bears on a particular stop.
type Note struct {
	Text string
	Why  string
	// Status decides how it may be stated. An inference must be hedged; §6
	// forbids presenting one as settled fact.
	Status memory.Status
}

// Trusted reports whether this can be stated plainly rather than hedged.
func (n Note) Trusted() bool { return n.Status.Trusted() }

// Buffer is extra time a standing rule says a stop needs.
type Buffer struct {
	Duration time.Duration
	Reason   string
	Status   memory.Status
}

// Slot is one stop in the day.
type Slot struct {
	ID       string
	JobID    string
	Title    string
	Location string
	Start    time.Time
	End      time.Time

	// GapBefore is the clear time since the previous stop ended. Negative
	// means the stops overlap, which is a problem no amount of driving solves.
	GapBefore time.Duration

	Notes   []Note
	Buffers []Buffer
}

// Overlaps reports whether this stop starts before the previous one ends.
func (s Slot) Overlaps() bool { return s.GapBefore < 0 }

// BufferTotal is how much extra time the known rules ask for.
func (s Slot) BufferTotal() time.Duration {
	var total time.Duration
	for _, b := range s.Buffers {
		if b.Status == memory.Confirmed {
			total += b.Duration
		}
	}
	return total
}

// Tight reports whether the gap before this stop is smaller than the buffers
// its own rules call for. This is a signal, not a verdict — without travel
// time it cannot know the day is impossible, only that it looks tight.
func (s Slot) Tight() bool {
	if s.GapBefore < 0 {
		return true
	}
	return s.BufferTotal() > 0 && s.GapBefore < s.BufferTotal()
}

// Day is one contractor's day, in order.
type Day struct {
	Date  time.Time
	Loc   *time.Location
	Slots []Slot
}

// Concerns returns the stops that look tight or overlap, for a brief that
// should be silent when everything is normal (§13).
func (d Day) Concerns() []Slot {
	var out []Slot
	for _, s := range d.Slots {
		if s.Tight() {
			out = append(out, s)
		}
	}
	return out
}

// Build orders the day and attaches what the business knows to each stop.
//
// It is a pure function: no database, no clock, no model. That is what makes
// the brief testable without spending anything.
func Build(appts []ledger.Appointment, facts []memory.Recalled, loc *time.Location, date time.Time) Day {
	if loc == nil {
		loc = time.UTC
	}

	slots := make([]Slot, 0, len(appts))
	for _, a := range appts {
		s := Slot{
			ID:       a.ID,
			JobID:    a.JobID.String,
			Title:    a.Title,
			Start:    a.StartsAt,
			Location: a.LocationText(),
		}
		s.End = a.Ends()
		slots = append(slots, s)
	}

	sort.SliceStable(slots, func(i, j int) bool { return slots[i].Start.Before(slots[j].Start) })

	for i := range slots {
		if i > 0 {
			slots[i].GapBefore = slots[i].Start.Sub(slots[i-1].End)
		}
		slots[i].Notes, slots[i].Buffers = match(slots[i], facts)
	}

	return Day{Date: date, Loc: loc, Slots: slots}
}

// bufferPattern finds an explicit duration in a rule: "45 minutes", "45 min",
// "an extra 30 mins". Deliberately narrow — a rule that does not say a number
// outright becomes a note rather than arithmetic, because guessing at how much
// time someone meant is how a schedule engine starts lying.
var bufferPattern = regexp.MustCompile(`(?i)(\d{1,3})\s*(?:minute|min)s?\b`)

// match attaches the facts that bear on a stop.
func match(s Slot, facts []memory.Recalled) ([]Note, []Buffer) {
	var (
		notes   []Note
		buffers []Buffer
	)
	haystack := strings.ToLower(s.Title + " " + s.Location)

	for _, f := range facts {
		if !relevant(f, haystack) {
			continue
		}
		note := Note{Text: f.Label, Why: f.WhyMatters, Status: f.Status}
		notes = append(notes, note)

		if m := bufferPattern.FindStringSubmatch(f.Label + " " + f.Detail); m != nil {
			if mins, err := strconv.Atoi(m[1]); err == nil && mins > 0 && mins <= 480 {
				buffers = append(buffers, Buffer{
					Duration: time.Duration(mins) * time.Minute,
					Reason:   f.Label,
					Status:   f.Status,
				})
			}
		}
	}
	return notes, buffers
}

// stopwords are words too common to prove a fact is about a given stop.
var stopwords = map[string]bool{
	"after": true, "before": true, "always": true, "never": true, "needs": true,
	"need": true, "jobs": true, "job": true, "book": true, "books": true,
	"booking": true, "bookings": true, "extra": true, "minutes": true, "minute": true,
	"customer": true, "customers": true, "there": true, "their": true, "under": true,
	"until": true, "about": true, "which": true, "every": true, "these": true,
	"those": true, "into": true, "from": true, "with": true, "that": true, "this": true,
	"have": true, "when": true, "will": true, "does": true, "than": true, "them": true,
	"then": true, "over": true, "onto": true, "each": true, "also": true, "were": true,
	"been": true, "being": true, "does not": true, "weekday": true, "weekdays": true,
}

var wordPattern = regexp.MustCompile(`[a-z0-9]{4,}`)

// relevant reports whether a fact is about this stop.
//
// The test is a distinctive shared word — "Harborview", "Marriott" — because a
// name is what actually ties a rule to a place. This is conservative on
// purpose: attaching a rule to the wrong stop puts a confident, wrong sentence
// in a brief, which costs more trust than omitting it.
//
// Known limit: a rule about "downtown jobs" attaches only to stops that say
// downtown. Recognising that an address *is* downtown needs location data the
// ledger does not have yet; that arrives with Phase 3's travel-time work.
func relevant(f memory.Recalled, haystack string) bool {
	for _, w := range wordPattern.FindAllString(strings.ToLower(f.Label), -1) {
		if stopwords[w] {
			continue
		}
		if strings.Contains(haystack, w) {
			return true
		}
	}
	return false
}

// Standing returns the rules that apply to the whole day rather than to any
// one stop — the things true before the first job starts.
func Standing(facts []memory.Recalled) []Note {
	var out []Note
	for _, f := range facts {
		if f.Kind == memory.Standing || f.Kind == memory.Goal {
			out = append(out, Note{Text: f.Label, Why: f.WhyMatters, Status: f.Status})
		}
	}
	return out
}
