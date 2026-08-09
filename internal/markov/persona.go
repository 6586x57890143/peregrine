package markov

import (
	"math"
	"strings"
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
		chance = math.Min(1.0, chance*1.3)
	}

	// Longer sentences carry filler better than short ones, so intensity scales with
	// length. Kept from legacy, which had the same idea with the numbers inline.
	lengthFactor := math.Min(1.0, float64(len(fields))/20.0)
	chance *= 0.7 + 0.6*lengthFactor

	if src.Float64() >= chance {
		return s
	}

	switch src.IntN(4) {
	case 0:
		return openers[src.IntN(len(openers))] + " " + s
	case 1:
		return s + " " + closers[src.IntN(len(closers))]
	case 2:
		return insertAt(fields, interjections[src.IntN(len(interjections))], insertPos(src, len(fields)))
	default:
		// A meta-comment attaches to the end of a word rather than standing alone, so
		// it reads as an aside rather than as a dropped token.
		pos := insertPos(src, len(fields))
		out := append([]string(nil), fields...)
		out[pos-1] += " " + metaComments[src.IntN(len(metaComments))]
		return strings.Join(out, " ")
	}
}

// Style on a Generator delegates, for callers that already have one.
func (g *Generator) Style(s string, p Persona, aboutName bool) string {
	return Style(g.src, g.weights, s, p, aboutName)
}

// insertPos picks where filler goes, weighted toward the middle.
//
// This is the part of the old implementation that was actually wrong rather than merely
// duplicated: it used `1 + rand.IntN(len(fields)-2)`, a flat draw over the interior. A
// flat draw puts filler immediately after the first word about as often as it puts it in
// the middle, and at the edges an interjection reads as a typo. A triangular draw, the
// average of two uniforms, concentrates on the middle with no tuning constant.
//
// Returns a position in [1, n-1], so filler never precedes the first word (that is what
// an opener is for) and never follows the last (that is a closer).
func insertPos(src Source, n int) int {
	if n <= 2 {
		return 1
	}
	span := n - 1 // valid positions are 1..n-1
	a := src.IntN(span)
	b := src.IntN(span)
	return 1 + (a+b)/2
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
