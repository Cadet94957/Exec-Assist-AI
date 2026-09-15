package schedule_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/memory"
	"github.com/Cadet94957/Exec-Assist-AI/internal/schedule"
)

var day = time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)

func at(h, m int) time.Time {
	return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
}

func appt(id, title, loc string, start time.Time, mins int) ledger.Appointment {
	a := ledger.Appointment{ID: id, Title: title, StartsAt: start, Source: "google"}
	if loc != "" {
		a.Location = sql.NullString{String: loc, Valid: true}
	}
	if mins > 0 {
		a.EndsAt = sql.NullTime{Time: start.Add(time.Duration(mins) * time.Minute), Valid: true}
	}
	return a
}

func fact(label, why string, kind memory.Kind, status memory.Status) memory.Recalled {
	return memory.Recalled{Label: label, WhyMatters: why, Kind: kind, Status: status}
}

// The day must come back in the order it will actually be worked, whatever
// order the calendar handed it over in.
func TestBuildOrdersTheDayAndMeasuresGaps(t *testing.T) {
	appts := []ledger.Appointment{
		appt("c", "Ferguson residence", "41 Cedar Ln", at(14, 0), 60),
		appt("a", "Harborview reglaze", "88 Harbor Way", at(8, 0), 120),
		appt("b", "Marriott 4th St measure", "1200 4th St", at(11, 0), 60),
	}

	d := schedule.Build(appts, nil, time.UTC, day)
	if len(d.Slots) != 3 {
		t.Fatalf("got %d slots, want 3", len(d.Slots))
	}

	var order []string
	for _, s := range d.Slots {
		order = append(order, s.ID)
	}
	if order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Errorf("order = %v, want [a b c]", order)
	}

	// 8:00–10:00 then 11:00 leaves an hour clear.
	if got := d.Slots[1].GapBefore; got != time.Hour {
		t.Errorf("gap before the second stop = %v, want 1h", got)
	}
	// The first stop has nothing before it.
	if d.Slots[0].GapBefore != 0 {
		t.Errorf("first stop reported a gap of %v", d.Slots[0].GapBefore)
	}
}

// An appointment with no end time still occupies the day.
func TestMissingEndTimeStillOccupiesTime(t *testing.T) {
	d := schedule.Build([]ledger.Appointment{
		appt("a", "Harborview reglaze", "", at(8, 0), 0),
		appt("b", "Marriott measure", "", at(9, 30), 60),
	}, nil, time.UTC, day)

	// Assuming an hour means 8:00–9:00, so 9:30 leaves 30 minutes — not 90.
	if got := d.Slots[1].GapBefore; got != 30*time.Minute {
		t.Errorf("gap = %v, want 30m — an open-ended job was treated as taking no time", got)
	}
}

// Two stops in the same place at the same time is a problem no amount of
// driving solves, and it must be visible without asking a model.
func TestOverlapIsDetected(t *testing.T) {
	d := schedule.Build([]ledger.Appointment{
		appt("a", "Harborview reglaze", "", at(8, 0), 120),
		appt("b", "Marriott measure", "", at(9, 0), 60),
	}, nil, time.UTC, day)

	second := d.Slots[1]
	if !second.Overlaps() {
		t.Error("an overlapping booking was not flagged")
	}
	if !second.Tight() {
		t.Error("an overlap should count as a concern")
	}
	if len(d.Concerns()) != 1 {
		t.Errorf("got %d concerns, want 1", len(d.Concerns()))
	}
}

// A quiet day must produce no concerns at all — the assistant is silent when
// things are normal (§13).
func TestNormalDayRaisesNothing(t *testing.T) {
	d := schedule.Build([]ledger.Appointment{
		appt("a", "Harborview reglaze", "", at(8, 0), 60),
		appt("b", "Marriott measure", "", at(13, 0), 60),
	}, nil, time.UTC, day)

	if len(d.Concerns()) != 0 {
		t.Errorf("a comfortable day produced %d concerns", len(d.Concerns()))
	}
}

// A rule naming a place attaches to that stop and to no other.
func TestNotesAttachToTheRightStop(t *testing.T) {
	facts := []memory.Recalled{
		fact("Harborview gate code is 4417", "Without it the crew waits at the gate.",
			memory.Reference, memory.Confirmed),
		fact("Marriott 4th St always specifies tempered", "Quoting annealed means requoting.",
			memory.Standing, memory.Confirmed),
	}

	d := schedule.Build([]ledger.Appointment{
		appt("a", "Harborview reglaze", "88 Harbor Way", at(8, 0), 60),
		appt("b", "Marriott 4th St measure", "1200 4th St", at(11, 0), 60),
	}, facts, time.UTC, day)

	if len(d.Slots[0].Notes) != 1 || d.Slots[0].Notes[0].Text != facts[0].Label {
		t.Errorf("Harborview notes = %+v", d.Slots[0].Notes)
	}
	if len(d.Slots[1].Notes) != 1 || d.Slots[1].Notes[0].Text != facts[1].Label {
		t.Errorf("Marriott notes = %+v", d.Slots[1].Notes)
	}
}

