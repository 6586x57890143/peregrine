package safety

import (
	"strings"
	"testing"
)

// These are the exact evasions internal/filter's TestSlurRulesAreEvadable asserts
// get through raw matching. Every one of them must fold to the plain form here.
// That pairing is the point: the weakness is documented where it exists and closed
// where it is closeable.
func TestNormalizeDefeatsKnownEvasions(t *testing.T) {
	// Built from code points, not literals, because most of these characters are
	// invisible or indistinguishable from ASCII and a literal would make a failing
	// test impossible to read.
	var (
		zwj      = string(rune(0x200D))  // zero-width joiner
		zwnj     = string(rune(0x200C))  // zero-width non-joiner
		zwsp     = string(rune(0x200B))  // zero-width space
		softHy   = string(rune(0x00AD))  // soft hyphen
		rtlMark  = string(rune(0x200F))  // right-to-left mark
		combAcut = string(rune(0x0301))  // combining acute accent
		combDier = string(rune(0x0308))  // combining diaeresis
		cyrO     = string(rune(0x043E))  // Cyrillic small o
		cyrA     = string(rune(0x0430))  // Cyrillic small a
		cyrE     = string(rune(0x0435))  // Cyrillic small ie
		cyrP     = string(rune(0x0440))  // Cyrillic small er, looks like p
		cyrC     = string(rune(0x0441))  // Cyrillic small es, looks like c
		greekO   = string(rune(0x03BF))  // Greek small omicron
		fullW    = string(rune(0xFF57))  // fullwidth latin small w
		mathBold = string(rune(0x1D430)) // mathematical bold small w
	)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text is unchanged", "you absolute wop", "you absolute wop"},
		{"case folded", "You ABSOLUTE Wop", "you absolute wop"},

		{"intra-word spaces joined", "you absolute w o p", "you absolute wop"},
		{"intra-word dashes joined", "you absolute w-o-p", "you absolute wop"},
		{"intra-word dots joined", "you absolute w . o . p", "you absolute wop"},
		{"intra-word underscores joined", "you absolute w_o_p", "you absolute wop"},
		{"mixed separators joined", "you absolute w. o -p", "you absolute wop"},

		{"zero-width joiner stripped", "you absolute w" + zwj + "op", "you absolute wop"},
		{"zero-width non-joiner stripped", "you absolute w" + zwnj + "op", "you absolute wop"},
		{"zero-width space stripped", "you absolute w" + zwsp + "op", "you absolute wop"},
		{"soft hyphen stripped", "you absolute w" + softHy + "op", "you absolute wop"},
		{"direction mark stripped", "you absolute w" + rtlMark + "op", "you absolute wop"},

		{"combining acute stripped", "you absolute wo" + combAcut + "p", "you absolute wop"},
		{"precomposed accent decomposed and stripped", "you absolute wóp", "you absolute wop"},
		{"stacked marks stripped", "you absolute wo" + combAcut + combDier + "p", "you absolute wop"},

		{"cyrillic o folded", "you absolute w" + cyrO + "p", "you absolute wop"},
		{"greek omicron folded", "you absolute w" + greekO + "p", "you absolute wop"},
		{"all-cyrillic word folded", "you absolute w" + cyrO + cyrP, "you absolute wop"},
		{"mixed script folded", cyrC + "ope " + cyrA + "nd s" + cyrE + "ethe", "cope and seethe"},

		{"fullwidth folded by NFKD", "you absolute " + fullW + "op", "you absolute wop"},
		{"mathematical bold folded by NFKD", "you absolute " + mathBold + "op", "you absolute wop"},

		{"leet zero folded", "you absolute w0p", "you absolute wop"},
		{"leet at-sign folded", "you absolute w@p", "you absolute wap"},
		{"leet mixed", "y0u @bs0lut3 w0p", "you absolute wop"},

		{"tripled letters collapsed", "you absolute woooop", "you absolute woop"},
		{"doubled letters kept", "that is cool", "that is cool"},

		{"whitespace collapsed", "you    absolute\t\twop", "you absolute wop"},
		{"leading and trailing trimmed", "  wop  ", "wop"},

		{
			// Everything at once, which is what a determined attempt looks like.
			"combined attack",
			"Y" + zwj + "0u " + cyrA + "bs" + greekO + "lu" + combAcut + "te  W" + zwsp + "0" + softHy + "p",
			"you absolute wop",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Normalize(c.in); got != c.want {
				t.Errorf("Normalize(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

// TestNormalizeDoesNotJoinOrdinaryText is the counterweight to
// joinSpacedLetters, and it is the failure mode that would matter most in
// practice. Joining too eagerly turns ordinary chat into one long string, and then
// any pattern matches something.
func TestNormalizeDoesNotJoinOrdinaryText(t *testing.T) {
	cases := map[string]string{
		"a big deal":                       "a big deal",
		"i am a fan":                       "i am a fan",
		"is a b test":                      "is a b test",
		"the quick brown fox":              "the quick brown fox",
		"do you want to go or not":         "do you want to go or not",
		"i think it is a good idea for us": "i think it is a good idea for us",

		// Hyphenated and punctuated real words. Each has a separator adjacent to a
		// real multi-character word, which ends any run, so none of them fold.
		"a well-known e-mail address": "a well-known e-mail address",
		"it is up-to-date":            "it is up-to-date",
		"x-ray of my cat":             "x-ray of my cat",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := Normalize(in); got != want {
				t.Errorf("Normalize(%q) = %q, want it unchanged", in, got)
			}
		})
	}
}

// TestNormalizeJoinsOnlyRunsOfThreeOrMore pins the threshold, because it is the
// single number that decides whether this is a defence or a false-positive
// generator.
func TestNormalizeJoinsOnlyRunsOfThreeOrMore(t *testing.T) {
	if got := Normalize("a b"); got != "a b" {
		t.Errorf("two single letters must not join: got %q", got)
	}
	if got := Normalize("a b c"); got != "abc" {
		t.Errorf("three single letters must join: got %q", got)
	}
	if got := Normalize("a b c d e"); got != "abcde" {
		t.Errorf("longer runs must join: got %q", got)
	}
	// A run adjacent to real words joins only the run.
	if got := Normalize("say a b c please"); got != "say abc please" {
		t.Errorf("got %q, want %q", got, "say abc please")
	}
}

// TestNormalizeIsIdempotent matters because the gate may see already-normalized
// text (a pattern being validated against itself, a rule tested at load time), and
// a second pass changing the result would mean the ruleset and the input disagree
// about what the canonical form is.
func TestNormalizeIsIdempotent(t *testing.T) {
	inputs := []string{
		"you absolute wop",
		"Y0u " + string(rune(0x0430)) + "bs0lute W" + string(rune(0x200D)) + "0p",
		"a b c d",
		"woooop",
		"",
		"   ",
		"2026 was a year",
	}
	for _, in := range inputs {
		once := Normalize(in)
		twice := Normalize(once)
		if once != twice {
			t.Errorf("Normalize is not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func TestNormalizeEdgeCases(t *testing.T) {
	cases := map[string]string{
		"":     "",
		" ":    "",
		"\t\n": "",
		// Punctuation folds to letters by design, then the repeat collapse caps the
		// run at two.
		"!!!":        "ii",
		"\U0001F426": "\U0001F426",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormalizeMangesNumbers documents a deliberate cost rather than a bug. The
// leet table maps the digits that get used as letters onto those letters, so this
// form is useless for anything numeric. That is why the output is matching-only and
// must never be stored, emitted or shown to a user.
//
// Not every digit is mapped, and that is also deliberate: 2 is far more often "to"
// than "z", so mapping it would buy a rare evasion at the cost of mangling ordinary
// text further. The invariant asserted here is that the digits which ARE used as
// letter substitutes fold, not that no digit survives.
func TestNormalizeMangesNumbers(t *testing.T) {
	if got := Normalize("2026"); got == "2026" {
		t.Error("expected the leet digits to fold; if the table changed, update the " +
			"comment on Normalize")
	}

	// The substitutions that actually appear in evasion attempts.
	for in, want := range map[string]string{
		"l33t":   "leet",
		"h4ck":   "hack",
		"n00b":   "noob",
		"5tup1d": "stupid",
		"g0t":    "got",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}

	// And the ones deliberately left alone, so removing them later is a visible
	// decision rather than a drift.
	if !strings.ContainsRune(Normalize("2 and 2"), '2') {
		t.Error("2 is deliberately not folded; if that changed, update the comment " +
			"on the confusables table")
	}
}
