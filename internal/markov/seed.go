package markov

import (
	"math"
	"sort"
	"strings"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/text"
)

// Seed selection: choosing where a sentence starts.
//
// This is a weighted draw over tiers, and the tiers are the ones the old legacy
// implementation had, with two changes.
//
// The first is that the concept-cluster tier is GONE and is replaced by a bounded
// two-hop expansion computed here, at query time. The cluster tier fired at weight
// 50.0, twice the next tier, applied to every member of a matching cluster. It had
// never once fired, because clusters were persisted string-keyed and decoded
// int-keyed, and when M7's scoping worked out what fixing that codec would have done
// the answer was: collapse every name-seed into one blob and hand that blob the
// highest-weight tier. See SPEC.md section 8, finding 29.
//
// The one thing clusters genuinely offered was TRANSITIVE association. The
// co-occurrence indexes answer "what appears near X"; they cannot answer "what appears
// near something that appears near X". That second hop is real, and it is worth
// exactly one bounded expansion tier rather than a persisted, stale, destructively
// rebuilt bucket with an unvalidated merge threshold. Computed per query it is never
// stale, needs no codec, and is a weight the golden samples can judge.
//
// The second change is that weights are additive in a log-space draw rather than raw
// multipliers in a linear one, matching the rest of the engine.

// Seed tier weights.
//
// These were the legacy weights, preserved through M7a so that a change in output could be
// attributed to the two-hop tier rather than to a simultaneous retuning. That reason has
// expired, and reading them against each other turned up two real problems, so the ladder is
// respaced here.
//
// FIRST, THE BONUSES WERE UNBOUNDED. Each association tier added a bare
// math.Sqrt(count) to its base weight, so a tier could climb straight out of its own band:
// at 50 recorded co-occurrences the name-topic tier reached 32, above the 30 a word the user
// actually typed gets. That is finding G2's shape in the seed selector rather than the
// scorer, and the scorer's own rule is the fix: every term that sums over evidence is
// squashed so it cannot exceed its own weight. assocSpread is that cap here, so each tier
// occupies a band it cannot leave and the ordering below is the ordering that happens.
//
// SECOND, ONE TIER WAS DEAD. weightPromptWord added single prompt words at 15.0, but tier 1
// runs n down to 1 and had already added exactly those keys at 30.0, and add() keeps the
// maximum. It could never win. Deleted rather than fixed: the question it asked was already
// answered one tier up, which is finding 28's shape.
const (
	// weightPromptNgram is scaled by n, so a longer prompt n-gram outranks a shorter
	// one, and a single prompt word enters here at 30.0. This is the tier that knows what
	// the user actually said.
	weightPromptNgram = 30.0

	// weightNameSeed starts the sentence AT the person rather than near them.
	//
	// ABOVE a single prompt word (30.0) and below a two-word prompt phrase (60.0), which is
	// the whole of the reasoning: who a message is about is more specific than any one word
	// in it, but a phrase somebody actually typed carries what they said AND who they meant.
	//
	// It has to clear 30.0 to do anything at all, because a recognized name is usually also a
	// prompt word and would otherwise be dominated by tier 1 for the same token, exactly as
	// weightPromptWord was. A weight below the tier that already covers your keys is not a
	// weak preference, it is no preference.
	weightNameSeed = 40.0

	// The association tiers, strongest claim first. What a name is about beats what a word
	// co-occurs with, which beats where a name's topics tend to sit, which beats a
	// second-hop association. Each spans [base, base+assocSpread).
	weightNameTopic      = 18.0
	weightTopicWord      = 12.0
	weightNamePositional = 8.0

	// weightTwoHop replaces the cluster tier. Lowest of the association tiers on purpose: a
	// transitive claim is weaker than a direct one even before the decay applies.
	weightTwoHop = 4.0

	// weightRecent is the fallback floor, scaled by n like the prompt tier.
	weightRecent = 1.0

	// assocSpread is how far evidence and position may move a candidate WITHIN its tier.
	// Small enough that the bands do not touch, which is what makes the ladder above true
	// rather than nominal.
	assocSpread = 3.0

	// assocEvidenceShare splits assocSpread between how well attested an association is and
	// how well the word works as an OPENING. The remainder goes to position.
	assocEvidenceShare = 0.6
)

