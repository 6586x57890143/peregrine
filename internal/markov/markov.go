// Package markov is the generation engine: the probability model, the heuristics
// layered on top of it, and the sampler.
//
// It imports neither bbolt nor discordgo, and it must not start doing so. The whole
// point of the seam is that a thousand lines of scoring heuristics become testable
// against Go maps with no database, no session and no config, which was impossible
// while they lived inside a bbolt read transaction in package main.
//
// # What changed and why it matters
//
// The engine this replaces was less random than it looked, and that was its worst
// property rather than a cosmetic one. Candidate scores started as raw n-gram counts
// and were then MULTIPLIED by an unbounded topic-gravity term and about a dozen
// further ad-hoc factors, with no normalization anywhere, and the result was raised
// to a power. Scores therefore spanned orders of magnitude and one candidate almost
// always dominated: the sampler was argmax with noise. For a bot whose purpose is
// chaos that is the worst available failure mode, and it hid well, because the output
// still varied slightly (SPEC.md section 5.1).
//
// So the score here is a real log-probability plus additive logits, and the reason
// that ordering matters is that addition in log space is multiplication of
// probabilities by a BOUNDED factor: a weight of 0.7 multiplies a candidate's odds by
// about two, and it does so no matter how large or small the base probability was.
// The old multiplicative form had no such property, which is why a single topic term
// could and did swamp the model.
//
// # Why the tuning weights are constants and the sampler's knobs are not
//
// Params comes from the environment because SPEC.md section 5.3 says those specific
// dials are the operator's: temperature, top-k, top-p, the two Kneser-Ney parameters
// and the author-diversity threshold. Weights does not, and that is deliberate. There
// are thirteen of them, they are only meaningful relative to each other, and the rule
// this repo runs on is that a knob an operator cannot reason about is worse than no
// knob. They are tuned by reading golden samples, which is the only instrument that
// can judge them, and they live in one struct so that reading them is possible at
// all. The previous arrangement had the same numbers scattered across ninety lines of
// a single function as bare multipliers.
package markov

import (
	"github.com/6586x57890143/peregrine/internal/corpus"
)

// Corpus is the read side of the engine's dependency, and every method on it is
// satisfied structurally by *storage.Reader.
//
// Declared here rather than in storage because the consumer owns the interface: that
// is what keeps storage from importing markov and markov from importing storage, and
// it is why the fake in the tests is thirty lines of maps.
//
// Two contracts hold on the values that come back, and neither is enforceable by the
// type system, so they are stated:
//
//   - Tokens are returned in the form the writer stored them, which is the form
//     text.LowerExceptURLs produces: lower case except inside a URL. Nothing here
//     re-normalizes a token, because doing it per candidate per step is real cost on
//     the innermost loop of generation, and because lowering a stored URL would
//     corrupt it. A writer that stores a raw-cased token silently breaks prefix
//     lookups by producing keys no reader will build.
//   - No method may start a transaction. *storage.Reader structurally cannot, which
//     is what makes the nested-read deadlock in SPEC.md finding 1 unwritable rather
//     than merely fixed. Do not widen this interface with anything that could.
type Corpus interface {
	// Successors enumerates the continuations of a prefix. Called only for the
	// orders candidates are drawn from, which stop early, never for the low-order
	// contexts that only contribute interpolation weight.
	Successors(prefix string) ([]corpus.Successor, error)

	// Successor is one continuation by name, for the counts the recursion needs at
	// orders it did not enumerate.
	Successor(prefix, next string) (corpus.Successor, bool, error)

	// PrefixTotal is c(prefix, .), the normalizer of the discounted term.
	PrefixTotal(prefix string) uint64

	// HasSuccessors is the cheap yes-or-no. Seed selection asks it dozens of times
	// per reply, once per candidate n-gram and once per two-hop candidate, and only
	// needs the answer rather than the list. It is NOT a byte-prefix match: keys are
	// <prefix> NUL <next>, so a query for "the" must not be satisfied by "the cat"
	// keys.
	HasSuccessors(prefix string) bool

	// FirstPrefix is the absolute fallback when no seed tier matches, because a real
	// prefix with continuations beats a sentinel with none.
	FirstPrefix() (string, bool)

	// KNStats carries N1+(prefix .) and N1+(. token).
	KNStats(prefix, token string) (corpus.KNStats, error)

	// TotalDistinctPredecessors is the denominator of the continuation unigram.
	TotalDistinctPredecessors() uint64

	// TotalTopicCount is the denominator of the raw unigram, for KNRawMix.
	TotalTopicCount() uint64

	// TopicCount is a word's raw unigram frequency.
	TopicCount(word string) uint64

	// TopicWordsFor and NameTopicsFor answer the co-occurrence questions the topic
	// heuristics ask.
	TopicWordsFor(word string) (map[string]corpus.TopicAssoc, error)
	NameTopicsFor(name string) (map[string]corpus.TopicAssoc, error)

	// IsName is presence only. The decoding version would put a JSON unmarshal in
	// the innermost loop, which is why storage has both.
	IsName(key string) bool
}

