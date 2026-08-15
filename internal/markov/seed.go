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

// seedTier identifies where a candidate came from. HIGHEST PRECEDENCE FIRST: a key that
// qualifies under two tiers is counted in the earlier one.
//
// tierName precedes tierPromptNgram deliberately, and getting that order backwards would
// make the name tier empty on exactly the prompts it exists for: a recognized name is almost
// always also a prompt unigram, so the prompt tier would claim it and the name tier's budget
// would go unspent. That is finding 34's lesson restated for budgets, which said that a
// weight below the tier already covering your keys is not a weak preference but no
// preference.
type seedTier int

const (
	tierName seedTier = iota
	tierPromptNgram
	tierNameTopic
	tierTopicWord
	tierTwoHop
	tierRecent
	numSeedTiers
)

// seedBudget is the share of the draw each tier may spend, and it replaces the per-candidate
// weights this file used to carry.
//
// # Why budgets rather than weights (SPEC.md section 8, finding 36)
//
// drawSeed samples proportional to weight over ALL candidates, so a tier's influence is its
// weight TIMES ITS CANDIDATE COUNT, and the counts are wildly unequal. The prompt tier
// contributes one candidate per n-gram window, about eighteen for a seven-word prompt, while
// the name tier contributes exactly one. At the old weights that was 1350 against 40, so a
// recognized name won the draw 4.57% of the time against a documented position above every
// association tier. The old comment reasoned pairwise ("above a single prompt word at 30,
// below a two-word phrase at 60"), which is true of one candidate against one candidate and
// says nothing about the draw.
//
// The general form is worth keeping: A BOUNDED WEIGHT IS NOT A BOUNDED INFLUENCE WHEN THE
// NUMBER OF CANDIDATES CARRYING IT IS UNBOUNDED. That generalizes finding 34, which bounded
// a candidate within its tier, to bounding the tier itself.
//
// Read the numbers as a sentence: half the draw is what the user typed, a quarter is the
// person the message is about, an eighth is what people say about that person, and the rest
// is association and recency.
//
// AN EMPTY TIER SPENDS NOTHING, which makes this self-correcting as the corpus fills. With
// the association indexes starved, as they are in production today (finding 45), a
// name-bearing prompt gives the name tier 50/150 = 33%; once the indexes are populated the
// same constants give 50/200 = 25%. Roughly one reply in three now, settling to one in four,
// with no constant changing.
var seedBudget = [numSeedTiers]float64{
	tierName:        50.0,
	tierPromptNgram: 100.0,
	tierNameTopic:   25.0,
	tierTopicWord:   15.0,
	tierTwoHop:      6.0,
	tierRecent:      4.0,
}

// recalledNameDiscount is how much less a person the channel was recently discussing is worth
// than one this message actually named.
//
// Within the same tier rather than a tier of their own, so recall cannot outrank presence
// however faded the conversation is, and so adding it costs no budget from anything else.
const recalledNameDiscount = 0.5

// assocEvidenceShare splits a candidate's within-tier score between how well attested the
// association is and how well the word works as an OPENING. The remainder goes to position.
const assocEvidenceShare = 0.6

// assocScore is a candidate's score WITHIN its tier, in [1.0, 2.0].
//
// A RATIO NOW, NOT A BAND. Keeping candidates inside a band was assocSpread's job, and
// per-tier budgets took that job over completely: a candidate cannot leave a tier it is
// normalized into. So assocSpread is deleted rather than kept, and the deletion is not a
// regression of finding 34 but its generalization.
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
func assocScore(d corpus.TopicAssoc) float64 {
	evidence := math.Tanh(math.Sqrt(float64(d.Count)) / 4.0)
	early := 1.0 - d.MeanPosition()
	return 1.0 + assocEvidenceShare*evidence + (1.0-assocEvidenceShare)*early
}

// seedCands collects candidates per tier and normalizes each tier onto its budget.
type seedCands struct {
	byTier [numSeedTiers]map[string]float64 // key -> within-tier score
	tierOf map[string]seedTier
}

