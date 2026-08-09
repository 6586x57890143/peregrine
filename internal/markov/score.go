package markov

import (
	"math"
	"strings"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/text"
)

// EndToken is the sentinel the corpus stores for the end of a message.
//
// Lower case, and that matters: the previous scorer contained a branch comparing
// against "<END>" while the only sentinel ever produced was "<end>", guarded by a
// character-length check on a word-joined string. It was dead in both halves
// (SPEC.md section 5.1). Exported so the caller and the engine cannot disagree about
// the spelling again.
const EndToken = "<end>"

// Persona selects which vocabulary bias applies to a sentence.
type Persona int

const (
	PersonaNeutral Persona = iota
	PersonaRoast
)

// roastLexicon is the roast vocabulary and its relative strength, hoisted to a
// package variable.
//
// This used to be a fourteen-entry map literal allocated INSIDE the per-candidate
// loop, with fourteen calls to a lowercase helper on constants that were already
// lower case, which is to say once per candidate per step per generated word
// (SPEC.md finding G6). The keys are written pre-normalized here so no conversion
// happens at all.
//
// Values are relative weights in 0..1, multiplied by Weights.Persona, so the
// vocabulary can be reordered without retuning the logit scale.
var roastLexicon = map[string]float64{
	"dumbass":  1.00,
	"idiot":    1.00,
	"loser":    0.80,
	"clown":    0.80,
	"cringe":   0.60,
	"pathetic": 0.60,
	"weak":     0.40,
	"sad":      0.40,
	"cope":     0.40,
	"seethe":   0.40,
	"mald":     0.40,
	"ratio":    0.30,
	"lmao":     0.20,
	"lol":      0.20,
}

// connectives get a mid-sentence nudge and a late-sentence dampening.
var connectives = map[string]struct{}{
	"and": {}, "but": {}, "then": {}, "because": {},
}

// Step is the mutable state of one sentence under construction, owned by the caller.
//
// Owned by the caller on purpose: the Generator holds no per-sentence state, which is
// what lets one Generator serve every message goroutine without a lock. The caller
// updates Prefix, Sentence, Used, Ngrams and Position between calls to Next.
//
// The sets are built once per sentence rather than per step. The previous scorer
// tokenized the whole prompt and rebuilt a prompt set inside the per-candidate
// function, so that work happened once per candidate per step.
type Step struct {
	// Prefix is the context, most recent word last. Next takes its suffixes.
	Prefix []string

	// Sentence is what has been generated so far, used for the immediate-repetition
	// check and for sentence progress.
	Sentence []string

	// Prompt is the original prompt text, for the similarity term.
	Prompt string

	// PromptSet and RecentSet are normalized word sets.
	PromptSet map[string]struct{}
	RecentSet map[string]struct{}

	// Used counts how often each word has been emitted in this sentence, and Ngrams
	// holds the 1, 2 and 3-grams already produced.
	Used   map[string]int
	Ngrams map[string]struct{}

	// Position is progress through the sentence, 0 at the start and 1 at the cap.
	Position float64

	// MinWords is the length floor below which the end sentinel is penalized.
	MinWords int

	// Persona selects the vocabulary bias.
	Persona Persona

	// CoreTopics are the prompt's non-stop words weighted by global significance,
	// and CurrentTopic is the topic the sentence started in.
	CoreTopics   map[string]float64
	CurrentTopic string

	// NameAssoc aggregates the topic associations of every recognized name in the
	// prompt.
	NameAssoc map[string]corpus.TopicAssoc
}

// Next picks the continuation, or returns "" when there is nothing eligible.
//
// An empty return is a dead end and the caller decides what to do about it: today
// that means a jump word or ending the sentence. It is deliberately not an error,
// because a prefix with no continuation is ordinary in a sparse corpus, and it is
// deliberately not a fallback string, because a fallback is a new output to reason
// about.
func (g *Generator) Next(s *Step) (string, error) {
	m := g.newModel()
	ctxs := g.contexts(s.Prefix)
	if len(ctxs) == 0 {
		return "", nil
	}

	cands, err := m.enumerate(ctxs)
	if err != nil {
		return "", err
	}
	if len(cands) == 0 {
		return "", nil
	}

	cands = g.eligible(cands)
	if len(cands) == 0 {
		return "", nil
	}

	assoc := g.loadAssoc(s)
	for i := range cands {
		cands[i].logit = cands[i].logProb + g.heuristics(s, cands[i], assoc)
	}

	return g.sample(cands), nil
}

