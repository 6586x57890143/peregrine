package markov

import (
	"math"
	"strings"

	"github.com/6586x57890143/peregrine/internal/corpus"
)

// EndToken is the sentinel the corpus stores for the end of a message.
//
// Lower case, and that matters: the previous scorer contained a branch comparing
// against "<END>" while the only sentinel ever produced was "<end>", guarded by a
// character-length check on a word-joined string. It was dead in both halves
// (SPEC.md section 5.1). Exported so the caller and the engine cannot disagree about
// the spelling again.
const EndToken = "<end>"

// Persona, the roast lexicon and the filler sets live in persona.go, because the
// in-sampler bias and the post-pass are one mechanism and splitting them across files
// is how they became two mechanisms in the first place.

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

	// PromptSet is the normalized set of words the user actually typed.
	PromptSet map[string]struct{}

	// RecentWeights is the conversation context, token to how recent it is in [0, 1].
	//
	// GRADED rather than a set, which is finding 48. It was a set, so RecentContext at 0.25
	// paid a word from the fiftieth-oldest message exactly what it paid one from the message
	// immediately before this reply. The weighting existed upstream and was collapsed away
	// here. Because the value is already bounded by construction the term needs no tanh.
	RecentWeights map[string]float64

	// Used counts how often each word has been emitted in this sentence, and Ngrams
	// holds the 1, 2 and 3-grams already produced.
	Used   map[string]int
	Ngrams map[string]struct{}

	// Position is progress through the sentence, 0 at the start and 1 at the target.
	//
	// IT IS ON THE SAME SCALE AS corpus.TopicAssoc.MeanPosition, which is a fraction of a
	// message, and every consumer below compares the two directly. Set it from
	// Length.Progress and from nothing else: both walk loops used to divide by Length.Max,
	// which is a different scale entirely, and the invariant was violated precisely because
	// it was never written down here. Length.Progress carries the full account.
	Position float64

	// Length is the one length model: floor, cap and the target sampled once for this
	// sentence. It replaces the three competing mechanisms described in length.go, so
	// there is no MinWords field here any more and no discard-and-retry in the caller.
	Length Length

	// Persona selects the vocabulary bias and, at the end, the filler set.
	Persona Persona

	// CoreTopics are the prompt's non-stop words weighted by global significance,
	// and CurrentTopic is the topic the sentence started in.
	CoreTopics   map[string]float64
	CurrentTopic string

	// NameAssoc aggregates the topic associations of every recognized name in the
	// prompt.
	NameAssoc map[string]corpus.TopicAssoc

	// PromptNames is every spelling of a name the PROMPT named, so the scorer can tell
	// the person under discussion apart from any other person the corpus knows.
	PromptNames map[string]struct{}

	// Jumps counts the dead-end jumps this sentence has already taken. Jump reads it and
	// increments it, so the count cannot drift away from the sentence it describes.
	Jumps int

	// Trace collects what the walk did, for the tuning export, or is nil.
	//
	// Here rather than on the Generator because the Generator holds no per-sentence state
	// and is shared by every message goroutine: a counter on it would be a data race on the
	// hottest path in the bot, which is finding 3 rebuilt deliberately. Nil is the normal
	// case and every method on Trace guards its own receiver, so tracing off is one branch
	// per step and no allocation.
	Trace *Trace
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
		// No continuation at any order. Distinct from the starved case below, and the
		// distinction is the whole reason the trace splits them: this is a sparse corpus,
		// that is the author-diversity gate ending the sentence.
		s.Trace.deadEnd(false, 0)
		return "", nil
	}

	// Counted before the filter, because eligible reuses the backing array and the count is
	// gone the moment it returns.
	before := len(cands)
	cands = g.eligible(cands)
	refused := before - len(cands)
	if len(cands) == 0 {
		s.Trace.deadEnd(true, refused)
		return "", nil
	}
	s.Trace.step(m.minCtxWords, len(cands), refused)

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
	if g.params.MinDistinctAuthors <= 0 {
		return cands
	}

	out := cands[:0]
	for _, c := range cands {
		if g.admits(c.token, c.authors) {
			out = append(out, c)
		}
	}
	return out
}

