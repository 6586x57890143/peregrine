// Package safety is the gate. It owns one normalizer, one ruleset, and the two
// verdicts every path has to pass through: CheckLearn on the way in and CheckEmit
// on the way out.
//
// Two gates rather than one, and that is not belt-and-braces. A Markov chain
// composes novel sequences from n-grams that were learned separately, so fragments
// that were each innocuous can join into something the operator has to answer for.
// Filtering the corpus lowers the rate; only an output gate bounds the result
// (SPEC.md section 4, A3). Removing either one because the other exists would be
// wrong.
//
// Gates go at chokepoints, never at call sites. CheckLearn is called inside
// learnMessage, the single funnel all four of its callers pass through, which is
// what closes the bypass in A1 for a fifth caller nobody has written yet.
package safety

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Normalize folds text into the single form that patterns are matched against.
//
// The output is FOR MATCHING ONLY. It is lossy by design: it mangles numbers,
// discards accents that carry meaning in other languages, and joins characters
// that were deliberately apart. It must never be stored in the corpus, never
// emitted, and never shown to a user. The caller keeps the original.
//
// The reason this exists at all is that the previous filters matched raw text with
// word boundaries and a hand-enumerated set of leet substitutions, so intra-word
// spacing, combining marks, zero-width characters, homoglyphs and any variant
// nobody had thought of walked straight through (SPEC.md section 4, A5). Every one
// of those is asserted as a known bypass in internal/filter's tests and is expected
// to be caught here.
//
// The order of the steps matters:
//
//  1. Case-fold, so patterns need no (?i) and no case variants.
//  2. NFKD, which decomposes accented characters into base plus combining mark and
//     expands compatibility forms, so fullwidth, mathematical bold, superscripts
//     and ligatures all reduce toward ASCII. NFKD rather than NFKC specifically
//     because step 3 needs the marks separated to strip them.
//  3. Drop combining marks and format characters. This is where zero-width joiners,
//     zero-width spaces, soft hyphens, direction marks and stacked diacritics go.
//  4. Fold confusables, which is the homoglyph defence: Cyrillic and Greek letters
//     that render identically to Latin become Latin.
//  5. Fold leet substitutions.
//  6. Collapse whitespace, join single-letter runs, and collapse long repeats.
//
// Steps 4 and 5 are tables rather than clever code because there is no rule to
// derive: which glyphs look alike is a fact about typefaces, not about Unicode.
func Normalize(s string) string {
	// 1. Case-fold. strings.ToLower rather than a full Unicode case-fold because
	// the confusable table below is keyed on lowercase and the difference only
	// matters for scripts this bot already refuses.
	s = strings.ToLower(s)

	// 2 and 3. Decompose, then strip marks and format characters in one pass.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range norm.NFKD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			// Non-spacing mark: a combining accent, now separated from its base by
			// NFKD. Dropping it turns "wóp" into "wop" and defeats an arbitrary
			// stack of diacritics rather than a fixed list of precomposed forms.
			continue
		case unicode.Is(unicode.Cf, r):
			// Format character: zero-width joiner and non-joiner, zero-width space,
			// soft hyphen, the bidirectional overrides. Invisible by definition, so
			// they exist in this position for exactly one reason.
			continue
		case r == 0xFEFF || r == 0x00AD:
			// Byte-order mark and soft hyphen, by code point rather than as
			// literals: a BOM is not even legal mid-file in Go source, and a soft
			// hyphen in a source literal is invisible. Both are already Cf today;
			// this is belt for a future Unicode revision that recategorizes either.
			continue
		default:
			// 4 and 5. Fold confusables and leet, falling through to the rune.
			if folded, ok := confusables[r]; ok {
				b.WriteRune(folded)
				continue
			}
			b.WriteRune(r)
		}
	}

	// 6. Structural collapsing.
	out := collapseSpace(b.String())
	out = joinSpacedLetters(out)
	out = collapseRepeats(out)
	return out
}