func newSeedCands() *seedCands {
	c := &seedCands{tierOf: map[string]seedTier{}}
	for i := range c.byTier {
		c.byTier[i] = map[string]float64{}
	}
	return c
}

// add records a candidate, keeping the highest-precedence tier that claims it and, within a
// tier, the highest score. Both rules match what the old flat add() did; only the accounting
// changed.
func (c *seedCands) add(t seedTier, key string, score float64) {
	if key == "" || score <= 0 {
		return
	}
	if prev, ok := c.tierOf[key]; ok {
		if prev < t {
			return
		}
		if prev > t {
			delete(c.byTier[prev], key)
		}
	}
	c.tierOf[key] = t
	if existing, ok := c.byTier[t][key]; !ok || score > existing {
		c.byTier[t][key] = score
	}
}

// drop removes a candidate entirely, for the attestation redraw.
func (c *seedCands) drop(key string) {
	if t, ok := c.tierOf[key]; ok {
		delete(c.byTier[t], key)
		delete(c.tierOf, key)
	}
}

// weights turns the per-tier scores into the absolute weights the draw uses.
//
// Each non-empty tier gets exactly its budget, split in proportion to within-tier scores.
// Recomputed after every drop rather than adjusted in place, which is what makes a rejected
// candidate's share go to NOBODY instead of being donated across every tier: if the name
// tier's only candidate fails attestation, the name tier should contribute zero, not hand a
// quarter of the draw to the prompt tier.
func (c *seedCands) weights() map[string]float64 {
	out := make(map[string]float64, len(c.tierOf))
	for t := range c.byTier {
		scores := c.byTier[t]
		if len(scores) == 0 {
			continue
		}
		var total float64
		for _, s := range scores {
			total += s
		}
		if total <= 0 {
			continue
		}
		budget := seedBudget[t]
		for k, s := range scores {
			out[k] = budget * s / total
		}
	}
	return out
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

	// RecentMessages is the conversation context, ONE ENTRY PER MESSAGE and oldest first.
	//
	// Per message rather than one flat slice, because this tier forms n-gram windows and a
	// window spanning two messages is a phrase nobody said. The previous encoding was worse
	// than merely flat: it expressed recency by REPEATING each token in place, so almost every
	// window was a doubled word and the genuine bigrams survived only at repetition
	// boundaries. One encoding served two consumers that needed opposite things, and it threw
	// away what each of them read (SPEC.md section 8, finding 48).
	RecentMessages [][]string

	// Names are the canonical recognized names in the prompt.
	Names []string

	// RecalledNames are people the channel was recently talking about who are NOT in the
	// current message.
	//
	// They reach the association tiers and nothing else. Deliberately not the name seed tier
	// and not PromptNames: seeding at, or naming, somebody nobody just mentioned reads as a
	// non-sequitur rather than as memory.
	RecalledNames []string

	// NameTokens is every SPELLING of a recognized name that appeared in the prompt, surface
	// form and canonical form both.
	//
	// Separate from Names rather than folded into it, because the two are read differently:
	// Names is looked up in name_topic, where only canonical forms are keys, so putting an
	// alias there would buy a guaranteed miss per lookup. This is looked up in the n-gram
	// index, where whichever spelling people actually type is what got learned.
	NameTokens []string

	// Trace records which tier won, or is nil. The same pointer the caller puts on the
	// Step: a seed and the walk that followed it are one sentence and splitting them across
	// two traces would mean joining them again at analysis time.
	Trace *Trace
}

// Seed picks a starting prefix, or returns "" when the corpus offers nothing.
//
// An empty return is the caller's problem to handle and deliberately not a fallback
// string: the caller knows whether it would rather fall back to a prompt word or say
// nothing, and this does not.
func (g *Generator) Seed(in SeedInput) string {
	// The candidates that came from what the user typed. Exempt from the author-diversity
	// gate below, because echoing somebody's own words back is not poisoning.
	fromPrompt := map[string]struct{}{}

	return g.drawAttestedSeed(g.collectSeedCands(in, fromPrompt), fromPrompt, in.Trace)
}

