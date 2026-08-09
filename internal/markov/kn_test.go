package markov

import (
	"math"
	"math/rand/v2"
	"testing"
)

// seeded returns a reproducible Source. rand.New over an explicit PCG seed rather
// than the package-level functions, which is the whole reason Source is a parameter.
func seeded(a, b uint64) Source { return rand.New(rand.NewPCG(a, b)) }

func TestContextsAreSuffixesLongestFirst(t *testing.T) {
	g := New(newFake(), testParams(), seeded(1, 2))

	got := g.contexts([]string{"the", "bird", "is", "on", "the", "roof"})

	// MaxNGram 5 means a context of at most 4 words, and each shorter context must be
	// a SUFFIX of the longer one. Getting this backwards gives a model that generates
	// nonsense while every count-based test still passes, because both directions
	// produce valid-looking keys.
	want := []string{"is on the roof", "on the roof", "the roof", "roof"}
	if len(got) != len(want) {
		t.Fatalf("got %d contexts %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("context %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestInterpolatedKNSumsToOne is the correctness test for the probability model, and
// it is the one that would catch a wrong lambda.
//
// Interpolated Kneser-Ney sums to one without a normalization step, because the mass
// lambda hands down is exactly the mass the discount removed: each of the N1+ observed
// continuations gives up D. If lambda is computed any other way the distribution
// leaks or over-counts, and nothing else in this suite would notice: generation would
// still produce words, and only the balance between orders would be wrong, which is
// the exact defect (unweighted backoff, finding G1) this model exists to fix.
//
// The sum is taken over the observed continuations plus the mass handed to the base
// case, since the base case spreads the remainder over the whole vocabulary.
func TestInterpolatedKNSumsToOne(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is on the roof")
	f.learn(5, "bob", "the bird is loose again")
	f.learn(5, "carol", "the bird knows what it did")

	g := New(f, testParams(), seeded(1, 2))
	m := g.newModel()

	// A context with real mass and more than one continuation.
	const ctx = "the bird"
	o, err := m.stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if o.total == 0 {
		t.Fatalf("fixture problem: %q has no occurrences", ctx)
	}
	if o.distinct < 2 {
		t.Fatalf("fixture problem: %q has %d distinct continuations, want at least 2", ctx, o.distinct)
	}

	var observed float64
	succ, err := f.Successors(ctx)
	if err != nil {
		t.Fatalf("successors: %v", err)
	}
	for _, s := range succ {
		observed += math.Max(float64(s.Count)-g.params.KNDiscount, 0) / float64(o.total)
	}

	lambda := o.lambda(g.params.KNDiscount)
	if total := observed + lambda; math.Abs(total-1.0) > 1e-9 {
		t.Errorf("discounted mass %.12f plus lambda %.12f = %.12f, want 1.0. "+
			"lambda must equal exactly the mass the discount removed, D*N1+/total, "+
			"or the balance between orders is wrong and nothing in the output shows it",
			observed, lambda, total)
	}
}

// TestHigherOrderIsPreferredWhenBothHaveMass is the pin for finding G1.
//
// The old shrink loop took the first non-empty result from the longest prefix, so a
// 4-gram continuation and a bigram continuation were scored on the same scale and the
// order carried no weight at all. Here both candidates are enumerated together and
// the one with high-order evidence must come out ahead, because it collects its own
// discounted high-order term AND the interpolated tail while the other gets only the
// tail.
//
// Constructed so raw frequency points the OTHER way: "loose" is more frequent overall
// than "roof", so a model that ignored the high-order context would prefer it.
func TestHigherOrderIsPreferredWhenBothHaveMass(t *testing.T) {
	f := newFake()
	// "roof" follows the full four-word context, once.
	f.learn(5, "alice", "the bird is on the roof")
	// "loose" follows only the short context, several times, and is a frequent word.
	for range 6 {
		f.learn(5, "bob", "a bird went loose")
		f.learn(5, "carol", "something loose happened")
	}

	g := New(f, testParams(), seeded(1, 2))
	m := g.newModel()

	ctxs := g.contexts([]string{"the", "bird", "is", "on", "the"})
	pRoof, err := m.prob(ctxs, "roof")
	if err != nil {
		t.Fatalf("prob roof: %v", err)
	}
	pLoose, err := m.prob(ctxs, "loose")
	if err != nil {
		t.Fatalf("prob loose: %v", err)
	}

	if pRoof <= pLoose {
		t.Errorf("P(roof|%q)=%.9f but P(loose|...)=%.9f. The candidate with high-order "+
			"evidence must win even though the other is more frequent overall; if it does "+
			"not, backoff is carrying no weight and this is finding G1 again",
			ctxs[0], pRoof, pLoose)
	}
}

// TestUnseenContextPassesThroughToTheLowerOrder covers the degenerate branch: a
// context nothing has ever been seen after must keep no mass and hand everything down,
// rather than contributing a zero that collapses the whole product.
func TestUnseenContextPassesThroughToTheLowerOrder(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is on the roof")

	g := New(f, testParams(), seeded(1, 2))
	m := g.newModel()

	withGarbage, err := m.prob([]string{"nobody said this ever", "bird"}, "is")
	if err != nil {
		t.Fatalf("prob: %v", err)
	}
	if withGarbage <= 0 {
		t.Fatalf("an unseen high-order context zeroed the probability (%v); lambda must be "+
			"1 for a context with no occurrences", withGarbage)
	}

	onlyReal, err := g.newModel().prob([]string{"bird"}, "is")
	if err != nil {
		t.Fatalf("prob: %v", err)
	}
	if math.Abs(withGarbage-onlyReal) > 1e-12 {
		t.Errorf("prefixing an unseen context changed the result: %.12f vs %.12f. An unseen "+
			"order must be a no-op, not a perturbation", withGarbage, onlyReal)
	}
}

// TestKNRawMixMovesTheBaseCase pins the deliberate deviation from textbook KN.
//
// mu interpolates the base case back toward raw frequency so that memes, which are
// frequent but appear in few distinct contexts and are therefore statistically
// indistinguishable from "San Francisco", are not systematically suppressed. The
// fixture builds exactly that shape: "copypasta" appears many times but always after
// the same context, while "varied" appears fewer times after many different ones.
//
// At mu=0 (textbook) the low-diversity token must lose. Raising mu must move it up.
func TestKNRawMixMovesTheBaseCase(t *testing.T) {
	f := newFake()
	// One context, many occurrences: the copypasta shape.
	for range 20 {
		f.learn(5, "alice", "spam spam copypasta")
	}
	// Many contexts, fewer occurrences each.
	for _, ctx := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta"} {
		f.learn(5, "bob", ctx+" thing varied")
	}

	measure := func(mu float64) (float64, float64) {
		p := testParams()
		p.KNRawMix = mu
		g := New(f, p, seeded(1, 2))
		m := g.newModel()
		return m.baseProb("copypasta"), m.baseProb("varied")
	}

	kn0Copy, kn0Varied := measure(0.0)
	if kn0Copy >= kn0Varied {
		t.Fatalf("fixture problem: at mu=0, textbook KN should rank the low-diversity "+
			"token below the high-diversity one, got copypasta=%.9f varied=%.9f",
			kn0Copy, kn0Varied)
	}

	hiCopy, _ := measure(1.0)
	if hiCopy <= kn0Copy {
		t.Errorf("raising mu from 0 to 1 did not raise the copypasta's base probability "+
			"(%.9f to %.9f). mu is what keeps the server's register from being suppressed "+
			"by KN working correctly (SPEC.md section 5.2)", kn0Copy, hiCopy)
	}
}

// TestBaseProbFallsBackWhenTheTopicTotalIsMissing covers the corpus that predates the
// count:topic_total counter. Storage backfills it, but the engine must not produce a
// zero or a NaN if it is ever absent.
func TestBaseProbFallsBackWhenTheTopicTotalIsMissing(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is here")
	f.topicTotal = 0 // as a pre-M7a corpus would read

	g := New(f, testParams(), seeded(1, 2))
	p := g.newModel().baseProb("bird")
	if p <= 0 || math.IsNaN(p) || math.IsInf(p, 0) {
		t.Errorf("baseProb = %v with no topic total; it must fall back to pure KN, not to "+
			"zero or NaN", p)
	}
}

// TestEnumerateBacksOffToWidenASparseChoice is why minCandidates exists.
//
// The corpus is sparse by nature: at order 5 nearly every 4-gram has count 1, so the
// longest context usually has exactly one continuation and the step is deterministic
// no matter what the sampler does. That was a large part of why the old engine felt
// canned even though it had a temperature-like dial.
func TestEnumerateBacksOffToWidenASparseChoice(t *testing.T) {
	f := newFake()
	// Exactly one continuation of the long context.
	f.learn(5, "alice", "the bird is on the roof")
	// Several continuations of the short one.
	f.learn(5, "bob", "the wolf")
	f.learn(5, "bob", "the ceiling")
	f.learn(5, "bob", "the void")
	f.learn(5, "bob", "the algorithm")
	f.learn(5, "bob", "the vibes")

	g := New(f, testParams(), seeded(1, 2))
	m := g.newModel()
	ctxs := g.contexts([]string{"bird", "is", "on", "the"})

	cands, err := m.enumerate(ctxs)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if len(cands) < minCandidates {
		t.Errorf("enumerate returned %d candidates from a context whose longest order has "+
			"one continuation; it must back off until it has at least %d or runs out of "+
			"orders, or generation is deterministic in a sparse corpus", len(cands), minCandidates)
	}

	var sawRoof bool
	for _, c := range cands {
		if c.token == "roof" {
			sawRoof = true
		}
	}
	if !sawRoof {
		t.Error("backing off dropped the high-order candidate; the union must keep it, " +
			"because it is the one the long context actually predicts")
	}
}

// TestEnumerateIsDeterministic guards the property that M6b had to delete a heuristic
// over: candidates arrive in a stable order, so nothing downstream that reads position
// can pick up Go's randomized map iteration.
func TestEnumerateIsDeterministic(t *testing.T) {
	f := newFake()
	for _, s := range []string{"the a", "the b", "the c", "the d", "the e", "the f", "the g"} {
		f.learn(5, "alice", s)
	}

	g := New(f, testParams(), seeded(1, 2))
	var first []string
	for run := range 8 {
		cands, err := g.newModel().enumerate([]string{"the"})
		if err != nil {
			t.Fatalf("enumerate: %v", err)
		}
		got := make([]string, len(cands))
		for i, c := range cands {
			got[i] = c.token
		}
		if run == 0 {
			first = got
			continue
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("run %d produced order %q, first run produced %q", run, got, first)
			}
		}
	}
}