// Source is the randomness the engine draws on.
//
// It is a parameter rather than a package-level generator, and that is not a style
// preference. There used to be one shared *rand.Rand seeded in main and called from
// every per-message goroutine, which is a live data race on the bot's hottest path
// (SPEC.md finding 3). The fix in M3 was to have no shared generator at all, using
// math/rand/v2's goroutine-safe top-level functions, and this interface preserves
// that: DefaultSource holds no state, so production still has nothing to race on,
// while a test can pass rand.New(rand.NewPCG(...)) and get reproducible output.
//
// Reproducibility is not a nicety here either. Generation quality cannot be settled
// by assertions, so the golden-sample harness has to produce the same text twice
// before a change to a weight can be attributed to the weight.
type Source interface {
	Float64() float64
	IntN(n int) int
}

// Params are the operator-visible dials from SPEC.md section 5.3.
type Params struct {
	// MaxNGram is the highest order. The context is at most MaxNGram-1 words.
	MaxNGram int

	// Temperature divides every logit. Above 1 flattens the distribution and below
	// 1 sharpens it.
	//
	// This replaces Creativity, whose arithmetic contradicted its name: it was
	// applied as an exponent of 1/(Creativity+0.01), so its 0.75 default gave an
	// exponent of 1.316, which SHARPENED, and raising it toward 1 could only ever
	// approach an exponent of 1.0 and never pass it. The dial could not reach the
	// half of its own range that would add chaos, which is why M2 deliberately left
	// it a constant instead of promoting it with the others (SPEC.md section 5.3).
	Temperature float64

	// TopK truncates the candidate tail before sampling. 0 disables.
	//
	// Truncation is what makes a high temperature surprising rather than word
	// salad: cut the tail, then sample the surviving head hot. Without it,
	// raising temperature mostly promotes the thousands of one-count continuations
	// that a sparse corpus is made of.
	TopK int

	// TopP is the nucleus threshold, applied after TopK. 1 disables.
	TopP float64

	// KNDiscount is the absolute discount D subtracted from each observed count.
	KNDiscount float64

	// KNRawMix is mu: how far the base case is pulled back from Kneser-Ney's
	// continuation distribution toward raw frequency. 0 is textbook KN, 1 is raw
	// counts.
	//
	// This is the single most counter-intuitive number in the codebase and the
	// comment on baseProb explains it. It is not a hedge against getting KN wrong.
	KNRawMix float64

	// MinDistinctAuthors is how many different people must have produced a
	// continuation before the bot will generate it, independent of how often it was
	// seen.
	//
	// n-gram weight is raw frequency, so repeating a phrase is a direct write to the
	// model and one determined user can teach the bot to say anything. This turns
	// that attack from persistence into collusion (SPEC.md section 4, A6).
	MinDistinctAuthors int

	// MinWords and MaxWords bound sentence length. The target is sampled between them
	// once per sentence, skewed short: a forty-word Markov ramble reads as a
	// malfunction in a chat channel and a six-word non-sequitur reads as a joke.
	MinWords int
	MaxWords int

	// CooccurrenceWindow bounds how far apart two words can be and still count as
	// co-occurring on the LEARN path. 0 means unbounded, which is the all-pairs loop
	// this replaces: quadratic in message length, inside the single write transaction
	// that serializes all ingestion.
	CooccurrenceWindow int

	// PromptRelevance is the logit added to a candidate that appears in the prompt.
	//
	// Its units changed in M7a and its old values are now out of range on purpose.
	// It used to be added to an unnormalized score whose scale was raw n-gram
	// counts, so its default was 15.0; here it is a logit, where 15.0 would
	// multiply a candidate's odds by three million and make prompt echo the only
	// thing the bot ever does. Keeping the variable's NAME while narrowing its
	// validated range means a stale 15.0 in an operator's .env is a startup error
	// that names the new range, rather than a silent behavior change. Renaming it
	// would have made the stale value simply stop being read, which is the failure
	// this repo keeps designing against.
	PromptRelevance float64
}

