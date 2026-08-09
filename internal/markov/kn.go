package markov

import (
	"math"
	"sort"
	"strings"
)

// Enumeration bounds. Constants rather than knobs, because they trade read latency
// against candidate diversity in a way an operator has no instrument to judge, and
// because top-k already gives them a dial that means something.
const (
	// minCandidates is how few candidates make it worth backing off one more order
	// purely to widen the choice.
	//
	// This does real work rather than guarding an edge case. The corpus is sparse:
	// at order 5, nearly every 4-gram has count 1, so the longest context usually
	// has exactly ONE continuation and the step is deterministic no matter what the
	// sampler does. That is a large part of why the old engine felt canned. Backing
	// off here widens the choice while the interpolation below keeps the high-order
	// evidence weighted properly, which is the whole difference from the old
	// first-non-empty-wins shrink loop.
	minCandidates = 5

	// perOrderCap and candidateCap bound the work. A low-order context like a single
	// common word can have thousands of continuations, and while enumerating them is
	// a cheap sequential scan, SCORING them is a dozen heuristics and several map
	// lookups each. So each order contributes at most its highest-count
	// continuations and the union is capped.
	//
	// This is a bias toward frequency at the enumeration step, and it is worth being
	// explicit that it is a bias: a rare continuation with a high interpolated
	// probability can be cut before it is ever scored. It is acceptable because
	// top-k truncates the tail anyway at a default of 40, so the cap sits well above
	// the point where candidates survive sampling, and because frequency bias is
	// aligned with the register this bot wants rather than against it.
	perOrderCap  = 96
	candidateCap = 256
)

// candidate is one continuation on its way through the scorer.
type candidate struct {
	token   string
	count   uint64
	authors uint32

	// logProb is log P(token | context) from interpolated Kneser-Ney, and logit is
	// that plus every heuristic. They are kept separate so a test can assert about
	// the model without the heuristics, and so the golden harness can print both.
	logProb float64
	logit   float64
}

// contexts returns the suffixes of the prefix from longest to shortest, capped at
// MaxNGram-1 words.
//
// Suffixes, not prefixes: the shorter context of "the cat sat" is "cat sat", because
// what a word follows is what predicts it. Getting this backwards produces a model
// that silently generates nonsense while every test about counts still passes.
func (g *Generator) contexts(prefixWords []string) []string {
	max := g.params.MaxNGram - 1
	if max < 1 {
		max = 1
	}
	if len(prefixWords) > max {
		prefixWords = prefixWords[len(prefixWords)-max:]
	}

	out := make([]string, 0, len(prefixWords))
	for k := len(prefixWords); k >= 1; k-- {
		out = append(out, strings.Join(prefixWords[len(prefixWords)-k:], " "))
	}
	return out
}

// orderStats is one context's cached statistics.
type orderStats struct {
	// total is c(context, .) and distinct is N1+(context .), the two numbers
	// absolute discounting needs.
	total    uint64
	distinct uint64

	// counts memoizes per-candidate counts for this context, filled on demand by
	// point lookup rather than by materializing every continuation.
	//
	// That distinction is the whole reason PrefixTotal exists. The recursion needs
	// statistics for EVERY order including the single-word context, and a common
	// single word can have thousands of continuations. Enumerating them to sum their
	// counts would allocate a slice of thousands of structs per distinct last word
	// per sentence, on the reply path, purely to compute one number that a
	// zero-allocation cursor scan already gives. So total comes from PrefixTotal,
	// distinct comes from the stored index, and the only counts ever read are the
	// ones belonging to candidates we are actually going to score.
	counts map[string]uint64
}

// lambda is the mass this context hands down to the next order.
//
// D * N1+(c .) / c(c .), which is exactly the mass the discount removed: each of the
// N1+ observed continuations gave up D. That is why interpolated Kneser-Ney sums to
// one without a normalization step, and why lambda is not a free parameter.
func (o orderStats) lambda(d float64) float64 {
	if o.total == 0 {
		// An unseen context keeps nothing and passes everything down.
		return 1.0
	}
	l := d * float64(o.distinct) / float64(o.total)
	if l > 1.0 {
		// Only reachable if D exceeds 1 or the stored distinct count has drifted
		// above the total, neither of which should happen. Clamping beats emitting a
		// probability above one and having the sampler quietly renormalize it away.
		return 1.0
	}
	return l
}

