package markov

import (
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/text"
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

	// Every position offered, so this measures the draw rather than the safety filter.
	all := make([]int, 0, n-1)
	for p := 1; p <= n-1; p++ {
		all = append(all, p)
	}

	var edge, middle int
	for range runs {
		pos := insertPos(src, all)
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

// TestInsertPosSignalsWhenThereIsNowhereSafe.
//
// -1 rather than a fallback position, because the caller must be able to decline to add
// filler at all. Falling back to an arbitrary index is what produced "why would ngl you say".
func TestInsertPosSignalsWhenThereIsNowhereSafe(t *testing.T) {
	src := seeded(1, 1)
	if got := insertPos(src, nil); got != -1 {
		t.Errorf("insertPos with no candidates = %d, want -1", got)
	}
	if got := insertPos(src, []int{3}); got != 3 {
		t.Errorf("insertPos with one candidate = %d, want 3", got)
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

// TestStyleNeverSplitsAConstruction is the pin for finding 45, and it fails against the
// pre-M16 splicer.
//
// The live sample that motivated it was "why would ngl you say", where the post-pass put an
// interjection between a modal and its subject. Style is the fourth producer of words in the
// pipeline and was the only one with no rule about what it was joining.
//
// The rule being asserted is the measured one, not the obvious one. "neither neighbour is a
// function word" is stricter and wrong: it would reject "greg is lowkey coping", which is
// where an adverb belongs. What breaks is filler BEFORE a function word, plus the one case a
// determiner on the left makes.
func TestStyleNeverSplitsAConstruction(t *testing.T) {
	interior := map[string]struct{}{}
	for _, w := range interjections {
		interior[w] = struct{}{}
	}

	f := goldenCorpus()
	checked := 0
	for _, temp := range []float64{0.7, 1.0, 1.6} {
		p := testParams()
		p.Temperature = temp
		p.MinDistinctAuthors = 2

		for _, prompt := range goldenPrompts() {
			for _, persona := range []Persona{PersonaNeutral, PersonaRoast} {
				g := New(f, p, seeded(0xC0FFEE, 0xBADF00D))
				for range 8 {
					line := generateReply(g, prompt, persona, true)
					if line == "" {
						continue
					}
					checked++
					words := strings.Fields(line)
					for i := 1; i < len(words)-1; i++ {
						if _, ok := interior[words[i]]; !ok {
							continue
						}
						if text.IsStopWord(words[i+1]) {
							t.Errorf("filler %q sits before the function word %q in %q, which "+
								"binds leftward and reads as a dropped token",
								words[i], words[i+1], line)
						}
						if text.IsDeterminer(words[i-1]) {
							t.Errorf("filler %q sits after the determiner %q in %q, which binds "+
								"rightward to its noun", words[i], words[i-1], line)
						}
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no styled samples generated, so this test would pass vacuously")
	}
	t.Logf("checked %d styled samples", checked)
}

// TestStyleChanceIsCappedAfterTheLengthFactor.
//
// math.Min sat on the roast multiplier, before the length factor multiplied the result
// again, so a long reply about a named person in roast mode reached 1.048 and filler was
// certain. A cap that is not the last operation is not a cap.
func TestStyleChanceIsCappedAfterTheLengthFactor(t *testing.T) {
	w := DefaultWeights()
	long := strings.Repeat("word ", 20)

	// A source that always returns a value just under 1.0 must still sometimes decline,
	// which it cannot do if the effective chance ever reaches 1.0.
	src := fixedSource{f: 0.999}
	if got := Style(src, w, long, PersonaRoast, true); got != long {
		t.Errorf("a 20-word roast reply about a named person always takes filler, so the "+
			"chance reached 1.0: got %q", got)
	}
}

// fixedSource returns a constant, for asserting about a probability rather than a draw.
type fixedSource struct{ f float64 }

func (s fixedSource) Float64() float64 { return s.f }
func (s fixedSource) IntN(n int) int   { return 0 }
