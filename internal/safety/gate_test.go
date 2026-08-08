package safety

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testGate(t *testing.T, list string) *Gate {
	t.Helper()
	bl, err := parseBlocklist(strings.NewReader(list), "test")
	if err != nil {
		t.Fatalf("parseBlocklist: %v", err)
	}
	return NewGate(bl, quietLogger(), false)
}

func TestCheckLearnAllowsOrdinaryText(t *testing.T) {
	g := testGate(t, "slur \\bexampleslur\\b\n")

	for _, s := range []string{
		"hey has anyone seen the bird today",
		"that is genuinely funny",
		"look at https://example.com/thing",
		"cope seethe mald",
	} {
		if v := g.CheckLearn(s); !v.Allowed {
			t.Errorf("CheckLearn(%q) rejected: %s", s, v.Reason)
		}
	}
}

// TestCheckLearnRejectsEvadedForms is the payoff for the normalizer. Each of these
// gets through internal/filter's raw matching, which that package's tests assert,
// and each must be caught here.
func TestCheckLearnRejectsEvadedForms(t *testing.T) {
	g := testGate(t, "slur \\bexampleslur\\b\n")

	cases := map[string]string{
		"plain":             "you are an exampleslur",
		"uppercase":         "you are an EXAMPLESLUR",
		"leet":              "you are an 3xampl3slur",
		"spaced letters":    "you are an e x a m p l e s l u r",
		"dotted letters":    "you are an e.x.a.m.p.l.e.s.l.u.r",
		"zero-width joiner": "you are an example" + string(rune(0x200D)) + "slur",
		"combining mark":    "you are an exampl" + "e" + string(rune(0x0301)) + "slur",
		"cyrillic e":        "you are an exampl" + string(rune(0x0435)) + "slur",
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			v := g.CheckLearn(s)
			if v.Allowed {
				t.Errorf("CheckLearn allowed %q (normalizes to %q)", s, Normalize(s))
			}
			if v.Reason == "" {
				t.Error("a rejection must carry a reason for the log")
			}
		})
	}
}

// TestCheckLearnRejectsWholeMessage is the reject-never-launder rule. The old
// filter replaced matches, so the message was still learned with its structure
// intact and the replacement token sitting in the slur's grammatical position: the
// bot had been taught the sentence and merely said something harmless where the
// slur went.
//
// The Verdict type has no field for rewritten text, which is what makes laundering
// unexpressible on this path rather than merely discouraged. This test states the
// intent so nobody adds one.
func TestCheckLearnRejectsWholeMessage(t *testing.T) {
	g := testGate(t, "slur \\bexampleslur\\b\n")

	v := g.CheckLearn("i think that exampleslur is why the bird left")
	if v.Allowed {
		t.Fatal("expected a rejection")
	}
	// A Verdict carries a verdict and a reason, never a cleaned string. If someone
	// adds one, this comment and the A5 finding are the argument against it.
	if v.Category != CategorySlur {
		t.Errorf("Category = %q, want %q", v.Category, CategorySlur)
	}
}

func TestCheckLearnRejectsSpamShape(t *testing.T) {
	g := testGate(t, "slur \\bexampleslur\\b\n")

	v := g.CheckLearn(strings.Repeat("a", 500))
	if v.Allowed {
		t.Error("a repeated-character wall must be rejected")
	}
	if !strings.Contains(v.Reason, "spam shape") {
		t.Errorf("Reason = %q, want it to name the spam shape", v.Reason)
	}
}

// TestCheckLearnAppliesSpamCategory covers the asymmetry between the two
// directions. Invite spam is worth refusing to learn and not worth suppressing
// output over.
func TestCheckLearnAppliesSpamCategory(t *testing.T) {
	g := testGate(t, "spam \\bfree nitro\\b\n")

	if v := g.CheckLearn("get free nitro here"); v.Allowed {
		t.Error("the spam category must apply on the learning path")
	}
	if v := g.CheckEmit("get free nitro here"); !v.Allowed {
		t.Error("the spam category must NOT block emission: generating something that " +
			"resembles advertising is not an incident")
	}
}

func TestCheckEmitBlocksHarmfulCategories(t *testing.T) {
	g := testGate(t, "slur \\bexampleslur\\b\nillegal \\bkill you\\b\n")

	for name, s := range map[string]string{
		"slur":    "the exampleslur was here",
		"illegal": "i will kill you tomorrow",
	} {
		t.Run(name, func(t *testing.T) {
			if v := g.CheckEmit(s); v.Allowed {
				t.Errorf("CheckEmit allowed %q", s)
			}
		})
	}
}