// model is the per-call state of the probability model: the loaded order statistics
// and the memo that keeps a repeated low-order context from being scanned twice.
//
// Per call rather than per Generator, deliberately. A cache that outlives the call
// would have to be invalidated as the corpus learns, and ingestion runs continuously,
// so a stale total is a silently wrong distribution. Within one sentence the corpus
// cannot change under us, because generation holds one read transaction.
type model struct {
	g      *Generator
	orders map[string]*orderStats
}

func (g *Generator) newModel() *model {
	return &model{g: g, orders: make(map[string]*orderStats, g.params.MaxNGram)}
}

// stats loads and memoizes one context's statistics.
func (m *model) stats(ctx string) (*orderStats, error) {
	if o, ok := m.orders[ctx]; ok {
		return o, nil
	}

	o := &orderStats{counts: map[string]uint64{}, total: m.g.corpus.PrefixTotal(ctx)}

	if o.total > 0 {
		if ks, err := m.g.corpus.KNStats(ctx, ""); err == nil {
			o.distinct = ks.DistinctSuccessors
		}
		// A context with occurrences must have at least one distinct continuation, and
		// clamping to the total keeps lambda inside [0,1]. Both directions are
		// reachable only if the incrementally maintained index has drifted from the
		// n-gram bucket, which the maintenance rebuild exists to repair, but a drifted
		// index must not produce a probability above one: that does not fail, it
		// quietly reweights every order and there is nothing in the output to see.
		if o.distinct == 0 {
			o.distinct = 1
		}
		if o.distinct > o.total {
			o.distinct = o.total
		}
	}

	m.orders[ctx] = o
	return o, nil
}

// count returns c(ctx, token), memoized.
func (m *model) count(ctx string, o *orderStats, token string) uint64 {
	if c, ok := o.counts[token]; ok {
		return c
	}
	var c uint64
	if s, found, err := m.g.corpus.Successor(ctx, token); err == nil && found {
		c = s.Count
	}
	o.counts[token] = c
	return c
}

// baseProb is the lowest order: the probability of a token with no context at all.
//
// This is where peregrine deliberately departs from the textbook, and the deviation
// is the single most counter-intuitive decision in the codebase, so it is worth
// stating in full rather than pointing at the spec.
//
// Kneser-Ney's central insight is that a lower-order estimate should use CONTINUATION
// counts, meaning the number of distinct contexts a token follows, rather than raw
// frequency. The canonical example is "Francisco": common in the corpus, but almost
// always preceded by "San", so raw frequency makes it a strong fallback everywhere
// while continuation counts correctly make it a weak one. That correction is why KN
// wins on perplexity.
//
// The problem is that a meme, a copypasta and an inside joke are statistically
// indistinguishable from "Francisco". They are frequent and they appear in few
// distinct contexts, because the whole point of a copypasta is that it comes with its
// own context. So pure Kneser-Ney would systematically suppress exactly the register
// this server runs on, and it would do so BECAUSE it is working correctly.
//
// mu (KNRawMix) interpolates the base case back toward raw frequency. 0.0 is textbook
// KN and maximizes conventional quality; 1.0 is raw counts; the default leans to KN
// while keeping the memes. Anyone who sets this to 0 on the authority of a paper is
// making the output worse by the only metric that matters here, which is whether the
// bot is funny (SPEC.md section 5.2).
func (m *model) baseProb(token string) float64 {
	p := m.params()

	var pkn float64
	if denom := m.g.corpus.TotalDistinctPredecessors(); denom > 0 {
		if ks, err := m.g.corpus.KNStats("", token); err == nil {
			pkn = float64(ks.DistinctPredecessors) / float64(denom)
		}
	}

	// The raw mix is skipped entirely when there is no unigram total to divide by.
	// Falling back to pure Kneser-Ney is the honest degradation: the alternative is a
	// zero probability for every token, which is a silent outage rather than a slightly
	// different register. Reachable only on an empty corpus, now that storage backfills
	// the counter for corpora written before it existed.
	if mu := p.KNRawMix; mu > 0 {
		if denom := m.g.corpus.TotalTopicCount(); denom > 0 {
			praw := float64(m.g.corpus.TopicCount(token)) / float64(denom)
			pkn = (1-mu)*pkn + mu*praw
		}
	}

	if pkn <= 0 {
		// A token with no predecessor count and no unigram count. It can still be
		// reached through a higher order's discounted term, so it needs a floor
		// rather than a zero: log(0) is -Inf, which poisons every arithmetic
		// operation downstream and turns one unlucky candidate into a NaN
		// distribution.
		return 1e-12
	}
	return pkn
}

