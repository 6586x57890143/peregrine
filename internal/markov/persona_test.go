package markov

import (
	"strings"
	"testing"
)

// TestStyleLeavesShortSentencesAlone. A three-word reply plus an opener is mostly
// filler, and filler is the seasoning rather than the dish.
func TestStyleLeavesShortSentencesAlone(t *testing.T) {
	w := DefaultWeights()
	w.StyleChance = 1.0
	w.StyleChanceName = 1.0

	for _, s := range []string{"", "bird", "bird moment", "the bird is"} {
		if got := Style(seeded(1, 2), w, s, PersonaRoast, true); got != s {
			t.Errorf("Style(%q) = %q, want it untouched: under four words there is not "+
				"enough sentence to season", s, got)
		}
	}
}

// TestStyleInsertsFillerWhenTheChanceIsCertain. With chance forced to 1 every branch
// must produce something, which is what makes the four-way switch worth testing at all:
// three of its four arms were unreachable in practice in the old implementation because
// the chance was low and the arms were never separately exercised.
func TestStyleInsertsFillerWhenTheChanceIsCertain(t *testing.T) {
	w := DefaultWeights()
	w.StyleChance = 1.0
	w.StyleChanceName = 1.0

	const base = "the bird is loose in the server again"
	seenChanged := 0
	for i := range 60 {
		got := Style(seeded(uint64(i), uint64(i+1)), w, base, PersonaNeutral, false)
		if got != base {
			seenChanged++
			// Whatever arm fired, the original words must all survive in order: filler
			// adds, it does not rewrite. A pass that dropped a word would be laundering
			// the sentence, which is a thing this repo refuses to do anywhere.
			if !containsInOrder(got, strings.Fields(base)) {
				t.Errorf("Style produced %q, which does not contain the original words in "+
					"order", got)
			}
		}
	}
	if seenChanged == 0 {
		t.Error("with the chance forced to 1, no sentence was ever styled")
	}
}

// TestStyleRespectsTheChance covers the dial itself: at zero the post-pass is off, which
// is what an operator turning the persona down should get.
func TestStyleRespectsTheChance(t *testing.T) {
	w := DefaultWeights()
	w.StyleChance = 0
	w.StyleChanceName = 0

	const base = "the bird is loose in the server again"
	for i := range 100 {
		if got := Style(seeded(uint64(i), 7), w, base, PersonaNeutral, false); got != base {
			t.Fatalf("with StyleChance 0, Style still changed the sentence to %q", got)
		}
	}
}

// TestStyleIsMoreLikelyForANameAndForRoast. Both are legacy's judgement, preserved: a
// reply about a specific person is the one most worth sharpening, and the roast persona
// is the one that wants filler.
func TestStyleIsMoreLikelyForANameAndForRoast(t *testing.T) {
	w := DefaultWeights()
	const base = "the bird is loose in the server again"

	rate := func(persona Persona, aboutName bool) float64 {
		changed := 0
		const runs = 3000
		for i := range runs {
			if Style(seeded(uint64(i), 99), w, base, persona, aboutName) != base {
				changed++
			}
		}
		return float64(changed) / runs
	}

	plain := rate(PersonaNeutral, false)
	named := rate(PersonaNeutral, true)
	roast := rate(PersonaRoast, false)

	if named <= plain {
		t.Errorf("named %.3f is not above plain %.3f", named, plain)
	}
	if roast <= plain {
		t.Errorf("roast %.3f is not above plain %.3f", roast, plain)
	}
}

// TestInsertPosPrefersTheMiddle is the pin for the part of the old implementation that
// was actually wrong rather than merely duplicated.
//
// It used a flat draw over the sentence interior, which puts filler immediately after
// the first word about as often as in the middle. At the edges an interjection reads as
// a typo. A triangular draw concentrates on the middle with no tuning constant.
func TestInsertPosPrefersTheMiddle(t *testing.T) {
	src := seeded(3, 4)
	const n = 11 // valid positions 1..10, middle around 5
	const runs = 20000

	var edge, middle int
	for range runs {
		pos := insertPos(src, n)
		if pos < 1 || pos > n-1 {
			t.Fatalf("insertPos returned %d, outside [1, %d]", pos, n-1)
		}
		switch {
		case pos <= 2 || pos >= n-2:
			edge++
		case pos >= 4 && pos <= 6:
			middle++
		}
	}
	if middle <= edge {
		t.Errorf("middle positions chosen %d times against %d at the edges; a flat draw "+
			"would put filler where it reads as a typo", middle, edge)
	}
}

