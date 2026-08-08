package filter

import (
	"strings"
	"testing"
)

func TestSpam(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty is not spam", "", false},
		{"ordinary message", "hey has anyone seen the bird today", false},
		{"punctuation is fine", "well, that happened! ok?", false},
		{"emoji are fine", "nice \U0001F426\U0001F602", false},
		{"digits are fine", "top 10 birds of 2026", false},
		{"a url is fine", "look at https://example.com/thing", false},

		{"over the length cap", strings.Repeat("a b ", 700), true},
		{"exactly at the length cap is fine", strings.Repeat("ab", 1000), false},

		{"repeated character wall", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},
		{"twenty repeats is allowed", strings.Repeat("a", 20), false},
		{"twenty one repeats is not", strings.Repeat("a", 22), true},
		{
			// Spaces are exempt on purpose: runs of them occur in pasted text and
			// are not what this check is for.
			"a run of spaces is not a wall",
			"hello" + strings.Repeat(" ", 40) + "world",
			false,
		},

		{"kaomoji wall", strings.Repeat("╯°■°）╯", 6), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := Spam(c.in)
			if got != c.want {
				t.Errorf("Spam(...) = %v (%q), want %v", got, reason, c.want)
			}
			// A verdict of spam must always come with a reason, because the caller
			// logs it and "blocked, no idea why" is not a usable log line.
			if got && reason == "" {
				t.Error("Spam returned true with no reason")
			}
			if !got && reason != "" {
				t.Errorf("Spam returned false but gave a reason %q", reason)
			}
		})
	}
}

// TestSpamRejectsNonLatinScripts pins a real policy decision rather than a bug.
// AllowedRune permits only Latin, so a message written predominantly in another
// script fails the 0.80 ratio and is dropped. That is a deliberate trade in a
// server that speaks English and receives symbol-wall spam, and it would be the
// wrong trade in a multilingual server. It is recorded in SPEC.md section 4.3 so
// it can be argued with rather than discovered; this test is here so nobody
// "fixes" it by accident, and so that changing it is a visible decision.
func TestSpamRejectsNonLatinScripts(t *testing.T) {
	cases := map[string]string{
		"cyrillic": "привет как дела",
		"greek":    "γεια σου τι κανεις",
		"japanese": "こんにちは世界です",
		"arabic":   "مرحبا كيف حالك",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, _ := Spam(in)
			if !got {
				t.Errorf("expected %s to be dropped by the Latin-only policy; if this "+
					"changed deliberately, update SPEC.md section 4.3 as well", name)
			}
		})
	}
}

func TestIllegal(t *testing.T) {
	blocked, category := Illegal("i will kill someone tomorrow")
	if !blocked {
		t.Error("the placeholder violent-threat pattern did not match")
	}
	if category == "" {
		t.Error("a block must name its category")
	}

	if blocked, _ := Illegal("hey has anyone seen the bird today"); blocked {
		t.Error("an ordinary message was blocked")
	}
}

// TestIllegalIsAPlaceholder documents the gap rather than pretending it is closed.
// The real patterns are operator data loaded from PEREGRINE_BLOCKLIST_PATH in M5,
// because committing an explicit list of threat and CSAM-adjacent terms would make
// this repository a searchable copy of one. Until then this blocks essentially
// nothing (SPEC.md section 4, A4), and a test asserting otherwise would be a lie
// that outlived the code.
func TestIllegalIsAPlaceholder(t *testing.T) {
	// Things a real list would catch and this one does not. If M5 lands and these
	// start being blocked here, this test should be deleted, not adjusted.
	for _, s := range []string{
		"i am going to hurt you",
		"where do i buy a gun with no paperwork",
	} {
		if blocked, _ := Illegal(s); blocked {
			t.Errorf("Illegal(%q) blocked: the placeholder list has grown, so update "+
				"this test and SPEC.md A4", s)
		}
	}
}

func TestContainsSlurAndReplace(t *testing.T) {
	// A pattern from the list, in its plain form. Kept minimal on purpose.
	const withSlur = "you absolute wop"

	if !ContainsSlur(withSlur) {
		t.Fatal("ContainsSlur missed a listed pattern")
	}
	replaced := ReplaceSlurs(withSlur)
	if replaced == withSlur {
		t.Error("ReplaceSlurs left the input unchanged")
	}
	if ContainsSlur(replaced) {
		t.Error("the replacement still matches a rule, so replacement is not idempotent")
	}

	const clean = "you absolute legend"
	if ContainsSlur(clean) {
		t.Error("ContainsSlur matched clean text")
	}
	if ReplaceSlurs(clean) != clean {
		t.Error("ReplaceSlurs altered clean text")
	}
}