// assocWeight bounds a tier's within-tier bonus so it cannot escape the tier, and splits it
// between evidence and position.
//
// EVIDENCE is tanh of sqrt(count) rather than bare sqrt(count). The shape still rewards the
// first few co-occurrences steeply and flattens after, which is the right curve, but it
// converges instead of growing without limit. The divisor puts the knee around a dozen
// co-occurrences.
//
// POSITION prefers a word that tends to appear EARLY in a message, because this is choosing
// where a sentence starts. Seeding on a word that normally sits mid-clause is what produced
// replies like "loose in the server is doomed" and "is peak bird behaviour honestly" in the
// golden samples: grammatical, chain-coherent, and reading as though the first half went
// missing.
//
// Note the deliberate contrast with bestPivot, which prefers MID-sentence words by the same
// data. That is not an inconsistency: a jump is choosing where to continue from, and a word
// that normally opens sentences reads as a restart there, while a word that normally closes
// them ends the sentence abruptly. Opening and continuing want opposite ends of the same
// number.
func assocWeight(base float64, d corpus.TopicAssoc) float64 {
	evidence := math.Tanh(math.Sqrt(float64(d.Count)) / 4.0)
	early := 1.0 - d.MeanPosition()
	return base + assocSpread*(assocEvidenceShare*evidence+(1.0-assocEvidenceShare)*early)
}

// Two-hop bounds. Constants rather than knobs for the same reason as the enumeration
// caps: they trade latency for reach in a way an operator has no instrument to judge.
const (
	// twoHopFirst is how many first-hop associations of each source word are followed,
	// highest count first.
	twoHopFirst = 6

	// twoHopSecond is how many second-hop associations are taken from each of those.
	twoHopSecond = 4

	// twoHopMinCount is the count a first-hop association needs before it is worth
	// following. 1 is noise: a single co-occurrence in one message says nothing, and
	// following it multiplies that nothing by twoHopSecond.
	twoHopMinCount = 2

	// twoHopDecay multiplies the weight at the second hop, so a transitive claim is
	// worth less than a direct one even before the tier weight applies.
	twoHopDecay = 0.5
)

// maxJumpsPerSentence bounds how many seams one sentence may contain. See Jump.
const maxJumpsPerSentence = 1

// SeedInput is everything seed selection needs from the caller.
type SeedInput struct {
	// PromptWords are the tokenized prompt, in order, already normalized.
	PromptWords []string

	// RecentWords are the decayed conversation context, the weakest tier.
	RecentWords []string

	// Names are the canonical recognized names in the prompt.
	Names []string

	// NameTokens is every SPELLING of a recognized name that appeared in the prompt, surface
	// form and canonical form both.
	//
	// Separate from Names rather than folded into it, because the two are read differently:
	// Names is looked up in name_topic, where only canonical forms are keys, so putting an
	// alias there would buy a guaranteed miss per lookup. This is looked up in the n-gram
	// index, where whichever spelling people actually type is what got learned.
	NameTokens []string
}

