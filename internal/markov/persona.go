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

	// THE EDGES ONLY. Interjections and meta-comments spliced into the middle were half of
	// all styling and the most common way a reply stopped reading as a sentence: replayed
	// against the production corpus, "(just saying)", "(or so they say)" and "(allegedly)"
	// were among the most frequent unattested trigrams in the output, and
	// "gotten that is (just saying) good to alex" is what they read like. Finding 45 had
	// already bounded WHERE they could go; no position fixed what they did to the sentence
	// around them (SPEC.md section 8, finding 60).
	if src.IntN(2) == 0 {
		return openers[src.IntN(len(openers))] + " " + s
	}
	return s + " " + closers[src.IntN(len(closers))]
}

// Style on a Generator delegates, for callers that already have one.
func (g *Generator) Style(s string, p Persona, aboutName bool) string {
	return Style(g.src, g.weights, s, p, aboutName)
}