// TestContainsSlurMatchesReplaceSlurs pins the two entry points against each
// other. ContainsSlur used to be implemented as ReplaceSlurs(s) != s, which
// allocated a whole rewritten string to answer a yes/no question and would have
// silently started returning false for any rule whose replacement equalled its own
// match. They are now independent implementations, so they need to be checked for
// agreement.
func TestContainsSlurMatchesReplaceSlurs(t *testing.T) {
	covered := 0
	for _, r := range slurRules {
		// Derive a matching sample from the pattern itself, so this covers every
		// rule in the list without naming any of them here.
		sample := sampleFor(r.re.String())
		if sample == "" {
			continue
		}
		covered++
		if !ContainsSlur(sample) {
			t.Errorf("ContainsSlur missed its own rule %q via sample %q", r.re, sample)
		}
		if ReplaceSlurs(sample) == sample {
			t.Errorf("ReplaceSlurs did not change %q, but ContainsSlur reports a match", sample)
		}
	}

	// sampleFor bails out on patterns it cannot handle, so without this the test
	// would pass just as happily having exercised nothing at all.
	if covered < len(slurRules)-2 {
		t.Errorf("only %d of %d rules produced a usable sample; sampleFor has stopped "+
			"handling the pattern shapes in this list, so this test is no longer "+
			"checking what it claims", covered, len(slurRules))
	}
}

// sampleFor turns a simple \b-anchored pattern into one string that matches it,
// resolving character classes and alternations to their first option. Returns ""
// for anything it cannot handle, so it can only produce false negatives, never
// false confidence.
func sampleFor(pattern string) string {
	s := strings.TrimPrefix(pattern, "(?i)")
	s = strings.TrimPrefix(s, `\b`)
	s = strings.TrimSuffix(s, `\b`)

	var out strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return ""
			}
			class := s[i+1 : i+end]
			if class == "" {
				return ""
			}
			out.WriteByte(class[0])
			i += end
		case '(':
			end := strings.IndexByte(s[i:], ')')
			if end < 0 {
				return ""
			}
			group := s[i+1 : i+end]
			if alt := strings.SplitN(group, "|", 2); len(alt) > 0 {
				out.WriteString(alt[0])
			}
			i += end
		case '{':
			// A repetition like {2} applies to the previous element; expanding it
			// properly is more machinery than this helper deserves.
			return ""
		case '?', '*', '+':
			// Optional or repeated: the minimum match is fine, so skip.
		case '\\':
			return ""
		default:
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

// TestSlurRulesAreEvadable is a test that ASSERTS A WEAKNESS, deliberately.
//
// Every pattern here matches raw text with \b anchors and a hand-enumerated set of
// leet classes, so intra-word spacing, zero-width characters, combining marks and
// Cyrillic homoglyphs all walk straight through (SPEC.md section 4, A5). This
// package cannot fix that, and it should not try: enumerating evasions by hand is a
// game the defender loses. M5's normalizer folds the input first and matches
// against the normalized form.
//
// Asserting the weakness rather than leaving it undocumented does two things. It
// stops anyone concluding from a green suite that raw matching is sufficient, and
// when M5 lands these cases move to internal/safety and are expected to be CAUGHT.
// A failure here after M5 means the normalizer is working and this test should be
// deleted.
func TestSlurRulesAreEvadable(t *testing.T) {
	// The evasion characters are built from explicit code points rather than
	// written as literals. Every one of them is either invisible or
	// indistinguishable from the ASCII it imitates, so a literal here would be a
	// test whose input nobody can read and whose failure nobody could diagnose.
	// (staticcheck flags the literal form for the same reason.)
	var (
		zwj      = string(rune(0x200D)) // zero-width joiner
		combAcut = string(rune(0x0301)) // combining acute accent
		cyrO     = string(rune(0x043E)) // Cyrillic small o, identical to ASCII o
	)
	evasions := map[string]string{
		"intra-word space":   "you absolute w o p",
		"zero-width joiner":  "you absolute w" + zwj + "op",
		"combining mark":     "you absolute wo" + combAcut + "p",
		"cyrillic homoglyph": "you absolute w" + cyrO + "p",
		"unenumerated leet":  "you absolute w0p",
	}
	for name, s := range evasions {
		t.Run(name, func(t *testing.T) {
			if ContainsSlur(s) {
				t.Errorf("this evasion is now caught by raw matching, which is a "+
					"surprise worth understanding: %q", s)
			}
		})
	}
}

func TestAllowedRuneAndEmoji(t *testing.T) {
	for _, r := range []rune{'a', 'Z', '9', ' ', '.', '?', '\U0001F426'} {
		if !AllowedRune(r) {
			t.Errorf("AllowedRune(%q) = false, want true", r)
		}
	}
	// Non-Latin scripts are excluded by policy; see TestSpamRejectsNonLatinScripts.
	for _, r := range []rune{'п', 'こ', 'م'} {
		if AllowedRune(r) {
			t.Errorf("AllowedRune(%q) = true, want false under the Latin-only policy", r)
		}
	}
	if !Emoji('\U0001F600') || Emoji('a') {
		t.Error("Emoji misclassified a basic case")
	}
}