// eligible drops continuations that too few distinct people have produced.
//
// This is the author-diversity gate (SPEC.md section 4, A6) and it is a FILTER rather
// than another logit, because a penalty is something a determined poisoner can
// out-repeat and a filter is not. n-gram weight is raw frequency, so repeating a
// phrase is a direct write to the model; requiring k different authors turns that
// from persistence into collusion.
//
// When the filter empties the candidate set the step becomes a dead end, which the
// caller turns into a shorter sentence. That direction is deliberate: the alternative
// is relaxing the threshold when it bites, and a safety control that yields the moment
// it has an effect is not a control. The operational consequence is real and belongs
// in the docs rather than in a workaround: on a fresh corpus the bot is nearly silent
// until several people have said similar things.
//
// The bot's own output is already excluded upstream, since learnMessage passes an
// empty author for its own messages, so self-learning cannot bootstrap a phrase into
// eligibility.
func (g *Generator) eligible(cands []candidate) []candidate {
	min := g.params.MinDistinctAuthors
	if min <= 0 {
		return cands
	}

	out := cands[:0]
	for _, c := range cands {
		// The end sentinel is structural rather than content: gating it on author
		// diversity would mean a sentence cannot end until several people have ended
		// a message the same way, which is a length bug wearing a safety hat.
		if c.token == EndToken || c.authors >= uint32(min) {
			out = append(out, c)
		}
	}
	return out
}

// assocCache holds the co-occurrence maps one sentence needs, loaded once.
type assocCache map[string]map[string]corpus.TopicAssoc

// loadAssoc pre-loads every topic association the heuristics will ask about.
//
// Once per step rather than once per candidate per step, which is what the old code
// did before it grew a cache with the same shape as this one. Kept as an explicit
// type so it is obvious that the scoring loop below does no I/O at all.
func (g *Generator) loadAssoc(s *Step) assocCache {
	cache := make(assocCache, len(s.CoreTopics)+len(s.NameAssoc)+1)
	load := func(topic string) {
		if topic == "" {
			return
		}
		if _, ok := cache[topic]; ok {
			return
		}
		if a, err := g.corpus.TopicWordsFor(topic); err == nil {
			cache[topic] = a
		} else {
			cache[topic] = nil
		}
	}
	for topic := range s.CoreTopics {
		load(topic)
	}
	for topic := range s.NameAssoc {
		load(topic)
	}
	load(s.CurrentTopic)
	return cache
}