// Attaching a rule to the wrong stop puts a confident, wrong sentence in a
// brief, which costs more trust than leaving it out.
func TestCommonWordsDoNotMatchEverything(t *testing.T) {
	facts := []memory.Recalled{
		fact("Never book jobs after 3pm on Tuesdays", "Kid pickup.", memory.Standing, memory.Confirmed),
	}
	d := schedule.Build([]ledger.Appointment{
		appt("a", "Harborview reglaze", "88 Harbor Way", at(8, 0), 60),
	}, facts, time.UTC, day)

	if len(d.Slots[0].Notes) != 0 {
		t.Errorf("a rule about booking hours attached itself to an unrelated job: %+v", d.Slots[0].Notes)
	}
}

// A rule that states a number becomes arithmetic; the rest stay notes.
func TestBuffersAreExtractedFromExplicitDurations(t *testing.T) {
	facts := []memory.Recalled{
		fact("Harborview needs an extra 45 minutes for parking",
			"Back-to-back stops there run late without it.", memory.Finding, memory.Inferred),
		fact("Harborview prefers an early start", "They open the dock first thing.",
			memory.Standing, memory.Confirmed),
	}

	d := schedule.Build([]ledger.Appointment{
		appt("a", "Harborview reglaze", "88 Harbor Way", at(8, 0), 60),
	}, facts, time.UTC, day)

	s := d.Slots[0]
	if len(s.Notes) != 2 {
		t.Fatalf("got %d notes, want 2", len(s.Notes))
	}
	if len(s.Buffers) != 1 {
		t.Fatalf("got %d buffers, want 1 — only the rule stating a number is arithmetic", len(s.Buffers))
	}
	if s.BufferTotal() != 45*time.Minute {
		t.Errorf("buffer total = %v, want 45m", s.BufferTotal())
	}
	// The buffer must remember it was an inference, so the brief hedges it.
	if s.Buffers[0].Status != memory.Inferred {
		t.Errorf("buffer status = %q, want inferred", s.Buffers[0].Status)
	}
}

// A gap smaller than the rules ask for is the signal Phase 3 turns into a
// verdict.
func TestTightWhenGapIsSmallerThanTheBuffer(t *testing.T) {
	facts := []memory.Recalled{
		fact("Marriott 4th St needs an extra 45 minutes for downtown parking",
			"Parking is what makes back-to-back downtown jobs run late.",
			memory.Finding, memory.Confirmed),
	}

	d := schedule.Build([]ledger.Appointment{
		appt("a", "Harborview reglaze", "", at(8, 0), 120),
		appt("b", "Marriott 4th St measure", "1200 4th St", at(10, 20), 60),
	}, facts, time.UTC, day)

	second := d.Slots[1]
	if second.GapBefore != 20*time.Minute {
		t.Fatalf("gap = %v, want 20m", second.GapBefore)
	}
	if !second.Tight() {
		t.Error("a 20-minute gap against a 45-minute buffer was not flagged as tight")
	}
	if second.Overlaps() {
		t.Error("a tight gap is not an overlap")
	}
}

// A nonsense duration must not become a buffer.
func TestImplausibleDurationsAreIgnored(t *testing.T) {
	facts := []memory.Recalled{
		fact("Harborview job took 9000 minutes once", "An outlier.", memory.Finding, memory.Inferred),
	}
	d := schedule.Build([]ledger.Appointment{
		appt("a", "Harborview reglaze", "", at(8, 0), 60),
	}, facts, time.UTC, day)

	if len(d.Slots[0].Buffers) != 0 {
		t.Errorf("an implausible duration became a buffer: %+v", d.Slots[0].Buffers)
	}
}

func TestStandingRulesApplyToTheWholeDay(t *testing.T) {
	facts := []memory.Recalled{
		fact("Never book after 3pm on Tuesdays", "Kid pickup.", memory.Standing, memory.Confirmed),
		fact("Harborview gate code is 4417", "Crew waits otherwise.", memory.Reference, memory.Confirmed),
		fact("Stop taking residential shower doors", "Margin is poor.", memory.Goal, memory.Confirmed),
	}

	standing := schedule.Standing(facts)
	if len(standing) != 2 {
		t.Fatalf("got %d day-wide rules, want 2 (the standing rule and the goal)", len(standing))
	}
}

func TestEmptyDayIsNotAnError(t *testing.T) {
	d := schedule.Build(nil, nil, nil, day)
	if len(d.Slots) != 0 {
		t.Errorf("got %d slots for an empty day", len(d.Slots))
	}
	if len(d.Concerns()) != 0 {
		t.Error("an empty day produced concerns")
	}
	if d.Loc != time.UTC {
		t.Error("a nil location should fall back to UTC rather than panic")
	}
}