// Seed picks a starting prefix, or returns "" when the corpus offers nothing.
//
// An empty return is the caller's problem to handle and deliberately not a fallback
// string: the caller knows whether it would rather fall back to a prompt word or say
// nothing, and this does not.
func (g *Generator) Seed(in SeedInput) string {
	cands := map[string]float64{}

	// The candidates that came from what the user typed. Exempt from the author-diversity
	// gate below, because echoing somebody's own words back is not poisoning.
	fromPrompt := map[string]struct{}{}

	add := func(key string, weight float64) {
		if key == "" || weight <= 0 {
			return
		}
		// Highest wins rather than accumulating. A word that qualifies under two tiers
		// is not twice as good a seed, and summing would let the low tiers gang up on a
		// prompt n-gram, which is the one tier that knows what the user said.
		if existing, ok := cands[key]; !ok || weight > existing {
			cands[key] = weight
		}
	}

	maxN := max(g.params.MaxNGram-1, 1)

	// Tier 1: n-grams from the prompt, longest first, and single prompt words at n == 1.
	for n := maxN; n >= 1; n-- {
		for i := 0; i+n <= len(in.PromptWords); i++ {
			key := strings.Join(in.PromptWords[i:i+n], " ")

			// A LONE FUNCTION WORD IS NOT A SEED. Starting a reply on "is" or "of" or "the"
			// can only produce something that reads as though its first half went missing:
			// the golden samples had "is peak bird behaviour honestly" and "what it did".
			//
			// Only this tier can do it, because the association indexes exclude stop words on
			// the write side, so this is the one place the check belongs. Multi-word keys are
			// exempt deliberately: "the bird" and "the server is" open perfectly well, and it
			// is the word standing alone that is the problem.
			if n == 1 && text.IsStopWord(key) {
				continue
			}

			if g.corpus.HasSuccessors(key) {
				add(key, float64(n)*weightPromptNgram)
				fromPrompt[key] = struct{}{}
			}
		}
	}

	// Tier 2: the recognized name ITSELF, so a reply can start at the person rather than only
	// somewhere near them.
	//
	// Exempt from the author-diversity gate, like the other prompt-derived tiers, and for a
	// reason that is worth stating because a safety exemption should never be inherited by
	// analogy alone: this contributes ONE token, and every step after it is still filtered by
	// eligible(). Repeating a username teaches the bot no sentence, so there is no poisoning
	// vector to close here. Without the exemption the tier would be dead on exactly the young
	// corpus that needs it most, since almost nothing has two distinct authors yet.
	for _, token := range in.NameTokens {
		if g.corpus.HasSuccessors(token) {
			add(token, weightNameSeed)
			fromPrompt[token] = struct{}{}
		}
	}

	// Tier 3: topics associated with a recognized name.
	for _, name := range in.Names {
		assoc, err := g.corpus.NameTopicsFor(name)
		if err != nil {
			continue
		}
		for topic, d := range assoc {
			add(topic, assocWeight(weightNameTopic, d))
		}
	}

	// Tier 4: words that co-occur with a prompt word.
	for _, word := range in.PromptWords {
		assoc, err := g.corpus.TopicWordsFor(word)
		if err != nil {
			continue
		}
		for other, d := range assoc {
			if other != word && d.Count > 1 {
				add(other, assocWeight(weightTopicWord, d))
			}
		}
	}

	// Tier 5: topics of the most recent name, restricted to ones the chain can continue
	// from. Narrower than tier 2 and weighted lower, which is why both exist.
	if len(in.Names) > 0 {
		last := in.Names[len(in.Names)-1]
		if assoc, err := g.corpus.NameTopicsFor(last); err == nil {
			for topic, d := range assoc {
				if g.corpus.HasSuccessors(topic) {
					add(topic, assocWeight(weightNamePositional, d))
				}
			}
		}
	}

	// Tier 6: the two-hop expansion, replacing the concept-cluster tier.
	for word, weight := range g.twoHop(in) {
		add(word, weight)
	}

	// Tier 7: recent conversation, the floor.
	for n := maxN; n >= 1; n-- {
		for i := 0; i+n <= len(in.RecentWords); i++ {
			key := strings.Join(in.RecentWords[i:i+n], " ")
			if g.corpus.HasSuccessors(key) {
				add(key, float64(n)*weightRecent)
			}
		}
	}

	return g.drawAttestedSeed(cands, fromPrompt)
}

// twoHop returns transitively associated words with decayed weights.
//
// Sources are the prompt words and the recognized names, because those are what the
// message is actually about. Both hops read the same two co-occurrence indexes the
// direct tiers read, so this adds no storage and cannot go stale.
//
// Every candidate must pass HasSuccessors. A second-hop word the chain cannot continue
// from is worse than no seed at all: it produces a one-word reply, which reads as the
// bot malfunctioning rather than as a short joke.
func (g *Generator) twoHop(in SeedInput) map[string]float64 {
	out := map[string]float64{}

	sources := make([]string, 0, len(in.PromptWords)+len(in.Names))
	sources = append(sources, in.PromptWords...)
	sources = append(sources, in.Names...)

	seen := map[string]struct{}{}
	for _, s := range sources {
		seen[s] = struct{}{}
	}

	for _, src := range sources {
		for _, first := range g.topAssoc(src, twoHopFirst) {
			for _, second := range g.topAssoc(first.word, twoHopSecond) {
				if _, isSource := seen[second.word]; isSource {
					continue
				}
				if second.word == first.word {
					continue
				}
				if !g.corpus.HasSuccessors(second.word) {
					continue
				}
				w := assocWeight(weightTwoHop, second.assoc) * twoHopDecay
				if existing, ok := out[second.word]; !ok || w > existing {
					out[second.word] = w
				}
			}
		}
	}
	return out
}

// assocEntry is one co-occurrence, flattened for sorting.
//
// It carries the whole TopicAssoc rather than just the count, so the two-hop tier weighs
// position the same way the direct tiers do. Carrying only the count here was what made the
// weakest tier the one exception to that rule, for no reason beyond it being written first.
type assocEntry struct {
	word  string
	assoc corpus.TopicAssoc
}