// TestIllegalCategoryAlerts pins the one behavioural difference between the two
// harmful categories. The illegal category exists for content where the operator's
// exposure is legal rather than reputational, so it pages a human.
func TestIllegalCategoryAlerts(t *testing.T) {
	g := testGate(t, "slur \\bexampleslur\\b\nillegal \\bkill you\\b\n")

	if v := g.CheckEmit("i will kill you"); v.Allowed || !v.Alert {
		t.Errorf("an illegal-category match must reject AND alert; got allowed=%v alert=%v", v.Allowed, v.Alert)
	}
	if v := g.CheckEmit("the exampleslur"); v.Allowed || v.Alert {
		t.Errorf("a slur match must reject WITHOUT alerting; got allowed=%v alert=%v", v.Allowed, v.Alert)
	}
}

// TestPauseAllWritesRefusesEveryEmit covers the incident lever. It must refuse
// everything outbound while leaving the learning path alone: during an incident the
// output is the problem, and stopping ingestion would also stop the bot noticing
// what is being said to it.
func TestPauseAllWritesRefusesEveryEmit(t *testing.T) {
	bl, err := parseBlocklist(strings.NewReader("slur \\bexampleslur\\b\n"), "test")
	if err != nil {
		t.Fatalf("parseBlocklist: %v", err)
	}
	g := NewGate(bl, quietLogger(), true)

	if !g.Paused() {
		t.Fatal("Paused() = false after constructing with pauseAllWrites")
	}
	v := g.CheckEmit("hey has anyone seen the bird today")
	if v.Allowed {
		t.Error("with writes paused, even innocuous output must be refused")
	}
	if !strings.Contains(v.Reason, "PAUSE_ALL_WRITES") {
		t.Errorf("Reason = %q, want it to name the variable so the log explains the silence", v.Reason)
	}

	if v := g.CheckLearn("hey has anyone seen the bird today"); !v.Allowed {
		t.Error("pausing writes must not stop learning")
	}

	g.SetPaused(false)
	if v := g.CheckEmit("hey has anyone seen the bird today"); !v.Allowed {
		t.Error("unpausing must restore emission")
	}
}

func TestGateCounters(t *testing.T) {
	g := testGate(t, "slur \\bexampleslur\\b\n")

	g.CheckLearn("an exampleslur")
	g.CheckLearn("an exampleslur")
	g.CheckLearn("perfectly fine")
	g.CheckEmit("an exampleslur")

	if got := g.LearnRejected(); got != 2 {
		t.Errorf("LearnRejected() = %d, want 2", got)
	}
	if got := g.EmitRejected(); got != 1 {
		t.Errorf("EmitRejected() = %d, want 1", got)
	}
}

// TestBuiltInBaselineAppliesWithAThinList pins that the in-source slur list still
// holds when the operator's blocklist has nothing relevant in it, which is the
// state every fresh deployment starts in.
func TestBuiltInBaselineAppliesWithAThinList(t *testing.T) {
	g := testGate(t, "spam \\bfree nitro\\b\n")

	// A term from internal/filter's built-in list, not from the loaded file.
	if v := g.CheckLearn("you absolute wop"); v.Allowed {
		t.Error("the built-in slur baseline must apply even when the operator list is thin")
	}
	if v := g.CheckEmit("you absolute wop"); v.Allowed {
		t.Error("the built-in slur baseline must apply on emit too")
	}
	// And it must catch the evaded form, which is the difference the normalizer
	// makes: internal/filter alone cannot do this.
	if v := g.CheckEmit("you absolute w o p"); v.Allowed {
		t.Error("the normalizer must let the built-in list catch spaced-out evasion")
	}
}

// TestGateWithNilBlocklistStillEnforcesBaseline covers the test-only construction
// path. It must never be more permissive than the baseline.
func TestGateWithNilBlocklistStillEnforcesBaseline(t *testing.T) {
	g := NewGate(nil, quietLogger(), false)

	if v := g.CheckLearn("you absolute wop"); v.Allowed {
		t.Error("with no blocklist the built-in baseline must still apply")
	}
	if v := g.CheckLearn("hey has anyone seen the bird"); !v.Allowed {
		t.Error("with no blocklist ordinary text must still pass")
	}
}