func TestInsertPosHandlesTinySentences(t *testing.T) {
	src := seeded(1, 1)
	for n := range 4 {
		if got := insertPos(src, n); got < 0 {
			t.Errorf("insertPos(%d) = %d", n, got)
		}
	}
}

// TestInsertAtDoesNotAliasTheCallersSlice. The old implementation used the
// append-into-a-subslice trick, which writes through the caller's backing array. It was
// latent rather than live because the caller did not keep a reference, but a copy costs
// one allocation on a path that already joins the whole slice into a string.
func TestInsertAtDoesNotAliasTheCallersSlice(t *testing.T) {
	fields := []string{"a", "b", "c", "d"}
	original := append([]string(nil), fields...)

	insertAt(fields, "X", 2)

	for i := range fields {
		if fields[i] != original[i] {
			t.Fatalf("insertAt mutated the caller's slice: %v, want %v", fields, original)
		}
	}
}

func TestInsertAtPlacesTheWord(t *testing.T) {
	got := insertAt([]string{"a", "b", "c"}, "X", 2)
	if got != "a b X c" {
		t.Errorf("got %q, want \"a b X c\"", got)
	}
	// An out-of-range position appends rather than panicking, because a styling bug
	// must not take down a reply.
	if got := insertAt([]string{"a"}, "X", 99); got != "a X" {
		t.Errorf("got %q, want \"a X\"", got)
	}
}

// TestLexiconBiasOnlyAppliesToRoast, and the lexicon is shared with the post-pass rather
// than being a second copy. Two mechanisms with overlapping intent is what finding G6
// was about.
func TestLexiconBiasOnlyAppliesToRoast(t *testing.T) {
	g := New(newFake(), testParams(), seeded(1, 2))

	if got := g.lexiconBias(PersonaNeutral, "cringe"); got != 0 {
		t.Errorf("neutral persona applied a bias of %.3f", got)
	}
	got := g.lexiconBias(PersonaRoast, "cringe")
	if got <= 0 {
		t.Errorf("roast persona applied no bias to a lexicon word, got %.3f", got)
	}
	if got > DefaultWeights().Persona {
		t.Errorf("bias %.3f exceeds the persona weight %.3f", got, DefaultWeights().Persona)
	}
	if got := g.lexiconBias(PersonaRoast, "aardvark"); got != 0 {
		t.Errorf("a non-lexicon word got a bias of %.3f", got)
	}
}

// TestLexiconCoversTheInflectedForms is the follow-up to what the M7a golden samples
// exposed: matching is whole-token, so "cope" being in the lexicon did nothing for
// "coping", which is what people actually write. Stemming is the wrong answer for a meme
// register, where the inflected form is often the joke, so the forms are enumerated.
func TestLexiconCoversTheInflectedForms(t *testing.T) {
	pairs := [][2]string{
		{"cope", "coping"},
		{"seethe", "seething"},
		{"mald", "malding"},
		{"ratio", "ratioed"},
		{"clown", "clowning"},
	}
	for _, p := range pairs {
		if _, ok := roastLexicon[p[0]]; !ok {
			t.Errorf("base form %q missing from the lexicon", p[0])
		}
		if _, ok := roastLexicon[p[1]]; !ok {
			t.Errorf("inflected form %q missing from the lexicon; whole-token matching "+
				"means the base form does not cover it", p[1])
		}
	}
}

func TestPersonaString(t *testing.T) {
	if PersonaRoast.String() != "roast" || PersonaNeutral.String() != "neutral" {
		t.Error("persona names are used in log lines and must be readable")
	}
}

// containsInOrder reports whether every want appears in s, in order.
func containsInOrder(s string, want []string) bool {
	rest := s
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			return false
		}
		rest = rest[i+len(w):]
	}
	return true
}
