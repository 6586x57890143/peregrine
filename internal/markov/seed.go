package markov

import (
	"math"
	"sort"
	"strings"

	"github.com/6586x57890143/peregrine/internal/corpus"
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
// These are the legacy tier weights preserved deliberately: this milestone changes
// WHERE the answers come from, not how strongly each tier is trusted, so that a change
// in output can be attributed to the two-hop tier rather than to a simultaneous
// retuning. The one exception is that the cluster tier's 50.0 has no successor; the
// two-hop tier deliberately enters LOW, below every direct-association tier, because a
// second-hop association is a weaker claim than a first-hop one.
const (
	// weightPromptNgram is scaled by n, so a longer prompt n-gram outranks a shorter
	// one. This is the strongest tier and should be: it is the only one that knows
	// what the user actually said.
	weightPromptNgram = 30.0

	weightNameTopic      = 25.0
	weightTopicWord      = 18.0
	weightNamePositional = 10.0
	weightPromptWord     = 15.0

	// weightTwoHop replaces the cluster tier. Low on purpose, per the note above.
	weightTwoHop = 6.0

	// weightRecent is the fallback floor, scaled by n like the prompt tier.
	weightRecent = 1.0
)

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

// SeedInput is everything seed selection needs from the caller.
type SeedInput struct {
	// PromptWords are the tokenized prompt, in order, already normalized.
	PromptWords []string

	// RecentWords are the decayed conversation context, the weakest tier.
	RecentWords []string

	// Names are the canonical recognized names in the prompt.
	Names []string
}

// Seed picks a starting prefix, or returns "" when the corpus offers nothing.
//
// An empty return is the caller's problem to handle and deliberately not a fallback
// string: the caller knows whether it would rather fall back to a prompt word or say
// nothing, and this does not.
func (g *Generator) Seed(in SeedInput) string {
	cands := map[string]float64{}
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

	// Tier 1: multi-word n-grams from the prompt, longest first.
	for n := maxN; n >= 1; n-- {
		for i := 0; i+n <= len(in.PromptWords); i++ {
			key := strings.Join(in.PromptWords[i:i+n], " ")
			if g.corpus.HasSuccessors(key) {
				add(key, float64(n)*weightPromptNgram)
			}
		}
	}

	// Tier 2: topics associated with a recognized name.
	for _, name := range in.Names {
		assoc, err := g.corpus.NameTopicsFor(name)
		if err != nil {
			continue
		}
		for topic, d := range assoc {
			add(topic, weightNameTopic+math.Sqrt(float64(d.Count)))
		}
	}

	// Tier 3: words that co-occur with a prompt word.
	for _, word := range in.PromptWords {
		assoc, err := g.corpus.TopicWordsFor(word)
		if err != nil {
			continue
		}
		for other, d := range assoc {
			if other != word && d.Count > 1 {
				add(other, weightTopicWord+math.Sqrt(float64(d.Count)))
			}
		}
	}

	// Tier 4: topics of the most recent name, restricted to ones the chain can continue
	// from. Narrower than tier 2 and weighted lower, which is why both exist.
	if len(in.Names) > 0 {
		last := in.Names[len(in.Names)-1]
		if assoc, err := g.corpus.NameTopicsFor(last); err == nil {
			for topic, d := range assoc {
				if g.corpus.HasSuccessors(topic) {
					add(topic, weightNamePositional+math.Sqrt(float64(d.Count)))
				}
			}
		}
	}

	// Tier 5: single prompt words.
	for _, word := range in.PromptWords {
		if g.corpus.HasSuccessors(word) {
			add(word, weightPromptWord)
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

	return g.drawSeed(cands)
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
				w := weightTwoHop + twoHopDecay*math.Sqrt(float64(second.count))
				if existing, ok := out[second.word]; !ok || w > existing {
					out[second.word] = w
				}
			}
		}
	}
	return out
}

// assocEntry is one co-occurrence, flattened for sorting.
type assocEntry struct {
	word  string
	count uint64
}

// topAssoc returns a word's strongest associations, highest count first.
//
// It merges the topic-word and name-topic indexes, because a source may be either an
// ordinary word or a person's name and the caller does not need to know which. Sorted
// with a token tie-break so the walk is deterministic: an unstable order here would
// make the golden samples irreproducible, which is the whole reason they are useful.
func (g *Generator) topAssoc(word string, limit int) []assocEntry {
	merged := map[string]uint64{}

	if assoc, err := g.corpus.TopicWordsFor(word); err == nil {
		for other, d := range assoc {
			if d.Count >= twoHopMinCount {
				merged[other] = d.Count
			}
		}
	}
	if assoc, err := g.corpus.NameTopicsFor(word); err == nil {
		for other, d := range assoc {
			if d.Count >= twoHopMinCount && d.Count > merged[other] {
				merged[other] = d.Count
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}

	out := make([]assocEntry, 0, len(merged))
	for w, c := range merged {
		out = append(out, assocEntry{word: w, count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
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

// Jump finds a related word when generation dead-ends mid-sentence.
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
func (g *Generator) Jump(in SeedInput, sentence []string) string {
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
			return best
		}
	}

	for i := len(context) - 1; i >= 0; i-- {
		if best := g.bestPivot(context[i], used, false); best != "" {
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