// collectSeedCands runs every tier and returns the unnormalized candidates.
//
// Separate from Seed so a test can assert that every tier actually contributes something.
// Two tiers have now been found dead by reading rather than by a failing test (findings 34
// and 37), and the reason a behavioural test could not catch either is that a shadowed tier
// changes no output at all: its keys are already present at a higher weight.
//
// fromPrompt is filled in as a side effect, because whether a candidate came from what the
// user typed is decided by which tier produced it and is needed by the attestation exemption.
func (g *Generator) collectSeedCands(in SeedInput, fromPrompt map[string]struct{}) *seedCands {
	cands := newSeedCands()

	maxN := max(g.params.MaxNGram-1, 1)

	// Tier 1: n-grams from the prompt, longest first, and single prompt words at n == 1.
	g.addWindows(cands, tierPromptNgram, in.PromptWords, maxN, fromPrompt)

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
			cands.add(tierName, token, 1.0)
			fromPrompt[token] = struct{}{}
		}
	}

	// Tier 3: topics associated with a name.
	//
	// RECALLED NAMES REACH THIS TIER AND NOT TIER 2, which is the whole rule about memory of
	// people. What the channel was recently discussing should colour what the bot talks about;
	// starting a reply AT somebody nobody just mentioned, or naming them outright, reads as a
	// non-sequitur rather than as memory. So recall steers and never seeds, exactly as the
	// referenced message does.
	//
	// They share the tier's budget rather than getting one of their own, at a discount, so
	// somebody named right now always outranks somebody named five messages ago.
	for _, name := range in.Names {
		assoc, err := g.corpus.NameTopicsFor(name)
		if err != nil {
			continue
		}
		for topic, d := range assoc {
			cands.add(tierNameTopic, topic, assocScore(d))
		}
	}
	for _, name := range in.RecalledNames {
		assoc, err := g.corpus.NameTopicsFor(name)
		if err != nil {
			continue
		}
		for topic, d := range assoc {
			cands.add(tierNameTopic, topic, recalledNameDiscount*assocScore(d))
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
				cands.add(tierTopicWord, other, assocScore(d))
			}
		}
	}

	// THE NAME-POSITIONAL TIER IS GONE, and it never once decided anything (SPEC.md
	// section 8, finding 37). It read NameTopicsFor for the most recent name at
	// weightNamePositional 8.0, but the tier above already added every one of those keys,
	// from the identical TopicAssoc, at weightNameTopic 18.0, and add() keeps the maximum.
	// It is weightPromptWord at 15.0 sitting under tier 1 at 30.0 all over again, which
	// finding 34 recorded and M14 deleted, reintroduced by the same respacing that deleted
	// it. Its one distinguishing feature, the HasSuccessors filter, is redundant too:
	// attested() already requires a successor with enough authors, which implies having
	// successors at all, for every non-prompt candidate whenever MinDistinctAuthors > 0.
	//
	// Deleted rather than reweighted, per finding 28: a tier that asks a question another
	// tier already answers is a duplicate however different its filter looks. Under budgets
	// it would have been worse than dead, since it would spend a share of the draw on
	// candidates that duplicate another tier.

	// The two-hop expansion, replacing the concept-cluster tier.
	for word, score := range g.twoHop(in) {
		cands.add(tierTwoHop, word, score)
	}

	// Recent conversation, the floor.
	//
	// One message at a time, so a window cannot span two of them: joining the tail of one
	// message to the head of the next produces a phrase nobody said, and the old flat slice
	// had no boundary in it at all.
	for _, msg := range in.RecentMessages {
		g.addWindows(cands, tierRecent, msg, maxN, nil)
	}

	return cands
}

