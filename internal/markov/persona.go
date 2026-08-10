package markov

import (
	"math"
	"strings"

	"github.com/6586x57890143/peregrine/internal/text"
)

// The persona layer: ONE mechanism where there were two.
//
// Legacy had a roast-vocabulary bias inside the sampler AND a separate applyEdgyStyle
// pass that injected filler into the finished sentence. Two mechanisms with overlapping
// intent, neither testable, and the second picked its insertion point with a raw random
// index (SPEC.md finding G6).
//
// They are one thing here: a Persona owns both an in-sampler lexicon bias and a
// post-pass, so "how roast-y is this reply" is one decision made once rather than two
// unrelated coin flips that can disagree. The post-pass chooses where to insert by
// POSITION WEIGHT rather than a bare index, which is the part that was actually broken:
// an interjection is funny in the middle of a sentence and reads as a typo at its edges.

// Persona selects which vocabulary bias and which filler apply to a sentence.
type Persona int

const (
	// PersonaNeutral applies no vocabulary bias and the mild filler set.
	PersonaNeutral Persona = iota

	// PersonaRoast biases toward roast vocabulary and is more willing to add filler.
	PersonaRoast
)

func (p Persona) String() string {
	if p == PersonaRoast {
		return "roast"
	}
	return "neutral"
}

// roastLexicon is the roast vocabulary and its relative strength, hoisted to a package
// variable.
//
// This used to be a fourteen-entry map literal allocated INSIDE the per-candidate loop,
// with fourteen calls to a lowercase helper on constants that were already lower case,
// which is to say once per candidate per step per generated word (finding G6). The keys
// are written pre-normalized so no conversion happens at all.
//
// Values are relative weights in 0..1, multiplied by Weights.Persona, so the vocabulary
// can be extended without retuning the logit scale.
//
// KNOWN GAP, recorded rather than papered over: matching is whole-token, so "cope" is
// here and "coping" is what people actually write. Stemming is the wrong answer for a
// meme register, where the inflected form often IS the joke, so the fix is to enumerate
// the forms that appear in real chat. The golden samples are what reveal which those
// are, and a twenty-line synthetic corpus cannot say. See SPEC.md section 10.
var roastLexicon = map[string]float64{
	"dumbass": 1.00, "idiot": 1.00,
	"loser": 0.80, "clown": 0.80, "clowning": 0.80,
	"cringe": 0.60, "pathetic": 0.60, "cringing": 0.60,
	"weak": 0.40, "sad": 0.40, "cope": 0.40, "coping": 0.40,
	"seethe": 0.40, "seething": 0.40, "mald": 0.40, "malding": 0.40,
	"ratio": 0.30, "ratioed": 0.30,
	"lmao": 0.20, "lol": 0.20,
}

// Filler sets for the post-pass. Package variables, not rebuilt per call.
var (
	openers = []string{
		"ngl", "tbh", "bruh", "like", "i guess", "idk but", "listen", "ok so",
		"fr tho", "no cap", "deadass", "lowkey", "bet", "sheesh", "valid",
	}
	closers = []string{
		"lol", "lmao", "whatever", "i guess", "or something", "smh", "for real",
		"periodt", "iykyk", "no cap", "fr fr", "ong",
	}
	interjections = []string{"ngl", "fr", "tbh", "like", "i mean", "lowkey", "bet"}
	metaComments  = []string{
		"(i think)", "(just saying)", "(don't quote me)", "(or so they say)", "(allegedly)",
	}
)

// lexiconBias is the in-sampler half: the logit added to a candidate that is in the
// persona's vocabulary.
func (g *Generator) lexiconBias(p Persona, token string) float64 {
	if p != PersonaRoast {
		return 0
	}
	if strength, ok := roastLexicon[token]; ok {
		return g.weights.Persona * strength
	}
	return 0
}