// collapseSpace reduces every run of whitespace to a single space and trims the
// ends, so a pattern can rely on single spaces between words.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// joinSpacedLetters turns a run of single letters separated by anything into one
// word, so "w o p", "w-o-p" and "w . o . p" all become "wop".
//
// It works on runs of alphanumerics and the separators between them rather than on
// whitespace-split words, which matters for two reasons the first attempt at this
// got wrong. "w-o-p" is a single whitespace-delimited word, so a word-based version
// never saw it. And in "w . o . p" the dots are themselves single characters, so a
// word-based version joined them into the result and produced "w.o.p".
//
// Two conditions keep this a defence rather than a false-positive generator:
//
//   - At least THREE single letters in the run. Three spaced letters is a
//     deliberate act; two is ordinary text, and "is a b test" must survive.
//   - Only single-character alphanumeric groups count. A multi-character group ends
//     the run, which is why "a big deal" is untouched: "a" is one single letter
//     followed by a real word.
//
// The joined form REPLACES the spaced one rather than being matched alongside it,
// and that is the tradeoff. A message that genuinely writes "a b c" reads as "abc"
// to the ruleset. Acceptable, because the ruleset holds slurs and threat patterns,
// not three-letter words people spell out.
func joinSpacedLetters(s string) string {
	type group struct {
		text  string
		alnum bool
	}

	// Split into alternating alphanumeric and separator groups.
	var groups []group
	var cur strings.Builder
	curAlnum := false
	for i, r := range s {
		a := isAlnum(r)
		if i > 0 && a != curAlnum {
			groups = append(groups, group{cur.String(), curAlnum})
			cur.Reset()
		}
		curAlnum = a
		cur.WriteRune(r)
	}
	if cur.Len() > 0 {
		groups = append(groups, group{cur.String(), curAlnum})
	}

	var out strings.Builder
	out.Grow(len(s))

	// pending holds a candidate run: the single letters seen so far and the
	// separators between them, kept so the run can be written back verbatim if it
	// turns out to be too short to join.
	var letters []string
	var verbatim strings.Builder

	flush := func() {
		if len(letters) >= 3 {
			for _, l := range letters {
				out.WriteString(l)
			}
		} else {
			out.WriteString(verbatim.String())
		}
		letters = letters[:0]
		verbatim.Reset()
	}

	for i, g := range groups {
		switch {
		case g.alnum && runeLen(g.text) == 1:
			letters = append(letters, g.text)
			verbatim.WriteString(g.text)
		case g.alnum:
			// A real word ends any run in progress.
			flush()
			out.WriteString(g.text)
		default:
			// A separator continues a run only if a run is in progress and another
			// single letter follows it. Otherwise it ends the run.
			if len(letters) > 0 && i+1 < len(groups) && groups[i+1].alnum && runeLen(groups[i+1].text) == 1 {
				verbatim.WriteString(g.text)
				continue
			}
			flush()
			out.WriteString(g.text)
		}
	}
	flush()

	return out.String()
}

func isAlnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// collapseRepeats reduces a run of identical characters to at most two, so
// "wooooop" becomes "woop" and "cool" is untouched.
//
// Two rather than one on purpose. English is full of doubled letters (cool, letter,
// better) and collapsing those would both destroy ordinary words and create false
// positives. Anything past two is a deliberate act, and reducing it to two rather
// than one means a pattern only ever has to allow for a single doubling.
func collapseRepeats(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	var prev rune
	count := 0
	for _, r := range s {
		if r == prev {
			count++
			// Write the first two, drop the rest of the run.
			if count < 2 {
				b.WriteRune(r)
			}
			continue
		}
		prev = r
		count = 0
		b.WriteRune(r)
	}
	return b.String()
}

// confusables maps glyphs that render like Latin letters onto those letters, plus
// the leet substitutions.
//
// A table because which glyphs look alike is a fact about typefaces and cannot be
// derived. Unicode's own confusables data is far larger; this covers the Cyrillic
// and Greek letters that actually appear in evasion attempts plus the digit and
// punctuation substitutions people type by hand.
//
// The digit mappings make this form useless for anything numeric, which is another
// reason the output is matching-only: "2026" folds to "zoz6". Ambiguous cases are
// resolved toward the letter that appears in the ruleset, so 1 becomes i rather
// than l, because the patterns that matter contain i.
var confusables = map[rune]rune{
	// Cyrillic
	'а': 'a', // а
	'в': 'b', // в
	'е': 'e', // е
	'ё': 'e', // ё
	'к': 'k', // к
	'м': 'm', // м
	'н': 'h', // н
	'о': 'o', // о
	'р': 'p', // р
	'с': 'c', // с
	'т': 't', // т
	'у': 'y', // у
	'х': 'x', // х
	'і': 'i', // і
	'ј': 'j', // ј
	'ѕ': 's', // ѕ
	'ӏ': 'l', // ӏ
	'һ': 'h', // һ
	'ԁ': 'd', // ԁ
	'ɡ': 'g', // ɡ

	// Greek
	'α': 'a', // α
	'β': 'b', // β
	'γ': 'y', // γ
	'δ': 'd', // δ
	'ε': 'e', // ε
	'ζ': 'z', // ζ
	'η': 'n', // η
	'θ': 'o', // θ
	'ι': 'i', // ι
	'κ': 'k', // κ
	'λ': 'l', // λ
	'μ': 'u', // μ
	'ν': 'v', // ν
	'ξ': 'e', // ξ
	'ο': 'o', // ο
	'π': 'n', // π
	'ρ': 'p', // ρ
	'ς': 's', // ς
	'σ': 'o', // σ
	'τ': 't', // τ
	'υ': 'u', // υ
	'φ': 'o', // φ
	'χ': 'x', // χ
	'ψ': 'y', // ψ
	'ω': 'w', // ω

	// Latin lookalikes that NFKD leaves alone
	'ı': 'i', // dotless i
	'ȷ': 'j', // dotless j
	'ǀ': 'l', // latin letter dental click
	'ⱱ': 'v', // latin small letter v with right hook

	// Leet and punctuation substitutions
	'0': 'o',
	'1': 'i',
	'3': 'e',
	'4': 'a',
	'5': 's',
	'6': 'g',
	'7': 't',
	'8': 'b',
	'9': 'g',
	'@': 'a',
	'$': 's',
	'!': 'i',
	'|': 'l',
	'+': 't',
	'€': 'e', // euro sign
	'£': 'l', // pound sign
}