// topAssoc returns a word's strongest associations, highest count first.
//
// It merges the topic-word and name-topic indexes, because a source may be either an
// ordinary word or a person's name and the caller does not need to know which. Sorted
// with a token tie-break so the walk is deterministic: an unstable order here would
// make the golden samples irreproducible, which is the whole reason they are useful.
func (g *Generator) topAssoc(word string, limit int) []assocEntry {
	merged := map[string]corpus.TopicAssoc{}

	if assoc, err := g.corpus.TopicWordsFor(word); err == nil {
		for other, d := range assoc {
			if d.Count >= twoHopMinCount {
				merged[other] = d
			}
		}
	}
	if assoc, err := g.corpus.NameTopicsFor(word); err == nil {
		for other, d := range assoc {
			if d.Count >= twoHopMinCount && d.Count > merged[other].Count {
				merged[other] = d
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}

	out := make([]assocEntry, 0, len(merged))
	for w, d := range merged {
		out = append(out, assocEntry{word: w, assoc: d})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].assoc.Count != out[j].assoc.Count {
			return out[i].assoc.Count > out[j].assoc.Count
		}
		return out[i].word < out[j].word
	})

	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// drawSeed makes the weighted draw.
//
// Deterministic iteration order before the draw, because Go's map iteration is
// randomized and a draw whose candidate order varies is not reproducible under a
// seeded source even though it is still correctly weighted. M6b had to delete a
// heuristic over exactly this class of dependency.
func (g *Generator) drawSeed(cands map[string]float64) string {
	if len(cands) == 0 {
		// No tier matched. The absolute fallback is any real prefix, because a prefix
		// with continuations beats a sentinel with none.
		if prefix, ok := g.corpus.FirstPrefix(); ok {
			return prefix
		}
		return ""
	}

	keys := make([]string, 0, len(cands))
	var total float64
	for k, w := range cands {
		keys = append(keys, k)
		total += w
	}
	sort.Strings(keys)

	if total <= 0 {
		return keys[0]
	}
	r := g.src.Float64() * total
	for _, k := range keys {
		r -= cands[k]
		if r <= 0 {
			return k
		}
	}
	return keys[len(keys)-1]
}

// drawAttestedSeed draws a seed and rejects one the author-diversity gate would refuse,
// redrawing a bounded number of times.
//
// The gate has to reach the seed for the same reason it has to reach the jump: the seed is a
// token the bot emits, and the corpus tiers (name-topic, topic-word, two-hop, recent) are
// built from co-occurrence indexes that carry no author attribution at all. Without this, a
// phrase one person repeated could be refused by the sampler at every step and still be the
// first word of the reply.
//
// prompt is the set of candidates that came from what the USER typed, and those are exempt.
// Echoing somebody's own words back is not poisoning, and refusing to seed from the prompt
// would make the bot least responsive exactly when it was addressed directly.
//
// Checked on the DRAWN candidate rather than filtered up front, deliberately: attestation is a
// Successors scan, and there can be hundreds of candidates on a busy prompt. A bounded redraw
// costs a handful of scans on the reply path where filtering would cost one per candidate.
func (g *Generator) drawAttestedSeed(cands map[string]float64, prompt map[string]struct{}) string {
	if g.params.MinDistinctAuthors <= 0 {
		return g.drawSeed(cands)
	}

	// Enough attempts to get past a few unattested candidates, few enough that a corpus
	// where nothing qualifies does not turn every reply into a scan of it. Falling through
	// to "" means the caller falls back to a prompt word or stays silent, which is the same
	// outcome an empty corpus produces.
	const attempts = 8
	for range attempts {
		seed := g.drawSeed(cands)
		if seed == "" {
			return ""
		}
		if _, fromPrompt := prompt[seed]; fromPrompt {
			return seed
		}
		// A multi-word seed is a stored prefix, and its own continuations are what the
		// sampler will gate at the first step, so attestation of the whole phrase is what
		// matters rather than of one token in it.
		if g.attested(seed) {
			return seed
		}
		delete(cands, seed)
		if len(cands) == 0 {
			return ""
		}
	}
	return ""
}

// Jump finds a related word when generation dead-ends mid-sentence, or returns "" when
// jumping would cost more than it buys.
//
// Same machinery as the two-hop expansion rather than a second implementation, which
// is the other half of deleting the cluster path: legacy had a cluster-based pivot and
// a topic-based pivot as separate code, and the cluster one had never fired. What is
// left is one question, "what is associated with what we have been talking about",
// asked at a different moment.
//
// The positional preference is kept from the old implementation: a jump targets a word
// whose usual position is near the middle of a sentence, because jumping to a word that
// normally ends sentences produces an abrupt stop and one that normally starts them
// reads as a restart.
//
// # Why this is bounded, and why the bounds live here
//
// A jump appends a word with NO n-gram relationship to the word before it: it comes out
// of the co-occurrence indexes, not out of the chain. So the join is not occasionally
// rough, it is guaranteed rough, and in live output it was the signature of every
// unreadable line while the purely chain-generated ones landed fine:
//
//	what's the point of even a | know i do that
//	back to the | go
//	u just what if you dont have | get the raped
//
// On a young corpus this is not a rare fallback either. The author-diversity gate empties
// the candidate set constantly, so dead ends are frequent and the jump was a major
// producer of words. It also worked against length.go's own stated principle, that short
// and punchy reads as a joke and long reads as a malfunction: it bought length with
// coherence, which is the wrong trade for a chat bot.
//
// The bounds are here rather than at the caller because there are two callers, production
// and the golden harness, and a harness that prints output the bot cannot produce is worse
// than no harness. One decision, one place.
//
// It takes the Step rather than a bare sentence so that all three bounds can be answered
// from what the engine already knows, and so the per-sentence count cannot drift out of
// sync with the sentence it counts.
func (g *Generator) Jump(in SeedInput, s *Step) string {
	sentence := s.Sentence

	// PAST THE LENGTH FLOOR, END INSTEAD. The jump exists to save a reply from being too
	// short to post, and Length.Min is the definition of too short, so that is the
	// threshold. Beyond it the length model already decided the sentence could stop, and
	// overriding that decision to add a guaranteed seam is a bad trade.
	if len(sentence) >= s.Length.Min {
		return ""
	}

	// ONE SEAM PER SENTENCE. One reads as a change of subject; two read as broken. This was
	// unbounded, so a sentence under the floor could jump at every single word.
	if s.Jumps >= maxJumpsPerSentence {
		return ""
	}

	// NOT AFTER A FUNCTION WORD. A determiner or a preposition demands a specific kind of
	// continuation, so a jump there cannot read as anything but a fault: "back to the" + "go".
	// After a content word the same jump reads as changing the subject.
	//
	// The jump TARGET is already never a stop word, because the association writers exclude
	// them, so this is about the word being jumped FROM, which nothing checked.
	if len(sentence) > 0 && text.IsStopWord(sentence[len(sentence)-1]) {
		return ""
	}

	used := make(map[string]struct{}, len(sentence))
	for _, w := range sentence {
		used[w] = struct{}{}
	}

	// Context is the prompt plus the tail of what has been generated, most recent last.
	const maxContext = 5
	tail := sentence
	if len(tail) > maxContext {
		tail = tail[len(tail)-maxContext:]
	}
	context := make([]string, 0, len(in.PromptWords)+len(tail)+len(in.Names))
	context = append(context, in.PromptWords...)
	context = append(context, tail...)

	// Names first and most recent first, since a name is the strongest thing a reply
	// can be about.
	for i := len(in.Names) - 1; i >= 0; i-- {
		if best := g.bestPivot(in.Names[i], used, true); best != "" {
			s.Jumps++
			return best
		}
	}

	for i := len(context) - 1; i >= 0; i-- {
		if best := g.bestPivot(context[i], used, false); best != "" {
			s.Jumps++
			return best
		}
	}
	return ""
}

// bestPivot picks the association of one source word that is best positioned to
// continue a sentence.
//
// nameSource selects which index to read: a name's associations live in name_topic and
// an ordinary word's in topic_word. Reading both for every source would double the
// lookups on a path that is already a fallback.
func (g *Generator) bestPivot(source string, used map[string]struct{}, nameSource bool) string {
	var assoc map[string]assocPos
	if nameSource {
		a, err := g.corpus.NameTopicsFor(source)
		if err != nil {
			return ""
		}
		assoc = toAssocPos(a)
	} else {
		a, err := g.corpus.TopicWordsFor(source)
		if err != nil {
			return ""
		}
		assoc = toAssocPos(a)
	}

	best := ""
	bestScore := 0.0
	for word, d := range assoc {
		if word == source || d.count <= 1 {
			continue
		}
		if _, seen := used[word]; seen {
			continue
		}
		// A jump the chain cannot continue from ends the sentence on the jump word,
		// which is worse than ending it one word earlier.
		if !g.corpus.HasSuccessors(word) {
			continue
		}
		// THE AUTHOR-DIVERSITY GATE APPLIES HERE TOO, and this was a hole.
		//
		// The gate lives in eligible(), which filters the sampler's candidates. Jump is a
		// SECOND producer of words: on a dead end it picks a token out of the co-occurrence
		// indexes and appends it to the sentence directly, and those indexes carry no author
		// attribution at all. So a phrase one person repeated forty times was refused by the
		// sampler and then handed back by the jump, which is finding A6 defeated by the exact
		// shape design principle 3 exists to prevent: a check at one of two producers.
		//
		// It went unnoticed because the test that would have caught it seeded the corpus with
		// no mentioned users, and both association indexes are gated on a name being present,
		// so the jump had nothing to find. Found in M11c when that fixture became realistic.
		if !g.attested(word) {
			continue
		}

		// Association strength, discounted by how far the word's usual position is from
		// the middle. Ties broken by token so the choice is deterministic.
		posScore := 1.0 - math.Abs(d.mean-0.5)
		score := math.Sqrt(float64(d.count)) * posScore
		if score > bestScore || (score == bestScore && word < best) {
			bestScore = score
			best = word
		}
	}
	return best
}

// assocPos is the pair of numbers the pivot cares about, flattened out of
// corpus.TopicAssoc so bestPivot can treat both indexes identically.
type assocPos struct {
	count uint64
	mean  float64
}

func toAssocPos(in map[string]corpus.TopicAssoc) map[string]assocPos {
	out := make(map[string]assocPos, len(in))
	for k, v := range in {
		out[k] = assocPos{count: v.Count, mean: v.MeanPosition()}
	}
	return out
}

// attested reports whether a word is one the corpus has seen several people use, which is
// what the author-diversity gate means for a token that is not a candidate continuation.
//
// The gate is naturally a property of an EDGE: ngram_auth records (prefix, next, author), so
// "how many distinct people said this continuation" is answerable and "how many distinct
// people said this word" is not, without a reverse index the layout deliberately does not
// carry. The closest honest question is whether the chain can leave this word along an edge
// that several people used, and that is what this asks.
//
// It is a filter and it does not relax, for the same reason eligible() does not: a control
// that yields the moment it has an effect is not a control. When it refuses everything the
// jump simply fails and the sentence ends, which is the same outcome a dead end already has.
//
// Cost: one Successors scan, on a path that runs at most once per dead end per sentence, and
// only for candidates that have already passed the cheaper checks above.
func (g *Generator) attested(word string) bool {
	min := g.params.MinDistinctAuthors
	if min <= 0 {
		return true
	}
	succ, err := g.corpus.Successors(word)
	if err != nil {
		return false
	}
	for _, s := range succ {
		// The end sentinel is exempt in eligible() because gating a sentence's ability to end
		// on how other people ended theirs is a length bug wearing a safety hat. It is NOT
		// exempt here: "the only thing following this word is the end of a message somebody
		// once sent" is not evidence that several people use the word.
		if s.Token != EndToken && s.Authors >= uint32(min) {
			return true
		}
	}
	return false
}

// TrimDangling removes trailing function words from a sentence that stopped because it ran
// out of chain rather than because it chose to end.
//
// The two endings are not the same claim, which is why this takes the caller's word for
// which one happened rather than guessing. An end sentinel means somebody really did finish
// a message at that word, and in this register that is worth keeping even when it looks
// abrupt. A dead end means the chain simply had nowhere to go, and stopping on "back to
// the" or "what's the point of even a" reads as the bot being cut off mid-thought, which is
// the same class of damage as the seam Jump was making.
//
// Same asymmetry attested() already makes about EndToken: what the corpus witnessed somebody
// do is evidence, and what generation merely ran into is not.
//
// It will trim a sentence down to nothing if that is all it was, and that is fine: the
// caller has a floor below which it posts nothing at all.
func TrimDangling(sentence []string) []string {
	end := len(sentence)
	for end > 0 && text.IsStopWord(sentence[end-1]) {
		end--
	}
	return sentence[:end]
}