// addWindows offers every n-gram window of words to one tier, longest first.
//
// # Why this is shared rather than written twice (SPEC.md section 8, finding 47)
//
// The prompt tier had two rules about what may OPEN a reply, and the recent tier had neither,
// so conversation memory could seed a sentence on "is" or "and" while the identical window from
// the prompt was refused. That is finding 28's shape: two statements of one rule, differing
// only in what one of them forgot. The rules belong to the question "may a reply start here",
// which is not a property of where the words came from.
//
// fromPrompt may be nil. It is filled only for prompt-derived candidates, which are exempt from
// the author-diversity gate at draw time, because echoing somebody's own words back is not
// poisoning. Recent-conversation candidates are NOT exempt: they are things other people said,
// and the corpus has to attest them like anything else.
func (g *Generator) addWindows(cands *seedCands, tier seedTier, words []string, maxN int, fromPrompt map[string]struct{}) {
	for n := maxN; n >= 1; n-- {
		for i := 0; i+n <= len(words); i++ {
			key := strings.Join(words[i:i+n], " ")

			// A LONE FUNCTION WORD IS NOT A SEED. Starting a reply on "is" or "of" or "the"
			// can only produce something that reads as though its first half went missing:
			// the golden samples had "is peak bird behaviour honestly" and "what it did".
			//
			// Multi-word keys are exempt deliberately: "the bird" and "the server is" open
			// perfectly well, and it is the word standing alone that is the problem.
			if n == 1 && text.IsStopWord(key) {
				continue
			}

			// AND A SEED MAY NOT OPEN ON A CONJUNCTION, whatever its length. The rule above
			// only catches a lone function word, so the prompt "greg and lachy are both" still
			// offered the window "and lachy are", which generated "and lachy are you know what
			// i am going to lose it". A conjunction promises a clause that was never there,
			// which is the same damage as the lone stop word one line up, arriving through a
			// window the length exemption let past.
			if !text.CanOpenSentence(words[i]) {
				continue
			}

			if g.corpus.HasSuccessors(key) {
				cands.add(tier, key, float64(n))
				if fromPrompt != nil {
					fromPrompt[key] = struct{}{}
				}
			}
		}
	}
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

	sources := make([]string, 0, len(in.PromptWords)+len(in.Names)+len(in.RecalledNames))
	sources = append(sources, in.PromptWords...)
	sources = append(sources, in.Names...)
	sources = append(sources, in.RecalledNames...)

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
				w := assocScore(second.assoc) * twoHopDecay
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
func (g *Generator) drawAttestedSeed(cands *seedCands, prompt map[string]struct{}, tr *Trace) string {
	// Recorded here rather than by the caller because this is the only place that knows
	// both which key won and which tier owns it, and the tier is the interesting half: two
	// dead tiers have been found by reading constants rather than by a failing test
	// (findings 34 and 37), and a share of real draws per tier is what would have shown
	// either one immediately.
	// The comma-ok is not defensive noise. seedTier's zero value is tierName, so a lookup
	// that missed would silently attribute the draw to the name tier, which is the one tier
	// whose share this repository has already had to fix once (finding 36). A miss records
	// nothing rather than recording a lie.
	won := func(seed string) string {
		if seed == "" {
			return ""
		}
		if tier, ok := cands.tierOf[seed]; ok {
			tr.seed(tier, seed)
		}
		return seed
	}

	if g.params.MinDistinctAuthors <= 0 {
		return won(g.drawSeed(cands.weights()))
	}

	// Enough attempts to get past a few unattested candidates, few enough that a corpus
	// where nothing qualifies does not turn every reply into a scan of it. Falling through
	// to "" means the caller falls back to a prompt word or stays silent, which is the same
	// outcome an empty corpus produces.
	const attempts = 8
	for range attempts {
		// Re-derived per attempt rather than adjusted in place. A rejected candidate must
		// forfeit its share to NOBODY: if the name tier's only candidate fails attestation
		// the name tier should contribute zero, where deleting from a flat weight map would
		// silently donate a quarter of the draw to the prompt tier instead.
		weights := cands.weights()
		seed := g.drawSeed(weights)
		if seed == "" {
			return ""
		}
		if _, fromPrompt := prompt[seed]; fromPrompt {
			return won(seed)
		}
		// A multi-word seed is a stored prefix, and its own continuations are what the
		// sampler will gate at the first step, so attestation of the whole phrase is what
		// matters rather than of one token in it.
		if g.attested(seed) {
			return won(seed)
		}
		cands.drop(seed)
		if len(cands.tierOf) == 0 {
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
		//
		// admissible rather than a second copy of the comparison, so the count allowance
		// M24 added reaches this producer too. The version of this line that predates it is
		// exactly the shape of finding 31, where the gate existed at one of three places
		// that put a word into a sentence and the other two handed refused phrases straight
		// back.
		if s.Token != EndToken && g.admissible(s.Count, s.Authors, 1) {
			return true
		}
	}
	return false
}

// TrimDangling removes trailing function words that leave a sentence hanging.
//
// It trims in two bands, and the difference between them is the whole content of this
// function.
//
// THE UNCONDITIONAL BAND is text.IsDanglingTail: prepositions, conjunctions, determiners and
// auxiliaries. These are trimmed whether or not the model chose to end, and that
// unconditionality is a correction to the rule below rather than an exception to it.
//
// THE CONDITIONAL BAND is every other function word, trimmed only when the sentence ran out
// of chain. An end sentinel means somebody really did finish a message on that word, and in
// this register that is worth keeping even when it looks abrupt: "i am going to lose it"
// ends on a stop word and is exactly right.
//
// # Why the first band had to be carved out of the second
//
// The original rule was the asymmetry attested() makes about EndToken: what the corpus
// witnessed somebody do is evidence, what generation merely ran into is not. That is right
// about the token and wrong about the construction, and golden samples are what showed it.
// Nearly a third of replies ended on a trailing preposition, all of them protected by this
// exemption. "about" is followed by the sentinel in the corpus by two different authors,
// because two people ended a message with "what are you talking about" - so the corpus
// attests that "about" can end a message, and generation cashed that attestation in on
// "nurock is coping about", where the phrase never arrives.
//
// The attestation is recorded on the token; the thing that made it true was the construction,
// and the composite key layout does not carry it. So the honest fix is to say which function
// words can close a sentence at all, which is what text.IsDanglingTail is.
//
// It will trim a sentence down to nothing if that is all it was, and that is fine: the
// caller has a floor below which it posts nothing at all, and saying less is the right
// direction for a failure that reads as a malfunction.
func TrimDangling(sentence []string, choseEnd bool) []string {
	end := len(sentence)
	for end > 0 {
		last := sentence[end-1]
		prev := ""
		if end > 1 {
			prev = sentence[end-2]
		}

		// An UNGOVERNED PRONOUN is the second thing the end-token attestation licensed
		// wrongly, and for the same reason: "i am going to lose it" and "greg and lachy
		// are you" both end on a pronoun, and only the word in front of it says which is
		// a sentence and which is a fragment.
		if text.IsDanglingTail(last) || !text.IsGovernedPronoun(prev, last) {
			end--
			continue
		}
		if !choseEnd && text.IsStopWord(last) {
			end--
			continue
		}
		break
	}
	return sentence[:end]
}

// SeedTopic reduces a seed to the single word the sentence is about.
//
// Step.CurrentTopic used to be the seed verbatim (SPEC.md section 8, finding 44). A seed is
// a stored n-gram prefix and is therefore usually several words, while topic_word is keyed by
// single words, so TopicWordsFor("what do you know") returned an EMPTY NON-NIL map. That
// passes the `a != nil` guard in the scorer and then never matches a candidate, so the 0.35
// CurrentTopic term was silently absent for roughly nine sentences in ten. It got worse as
// the corpus grew, because more long prompt windows gain successors and win the seed draw.
//
// The LAST content word rather than the first, because that is the word the chain actually
// continues from and therefore the one whose associations describe where the sentence is.
//
// Exported because production and the golden harness both set CurrentTopic, and two callers
// computing "what topic is this sentence in" is how they come to disagree.
func SeedTopic(seed string) string {
	words := strings.Fields(seed)
	for i := len(words) - 1; i >= 0; i-- {
		if !text.IsStopWord(words[i]) {
			return words[i]
		}
	}
	// All function words. Returning the last one is better than returning the whole phrase,
	// which is guaranteed to miss, and the scorer treats "" as no topic at all.
	if len(words) > 0 {
		return words[len(words)-1]
	}
	return ""
}