// heuristics is the sum of every additive logit for one candidate.
//
// Every term here was a multiplication into an unnormalized score before, and the
// translation is not mechanical: a multiplicative factor of 1.5 means something
// different depending on the score it lands on, whereas an additive 0.4 in log space
// multiplies the candidate's probability by e^0.4 regardless. Unbounded sums are
// squashed with tanh so that no term can dominate however much evidence piles up,
// which is the specific failure that made the old sampler argmax with noise.
func (g *Generator) heuristics(s *Step, c candidate, assoc assocCache) float64 {
	w := g.weights
	tok := c.token
	var logit float64

	// Topic gravity: association with the prompt's core topics, weighted by how well
	// the candidate's usual position matches where we are in the sentence.
	var gravity float64
	for topic, significance := range s.CoreTopics {
		a, ok := assoc[topic]
		if !ok || a == nil {
			continue
		}
		d, ok := a[tok]
		if !ok {
			continue
		}
		strength := math.Sqrt(float64(d.Count))
		pos := math.Exp(-math.Abs(s.Position-d.MeanPosition()) * 5.0)
		gravity += strength * pos * significance
	}
	if gravity > 0 {
		logit += w.TopicGravity * math.Tanh(gravity/4.0)
	}

	// Name association: the same idea for topics tied to a recognized name, applied
	// hierarchically, the name's position gating the word's.
	var nameScore float64
	for topic, td := range s.NameAssoc {
		topicPos := math.Exp(-math.Abs(s.Position-td.MeanPosition()) * 3.0)
		if topicPos <= 0.1 {
			continue
		}
		a, ok := assoc[topic]
		if !ok || a == nil {
			continue
		}
		d, ok := a[tok]
		if !ok {
			continue
		}
		wordPos := math.Exp(-math.Abs(s.Position-d.MeanPosition()) * 4.0)
		nameScore += math.Sqrt(float64(d.Count)) * topicPos * wordPos
	}
	if nameScore > 0 {
		logit += w.NameAssoc * math.Tanh(nameScore/3.0)
	}

	// Staying inside the topic the sentence started in.
	if s.CurrentTopic != "" {
		if a := assoc[s.CurrentTopic]; a != nil {
			if _, ok := a[tok]; ok {
				logit += w.CurrentTopic
			}
		}
	}

	// Global significance. Squashed, so a word seen ten thousand times does not
	// outrank the model.
	if n := g.corpus.TopicCount(tok); n > 0 {
		logit += w.Significance * math.Tanh(math.Sqrt(float64(n))/12.0)
	}

	// A learned display name.
	if g.corpus.IsName(tok) {
		logit += w.IsName
	}

	// Persona vocabulary.
	if s.Persona == PersonaRoast {
		if strength, ok := roastLexicon[tok]; ok {
			logit += w.Persona * strength
		}
	}

	// Prompt echo and recent conversation.
	if _, ok := s.PromptSet[tok]; ok {
		logit += g.params.PromptRelevance
	}
	if _, ok := s.RecentSet[tok]; ok {
		logit += w.RecentContext
	}

	// Prompt gravity: similarity between the sentence fragment and the prompt.
	if s.Prompt != "" {
		fragment := strings.Join(s.Prefix, " ") + " " + tok
		if sim := text.Similarity(fragment, s.Prompt); sim > 0.05 {
			logit += w.PromptGravity * sim
		}
	}

	// Repetition. Gentle on purpose: memetic repetition is the register, so this is
	// meant to suppress stuttering rather than to flatten a deliberate cadence.
	if n := s.Used[tok]; n > 0 {
		p := w.Repetition * float64(n)
		if p < w.RepetitionCap {
			p = w.RepetitionCap
		}
		logit += p
	}
	for i := len(s.Sentence) - 1; i >= 0 && i >= len(s.Sentence)-5; i-- {
		if s.Sentence[i] == tok {
			logit += w.ImmediateRepeat
			break
		}
	}
	if len(s.Prefix) > 0 {
		if _, found := s.Ngrams[lastN(s.Prefix, 1)+" "+tok]; found {
			logit += w.BigramRepeat
		}
		if len(s.Prefix) >= 2 {
			if _, found := s.Ngrams[lastN(s.Prefix, 2)+" "+tok]; found {
				logit += w.TrigramRepeat
			}
		}
	}

	// The end sentinel, penalized until the sentence has reached its floor. The one
	// competing length mechanism kept in M7a; M7b replaces all of them with a single
	// length model.
	if tok == EndToken && len(s.Sentence) < s.MinWords {
		logit += w.EndEarly
	}

	// Connectives: a nudge mid-sentence, a dampening right at the end. Both small,
	// and both were multiplicative constants before.
	if _, ok := connectives[tok]; ok {
		switch {
		case s.Position > 0.85:
			logit -= w.Connective
		case s.Position > 0.2 && s.Position < 0.8:
			logit += w.Connective
		}
	}

	return logit
}

// lastN joins the final n words of a slice.
func lastN(words []string, n int) string {
	if len(words) < n {
		n = len(words)
	}
	return strings.Join(words[len(words)-n:], " ")
}
