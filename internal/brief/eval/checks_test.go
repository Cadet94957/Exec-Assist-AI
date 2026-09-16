package eval_test

import (
	"testing"

	"github.com/Cadet94957/Exec-Assist-AI/internal/brief/eval"
)

// The checks are the measurement, so they get tested too. A check that cannot
// fail is worse than no check: it reports success forever.

const sample = `STOPS (2, in order)

1. 08:00  Harborview — storefront reglaze  @ 88 Harbor Way
   runs until 10:00
   - [confirmed] Harborview gate code is 4417

2. 13:00  Ferguson residence — shower door  @ 41 Cedar Ln
   runs until 14:00
   3h clear beforehand

MONEY OUTSTANDING

- Marriott 4th St $1250.00, overdue, 9 days overdue
`

func TestNoInventedNumbersCatchesFabrication(t *testing.T) {
	ok := eval.NoInventedNumbers(sample, "8:00 Harborview, gate code 4417. 13:00 Ferguson.")
	if !ok.Passed {
		t.Errorf("a faithful brief was flagged: %s", ok.Detail)
	}

	// A plausible-looking invention: a gate code nobody supplied.
	bad := eval.NoInventedNumbers(sample, "8:00 Harborview, gate code 9182.")
	if bad.Passed {
		t.Error("an invented gate code was not caught")
	}

	// Counting what it was given is fine.
	counting := eval.NoInventedNumbers(sample, "2 stops today.")
	if !counting.Passed {
		t.Errorf("counting the stops was flagged as invention: %s", counting.Detail)
	}

	// Separators and leading zeros must not create false alarms.
	formatted := eval.NoInventedNumbers(sample, "Marriott owes $1,250.00 and you start at 08:00.")
	if !formatted.Passed {
		t.Errorf("reformatted numbers were flagged: %s", formatted.Detail)
	}
}

func TestNoInventedNamesCatchesFabrication(t *testing.T) {
	ok := eval.NoInventedNames(sample, "First up is Harborview, then Ferguson in the afternoon.")
	if !ok.Passed {
		t.Errorf("a faithful brief was flagged: %s", ok.Detail)
	}

	bad := eval.NoInventedNames(sample, "First up is Harborview, then a call with Kensington Glass.")
	if bad.Passed {
		t.Error("an invented customer was not caught")
	}

	// Ordinary capitalised words must not trip it.
	benign := eval.NoInventedNames(sample, "Nothing else today. Attention: Marriott is overdue. Bring suction cups.")
	if !benign.Passed {
		t.Errorf("ordinary words were treated as invented names: %s", benign.Detail)
	}
}

func TestHedgedCatchesAFlatAssertion(t *testing.T) {
	if r := eval.Hedged("You usually allow about 45 minutes for parking there."); !r.Passed {
		t.Error("a hedged sentence was not recognised")
	}
	if r := eval.Hedged("You need 45 minutes for parking there."); r.Passed {
		t.Error("an inference stated as fact was not caught — this is the §6 rule")
	}
}

func TestBrevityAndHeadlineChecks(t *testing.T) {
	if r := eval.ShorterThan("Nothing on today.", 400); !r.Passed {
		t.Error("a short brief was flagged as long")
	}
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	if r := eval.ShorterThan(string(long), 400); r.Passed {
		t.Error("a padded brief for a quiet day was not caught")
	}

	if r := eval.HeadlineFits("3 stops today.", 120); !r.Passed {
		t.Error("a short headline was rejected")
	}
	if r := eval.HeadlineFits("", 120); r.Passed {
		t.Error("an empty headline passed")
	}
	if r := eval.HeadlineFits(string(long), 120); r.Passed {
		t.Error("an overlong headline passed")
	}
}

func TestMentions(t *testing.T) {
	if r := eval.Mentions("x", "Harborview at 8, Ferguson at 1.", "Harborview", "Ferguson"); !r.Passed {
		t.Error("present terms were reported missing")
	}
	r := eval.Mentions("x", "Harborview at 8.", "Harborview", "Ferguson")
	if r.Passed {
		t.Error("a missing stop was not reported")
	}
	if r.Detail == "" {
		t.Error("the failure did not say what was missing")
	}
}

// Every fixture must be well-formed, so a broken case fails here rather than
// after spending money on an API call.
func TestFixturesAreUsable(t *testing.T) {
	cases := eval.Cases()
	if len(cases) < 5 {
		t.Fatalf("only %d eval cases", len(cases))
	}
	for _, c := range cases {
		if c.Name == "" || c.Check == nil {
			t.Errorf("malformed case: %+v", c.Name)
		}
		text := eval.InputText(c.Input)
		if text == "" {
			t.Errorf("%s: renders an empty prompt", c.Name)
		}
		if c.Name != "quiet day" && len(c.Input.Schedule.Slots) == 0 &&
			len(c.Input.Money) == 0 && len(c.Input.Changes) == 0 {
			t.Errorf("%s: nothing for the model to talk about", c.Name)
		}
	}
}