func (m *model) params() Params { return m.g.params }

// prob returns P(token | contexts), the interpolated Kneser-Ney probability, walking
// the recursion from the shortest context up to the longest.
//
// Written bottom-up rather than as recursion because the recursion is linear: each
// order needs only the order below it, so a loop over the reversed context list is
// the same arithmetic without the call depth, and it makes the lambda chain visible
// in one place.
func (m *model) prob(ctxs []string, token string) (float64, error) {
	p := m.baseProb(token)

	// ctxs runs longest to shortest, so walk it backwards.
	for i := len(ctxs) - 1; i >= 0; i-- {
		o, err := m.stats(ctxs[i])
		if err != nil {
			return 0, err
		}
		if o.total == 0 {
			// Unseen context: lambda is 1 and the discounted term is 0, so this
			// order passes the lower estimate through unchanged.
			continue
		}
		var discounted float64
		if c := m.count(ctxs[i], o, token); c > 0 {
			discounted = math.Max(float64(c)-m.params().KNDiscount, 0) / float64(o.total)
		}
		p = discounted + o.lambda(m.params().KNDiscount)*p
	}

	if p <= 0 {
		return 1e-12, nil
	}
	return p, nil
}

// enumerate builds the candidate set and its log-probabilities.
//
// This replaces the prefix-shrink loop that took the first non-empty result from the
// longest context, which meant a 4-gram continuation and a bigram continuation were
// scored on the same scale and the order carried no weight at all (SPEC.md finding
// G1). Here every candidate is scored with the full interpolation, so a continuation
// with high-order evidence gets both its discounted high-order term AND the
// interpolated tail, while one that only exists at a lower order gets the tail alone.
// That is what "prefers the higher order when both have mass" means, and it falls out
// of the model rather than being a rule bolted on top.
func (m *model) enumerate(ctxs []string) ([]candidate, error) {
	type seen struct {
		count   uint64
		authors uint32
	}
	pool := make(map[string]seen)

	for _, ctx := range ctxs {
		o, err := m.stats(ctx)
		if err != nil {
			return nil, err
		}
		if o.total == 0 {
			continue
		}

		succ, err := m.g.corpus.Successors(ctx)
		if err != nil {
			return nil, err
		}

		// Highest count first, with the token as a deterministic tie-break. The
		// tie-break is not cosmetic: Successors returns a cursor scan, so its order
		// is already deterministic, and a sort that was not stable under equal
		// counts would make the golden samples irreproducible.
		sort.Slice(succ, func(i, j int) bool {
			if succ[i].Count != succ[j].Count {
				return succ[i].Count > succ[j].Count
			}
			return succ[i].Token < succ[j].Token
		})

		added := 0
		for _, s := range succ {
			// Fill this order's count memo while the list is in hand, for every
			// continuation and not just the ones that fit the cap: prob will ask for
			// some of them from a different order's enumeration, and a map write here
			// costs less than the point lookup it saves.
			o.counts[s.Token] = s.Count

			if added >= perOrderCap || len(pool) >= candidateCap {
				continue
			}
			if _, dup := pool[s.Token]; dup {
				continue
			}
			pool[s.Token] = seen{count: s.Count, authors: s.Authors}
			added++
		}

		if len(pool) >= minCandidates || len(pool) >= candidateCap {
			break
		}
	}

	if len(pool) == 0 {
		return nil, nil
	}

	tokens := make([]string, 0, len(pool))
	for t := range pool {
		tokens = append(tokens, t)
	}
	// Sorted so the candidate slice is in a deterministic order before scoring. Go's
	// map iteration is randomized, and M6b already had to delete a heuristic that
	// depended on candidate index for exactly this reason: anything downstream that
	// reads position must read a stable one.
	sort.Strings(tokens)

	out := make([]candidate, 0, len(tokens))
	for _, t := range tokens {
		p, err := m.prob(ctxs, t)
		if err != nil {
			return nil, err
		}
		s := pool[t]
		out = append(out, candidate{
			token:   t,
			count:   s.count,
			authors: s.authors,
			logProb: math.Log(p),
		})
	}
	return out, nil
}
