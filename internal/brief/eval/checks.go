// Package eval measures the brief prompt against the rules that matter.
//
// The brief prompt is the product, and it is the part most likely to be tuned
// once pilot feedback arrives. Without measurement that tuning is guesswork
// that can silently break the two rules the whole thing rests on: never invent,
// and never state a guess as fact.
//
// The checks here are deliberately programmatic rather than judged by another
// model. "Did it invent a number" has an exact answer, and an exact answer is
// worth more than a plausible opinion — especially for a rule whose violation
// is the failure mode the product exists to prevent.
package eval

import (
	"fmt"
	"regexp"
	"strings"
)

// Result is one check's outcome.
type Result struct {
	Name   string
	Passed bool
	Detail string
}

// hedges are the ways English marks something as not certain. A brief carrying
// an inference must use one of them.
var hedges = []string{
	"usually", "typically", "generally", "often", "tend", "tends", "about",
	"around", "roughly", "looks like", "seems", "i think", "may ", "might ",
	"probably", "normally", "appears",
}

var (
	// Repeated separator groups, so "1,250.00" is one number rather than
	// "1,250" and "00" — which would make every reformatted amount look
	// invented.
	numberPattern = regexp.MustCompile(`\d+(?:[.:,]\d+)*`)
	// A capitalised word that is not at the start of a sentence is almost
	// always a name — the kind of thing that must not be invented.
	properNoun = regexp.MustCompile(`(?:[^.!?\n]\s+)([A-Z][a-zA-Z']{2,})`)
)

// allowedCapitals are words a brief may reasonably capitalise without them
// being a name drawn from the data.
var allowedCapitals = map[string]bool{
	"Monday": true, "Tuesday": true, "Wednesday": true, "Thursday": true,
	"Friday": true, "Saturday": true, "Sunday": true,
	"January": true, "February": true, "March": true, "April": true, "May": true,
	"June": true, "July": true, "August": true, "September": true,
	"October": true, "November": true, "December": true,
	"Attention": true, "Today": true, "Tomorrow": true, "Bring": true,
	"Morning": true, "Afternoon": true, "Evening": true, "Nothing": true,
	"You": true, "Your": true, "Note": true, "Heads": true, "One": true,
	"Two": true, "Three": true, "Four": true, "Five": true, "Six": true,
	"Also": true, "Then": true, "After": true, "Before": true, "Check": true,
	"Allow": true, "Gate": true, "Watch": true, "Gap": true, "Gaps": true,
}

// NoInventedNumbers is the strongest check here. Times, amounts and durations
// are exactly what a contractor acts on, and a number that appears in a brief
// but not in the data was made up.
func NoInventedNumbers(input, output string) Result {
	haystack := numberPattern.FindAllString(input, -1)
	known := make(map[string]bool, len(haystack))
	for _, n := range haystack {
		known[normaliseNumber(n)] = true
	}

	var invented []string
	for _, n := range numberPattern.FindAllString(output, -1) {
		norm := normaliseNumber(n)
		if known[norm] {
			continue
		}
		// A brief may legitimately count things it was given: "3 stops today"
		// when three stops are listed. Small integers are therefore allowed.
		if len(norm) <= 2 && !strings.ContainsAny(norm, ".:,") {
			continue
		}
		invented = append(invented, n)
	}

	if len(invented) > 0 {
		return Result{"no invented numbers", false,
			fmt.Sprintf("these appear in the brief but not in the data: %s", strings.Join(invented, ", "))}
	}
	return Result{"no invented numbers", true, ""}
}

// NoInventedNames catches a customer, site or supplier the data never mentioned.
func NoInventedNames(input, output string) Result {
	lowerInput := strings.ToLower(input)

	var invented []string
	for _, m := range properNoun.FindAllStringSubmatch(output, -1) {
		word := m[1]
		if allowedCapitals[word] {
			continue
		}
		if strings.Contains(lowerInput, strings.ToLower(word)) {
			continue
		}
		invented = append(invented, word)
	}

	if len(invented) > 0 {
		return Result{"no invented names", false,
			fmt.Sprintf("named without support from the data: %s", strings.Join(invented, ", "))}
	}
	return Result{"no invented names", true, ""}
}

// Hedged checks that an unconfirmed fact is not stated as settled. §6 forbids
// presenting an inference as fact, and this is the observable form of that.
func Hedged(output string) Result {
	lower := strings.ToLower(output)
	for _, h := range hedges {
		if strings.Contains(lower, h) {
			return Result{"hedges the inference", true, ""}
		}
	}
	return Result{"hedges the inference", false,
		"the brief carried an inferred fact but stated it flatly, with no hedge"}
}

// Mentions checks that something the contractor needs actually made it in.
func Mentions(name, output string, wanted ...string) Result {
	lower := strings.ToLower(output)
	var missing []string
	for _, w := range wanted {
		if !strings.Contains(lower, strings.ToLower(w)) {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		return Result{name, false, "not mentioned: " + strings.Join(missing, ", ")}
	}
	return Result{name, true, ""}
}

// ShorterThan keeps a quiet day quiet. §13 asks the assistant to be silent
// when things are normal, and a padded brief is the opposite of that.
func ShorterThan(output string, max int) Result {
	if len(output) > max {
		return Result{"stays short", false,
			fmt.Sprintf("%d characters for a day with nothing in it (limit %d)", len(output), max)}
	}
	return Result{"stays short", true, ""}
}

// HeadlineFits keeps the notification readable on a lock screen.
func HeadlineFits(headline string, max int) Result {
	if len(headline) > max {
		return Result{"headline fits a lock screen", false,
			fmt.Sprintf("%d characters (limit %d): %q", len(headline), max, headline)}
	}
	if strings.TrimSpace(headline) == "" {
		return Result{"headline fits a lock screen", false, "empty headline"}
	}
	return Result{"headline fits a lock screen", true, ""}
}

// normaliseNumber strips separators so "1,250" and "1250" compare equal, and
// "08:00" matches "8:00".
func normaliseNumber(s string) string {
	s = strings.ReplaceAll(s, ",", "")
	// "08:00" and "8:00" are the same time written two ways.
	for len(s) > 1 && s[0] == '0' && s[1] != '.' {
		s = s[1:]
	}
	return s
}

func lowerOf(s string) string     { return strings.ToLower(s) }
func contains(s, sub string) bool { return strings.Contains(s, sub) }