// Weights are the logit coefficients. See the package comment for why these are not
// environment variables.
//
// Every one is a bounded addition in log space, and the ranges are stated because an
// unbounded term is exactly what went wrong before: topic gravity was 1 + sum(...)
// with nothing capping the sum, multiplied into the score.
type Weights struct {
	// TopicGravity biases toward words associated with the prompt's core topics,
	// weighted by how well their usual position matches where we are in the
	// sentence. Squashed with tanh, so the term is bounded by this weight however
	// many topics pile on.
	TopicGravity float64

	// NameAssoc is the same idea for topics associated with a recognized name.
	NameAssoc float64

	// CurrentTopic nudges toward staying inside the topic the sentence started in.
	CurrentTopic float64

	// Significance rewards globally frequent words, gently. This is the term that
	// short tokens were invisible to until M6b, because topic counts skipped
	// anything under three characters, so "ok", "no" and "wtf" scored zero in a
	// server whose register is short interjections (SPEC.md finding G10).
	Significance float64

	// IsName gives a small edge to learned display names.
	IsName float64

	// Persona is the roast and aggro vocabulary bias. The lexicon it reads is a
	// package variable rather than a map rebuilt per candidate per step, which is
	// what the previous code did, fourteen entries and fourteen lowercase
	// conversions deep inside the innermost loop.
	Persona float64

	// RecentContext nudges toward words from the recent conversation.
	RecentContext float64

	// PromptGravity rewards similarity between the sentence so far and the prompt.
	PromptGravity float64

	// Repetition, ImmediateRepeat, Bigram and Trigram are penalties, so they are
	// negative. They are deliberately gentle: memetic repetition is the desired
	// register here, and a copypasta cadence or a doubled emote is the joke rather
	// than a defect. These suppress stuttering artifacts without flattening
	// repetition that looks intentional (SPEC.md section 5.3).
	Repetition      float64
	RepetitionCap   float64
	ImmediateRepeat float64
	BigramRepeat    float64
	TrigramRepeat   float64

	// EndEarly, EndLate and EndLateCap are the length model's three regimes: a
	// penalty below the floor easing to zero at the target, then a bonus growing past
	// the target so a sentence that has outstayed its welcome ends on its own rather
	// than only at the hard cap. See length.go.
	//
	// EndLateCap exists because an end token that becomes certain regardless of
	// context makes every long sentence stop in the same grammatical place, which
	// reads as a template.
	EndEarly   float64
	EndLate    float64
	EndLateCap float64

	// Connective is the mid-sentence nudge toward and/but/then/because, and away
	// from them at the very end.
	Connective float64

	// StyleChance and StyleChanceName are the probability that the persona post-pass
	// adds filler, the second when the reply is about a recognized person. A reply
	// about someone specific is the one most worth making sharper, which is legacy's
	// judgement and is kept.
	StyleChance     float64
	StyleChanceName float64
}

// DefaultWeights are the starting point, to be moved by reading golden samples.
//
// The scale to keep in mind while tuning: log-probability differences between a
// strong and a weak continuation in a sparse corpus run to several nats, so a weight
// near 1.0 is a firm opinion and anything past 2.0 will override the model outright.
// Nothing here is above 1.0 except the repetition penalties, which are meant to
// override.
func DefaultWeights() Weights {
	return Weights{
		TopicGravity:    0.70,
		NameAssoc:       0.45,
		CurrentTopic:    0.35,
		Significance:    0.20,
		IsName:          0.25,
		Persona:         0.80,
		RecentContext:   0.25,
		PromptGravity:   0.80,
		Repetition:      -0.55,
		RepetitionCap:   -2.20,
		ImmediateRepeat: -2.50,
		BigramRepeat:    -1.60,
		TrigramRepeat:   -3.00,
		EndEarly:        -2.30,
		EndLate:         0.35,
		EndLateCap:      1.80,
		Connective:      0.15,
		StyleChance:     0.35,
		StyleChanceName: 0.65,
	}
}

// Generator holds the corpus, the dials and the randomness for one bot.
//
// It is safe for concurrent use as long as Source is, which DefaultSource is. It
// keeps no per-sentence state: everything mutable lives in the Step the caller owns,
// which is what lets one Generator serve every message goroutine.
type Generator struct {
	corpus  Corpus
	params  Params
	weights Weights
	src     Source
}

// New builds a Generator. A nil Source means DefaultSource.
func New(c Corpus, p Params, src Source) *Generator {
	if src == nil {
		src = DefaultSource{}
	}
	return &Generator{corpus: c, params: p, weights: DefaultWeights(), src: src}
}

// SetWeights overrides the logit coefficients. For the golden harness and for
// tuning; production uses the defaults.
func (g *Generator) SetWeights(w Weights) { g.weights = w }

// Params returns the dials, for callers that need the length bounds.
func (g *Generator) Params() Params { return g.params }