// admits is the gate's decision for one continuation, factored out so that candidate
// enumeration can count SURVIVORS rather than raw candidates.
//
// Splitting it out is not tidiness. The backoff floor used to be measured on the
// unfiltered pool, so enumeration stopped as soon as five continuations existed and the
// gate then deleted three or four of them: measured against the golden fixture, a prefix
// that stopped at the floor left the sampler choosing between TWO candidates, while one
// that happened to back off further left fourteen. Top-k of 40 and any temperature at all
// are meaningless against a set of two, which is how the engine went back to being argmax
// with noise in exactly the places the floor was supposed to prevent it.
//
// Note what this does NOT do, because it touches a safety control: every candidate still
// has to pass the gate. Enumeration keeps LOOKING at lower orders until enough survivors
// exist; it never admits one that failed. The gate still does not relax when it bites, and
// when no order supplies survivors the step is still a dead end.
func (g *Generator) admits(token string, authors uint32) bool {
	min := g.params.MinDistinctAuthors
	if min <= 0 {
		return true
	}
	// The end sentinel is structural rather than content: gating it on author diversity
	// would mean a sentence cannot end until several people have ended a message the same
	// way, which is a length bug wearing a safety hat.
	return token == EndToken || authors >= uint32(min)
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

	// THE PERSON'S OWN VOCABULARY, which had no logit at all (SPEC.md section 8, finding 42).
	//
	// s.NameAssoc's keys ARE the words the corpus has seen people use about this person. The
	// block below walks a SECOND hop out of them, looking up each key in TopicWordsFor and
	// scoring the candidate for co-occurring with that, and until now the second hop was the
	// only name-association term there was. So "cope" being what the server says about greg
	// moved no candidate, while a word that merely co-occurs with "cope" did.
	//
	// Compare TopicGravity above: that is the same one-hop question asked of a prompt word,
	// at 0.70. A name is more specific than any one word in the message, so this sits just
	// above it, and below PromptName at 0.90 because naming the person is more direct still.
	//
	// Costs no I/O: NameAssoc is already on the Step, and this is a map lookup.
	if d, ok := s.NameAssoc[tok]; ok {
		pos := math.Exp(-math.Abs(s.Position-d.MeanPosition()) * 4.0)
		if e := math.Sqrt(float64(d.Count)) * pos; e > 0 {
			logit += w.NameTopic * math.Tanh(e/3.0)
		}
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

	// THE PERSON THE MESSAGE WAS ABOUT, which is a different claim from the one above.
	// IsName rewards every learned name equally, so a reply to "what is up with lachy"
	// was as likely to name a bystander as to name lachy, and answering by naming
	// somebody else entirely is the failure this closes.
	//
	// Flat rather than summed over evidence, so no tanh: it is one token being either
	// the subject or not. It also cannot make the bot chant the name, because
	// ImmediateRepeat at -2.50 fires on anything seen in the last five words and swamps
	// this after the first emission.
	if _, ok := s.PromptNames[tok]; ok {
		logit += w.PromptName
	}

	// Persona vocabulary. One call, shared with the post-pass in persona.go, rather
	// than a lexicon rebuilt here per candidate.
	logit += g.lexiconBias(s.Persona, tok)

	// Prompt echo and recent conversation.
	if _, ok := s.PromptSet[tok]; ok {
		logit += g.params.PromptRelevance
	}
	if weight := s.RecentWeights[tok]; weight > 0 {
		logit += w.RecentContext * weight
	}

	// PROMPT GRAVITY IS GONE, and it was three things at once, none of them what it claimed
	// (SPEC.md section 8, finding 43).
	//
	// It computed a Jaccard index between the sentence so far plus this candidate, and the
	// prompt. Within one step the prefix is IDENTICAL for every candidate, so most of that
	// number was a constant, and a constant added to every logit cancels in the softmax. What
	// actually varied was two things. First, whether the candidate is a prompt word, worth
	// about 0.09 logits at typical sizes, which duplicates the PromptRelevance term four lines
	// up that pays 0.6 for exactly that. Second, whether the candidate is ALREADY in the
	// prefix, worth about 0.02, because a repeat does not grow the union: it paid a candidate
	// for doing the thing Repetition, ImmediateRepeat and BigramRepeat all exist to punish.
	//
	// It was also the most expensive term in the loop, joining the whole prefix and
	// re-tokenizing the entire prompt through a regex once per candidate per step.
	//
	// text.Similarity went with it, having no other caller. Its own doc comment described
	// rejecting a sentence that is too close to the prompt, which no caller has ever done, and
	// that gap is its own small evidence the term was never doing the job it was named for. A
	// parrot check, if one is ever wanted, belongs on the finished sentence where it is one
	// call rather than one per candidate.

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

	// The end sentinel, shifted by the length model. This is now the ONLY place length
	// influences generation: the discard-and-retry and the second progress-based
	// multiplier are gone, so there is one answer to "how long should this be" instead
	// of three that could disagree.
	if tok == EndToken {
		logit += s.Length.endLogit(w, len(s.Sentence))
	}

	// Connectives: a nudge mid-sentence, a dampening right at the end. Both small,
	// and both were multiplicative constants before.
	//
	// BOTH BRANCHES ARE REACHABLE NOW, which is new. Position used to be measured against
	// Length.Max, so "right at the end" meant word 16 of 18 against a median target of 7 and
	// the dampening had effectively never fired, while the mid-sentence nudge covered almost
	// the whole sentence. The term was therefore a flat bias toward "and/but/then/because"
	// in an engine whose length model exists to argue against run-ons. See Length.Progress.
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