// Style is the post-pass half: it adds filler to a finished sentence, or returns it
// unchanged.
//
// A package function rather than a Generator method, and deliberately so. A Generator
// holds a Corpus, which in production is a *storage.Reader bound to one transaction,
// whereas the post-pass runs AFTER that transaction has closed, because it comes after
// the sentence cleaner which needs a Discord session. Making this a method would have
// meant either constructing a Generator with a nil corpus, which is a trap waiting for
// the first person to add a corpus lookup here, or holding a Reader past its
// transaction, which is the bug the Reader type exists to prevent. It needs neither the
// corpus nor the model, so it asks for neither.
//
// A nil src means DefaultSource, so a caller that does not care about reproducibility
// does not have to name one.
//
// aboutName raises the chance, preserving legacy's judgement: a reply about a specific
// person is the one most worth making sharper.
//
// Sentences under four words are returned untouched. A three-word reply plus an opener
// is mostly filler, and filler is the seasoning rather than the dish.
func Style(src Source, w Weights, s string, p Persona, aboutName bool) string {
	if src == nil {
		src = DefaultSource{}
	}

	fields := strings.Fields(s)
	if len(fields) < 4 {
		return s
	}

	chance := w.StyleChance
	if aboutName {
		chance = w.StyleChanceName
	}
	if p == PersonaRoast {
		chance *= 1.3
	}

	// Longer sentences carry filler better than short ones, so intensity scales with
	// length. Kept from legacy, which had the same idea with the numbers inline.
	lengthFactor := math.Min(1.0, float64(len(fields))/20.0)
	chance *= 0.7 + 0.6*lengthFactor

	// THE CAP GOES LAST OR IT IS NOT A CAP. It used to sit on the roast multiplier, before
	// the length factor multiplied the result again, so a long reply about a named person in
	// roast mode reached a chance above 1.0 and filler was certain rather than likely.
	chance = math.Min(1.0, chance)

	if src.Float64() >= chance {
		return s
	}

	switch src.IntN(4) {
	case 0:
		return openers[src.IntN(len(openers))] + " " + s
	case 1:
		return s + " " + closers[src.IntN(len(closers))]
	case 2:
		pos := insertPos(src, safeInsertPositions(fields))
		if pos < 0 {
			// NO SAFE POSITION MEANS NO FILLER, rather than falling back to an opener or a
			// closer. Two reasons: saying less is this repo's direction for a failure that
			// reads as a malfunction, and redistributing this branch's probability into the
			// other two would silently change the mix of the four styles, which is exactly
			// what a before-and-after golden read needs to stay attributable.
			return s
		}
		return insertAt(fields, interjections[src.IntN(len(interjections))], pos)
	default:
		// A meta-comment attaches to the end of a word rather than standing alone, so
		// it reads as an aside rather than as a dropped token.
		pos := insertPos(src, safeInsertPositions(fields))
		if pos < 0 {
			return s
		}
		out := append([]string(nil), fields...)
		out[pos-1] += " " + metaComments[src.IntN(len(metaComments))]
		return strings.Join(out, " ")
	}
}

// Style on a Generator delegates, for callers that already have one.
func (g *Generator) Style(s string, p Persona, aboutName bool) string {
	return Style(g.src, g.weights, s, p, aboutName)
}

// safeInsertPositions returns the interior positions where filler can go without splitting a
// construction. Position p means "between fields[p-1] and fields[p]".
//
// # Why this exists (SPEC.md section 8, finding 45)
//
// Style is the FOURTH producer of words in the pipeline, after the sampler, the seed and the
// dead-end jump, and it was the only one with no rule about what it was joining. It spliced
// by position alone and produced "why would ngl you say" in live output, splitting a modal
// from its subject. Jump got "NOT AFTER A FUNCTION WORD" in M14 for exactly this failure; a
// rule applied to three of four producers is not a rule.
//
// # The two conditions, and why it is not "neither neighbour is a function word"
//
// That stricter rule is the obvious one and it is wrong, which measuring said and reasoning
// did not. Filler sitting between a copula and a participle is the natural home of an
// adverb: "alexiane ratioed greg is lowkey coping" and "what is lowkey starting drama again"
// both read correctly, and both have a function word on the left. Rejecting them would cost
// good output for nothing.
//
// What actually breaks is inserting BEFORE a function word, because a function word binds
// leftward to what it follows: "are | like | you", "do | ngl | you", "server | ngl | is".
// The one case that misses is a determiner on the left, which binds rightward to its noun:
// "the | tbh | bird". So the rule is those two conditions and no more.
func safeInsertPositions(fields []string) []int {
	if len(fields) <= 2 {
		return nil
	}
	out := make([]int, 0, len(fields))
	for p := 1; p <= len(fields)-1; p++ {
		if text.IsStopWord(fields[p]) || text.IsDeterminer(fields[p-1]) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// insertPos picks one of the candidate positions, weighted toward the middle.
//
// This is the part of the old implementation that was actually wrong rather than merely
// duplicated: it used `1 + rand.IntN(len(fields)-2)`, a flat draw over the interior. A
// flat draw puts filler immediately after the first word about as often as it puts it in
// the middle, and at the edges an interjection reads as a typo. A triangular draw, the
// average of two uniforms, concentrates on the middle with no tuning constant.
//
// The draw is over the index into the candidate list rather than over the raw span, so the
// mid-sentence preference survives the filter above.
func insertPos(src Source, positions []int) int {
	switch len(positions) {
	case 0:
		return -1
	case 1:
		return positions[0]
	}
	a := src.IntN(len(positions))
	b := src.IntN(len(positions))
	return positions[(a+b)/2]
}

// insertAt splices a word into a field slice at pos.
//
// It copies rather than using the append-into-a-subslice trick the old code used, which
// aliased the caller's backing array and would have corrupted it if the caller had kept
// a reference. It did not, so this was latent rather than live, but the copy costs one
// allocation on a path that already joins the whole slice into a string.
func insertAt(fields []string, word string, pos int) string {
	if pos < 0 || pos > len(fields) {
		pos = len(fields)
	}
	out := make([]string, 0, len(fields)+1)
	out = append(out, fields[:pos]...)
	out = append(out, word)
	out = append(out, fields[pos:]...)
	return strings.Join(out, " ")
}
