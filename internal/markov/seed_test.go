package markov

import (
	"strings"
	"testing"
)

// TestSeedPrefersAPromptNgram. The prompt tier is the strongest and must be, because it
// is the only tier that knows what the user actually said. Everything else is the bot
// free-associating.
func TestSeedPrefersAPromptNgram(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")
	// A competing association, so the lower tiers have something to offer.
	for range 10 {
		f.addTopicWord("bird", "unrelated", 0.5)
		f.addTopicWord("unrelated", "elsewhere", 0.5)
	}
	f.learn(5, "bob", "unrelated things happen")
	f.learn(5, "carol", "elsewhere entirely different")

	g := New(f, testParams(), seeded(1, 2))

	// Sampled many times: the draw is weighted, not argmax, so the assertion is about
	// the distribution rather than a single call. A tier at 30n against tiers at 18 and
	// 6 should dominate heavily without being the only outcome.
	hits := 0
	const runs = 400
	for range runs {
		seed := g.Seed(SeedInput{PromptWords: []string{"the", "bird"}})
		if strings.Contains(seed, "bird") || strings.Contains(seed, "the") {
			hits++
		}
	}
	if hits < runs/2 {
		t.Errorf("a prompt n-gram was chosen %d/%d times; the prompt tier is weighted 30 "+
			"per word against 18 and 6 and must dominate, or replies stop being about "+
			"what was said", hits, runs)
	}
}

// TestSeedTwoHopReachesATransitiveAssociation is the pin for what replaced the concept
// clusters, and the fixture is built so that ONLY a two-hop walk can find the answer.
//
// "bird" co-occurs with "roof", and "roof" co-occurs with "gutter", but "bird" and
// "gutter" never co-occur. A direct-association tier cannot reach "gutter". This is
// exactly the reach clusters were supposed to provide, and it is now one bounded query
// instead of a persisted bucket with a merge threshold nobody validated (finding 29).
func TestSeedTwoHopReachesATransitiveAssociation(t *testing.T) {
	f := newFake()

	// "gutter" must be continuable, or the two-hop tier correctly refuses it.
	f.learn(5, "alice", "gutter cleaning again")
	f.learn(5, "bob", "gutter cleaning again")

	for range 5 {
		f.addTopicWord("bird", "roof", 0.5)
		f.addTopicWord("roof", "gutter", 0.5)
	}

	g := New(f, testParams(), seeded(3, 4))

	var found bool
	for range 600 {
		if g.Seed(SeedInput{PromptWords: []string{"bird"}}) == "gutter" {
			found = true
			break
		}
	}
	if !found {
		t.Error("the two-hop tier never reached \"gutter\", which is two association hops " +
			"from the prompt and zero direct hops. That reach is the only thing concept " +
			"clusters offered, and it is the whole justification for deleting them")
	}
}

// TestSeedTwoHopRefusesADeadEnd. A seed the chain cannot continue from produces a
// one-word reply, which reads as the bot malfunctioning rather than as a short joke.
// Every two-hop candidate has to pass HasSuccessors.
func TestSeedTwoHopRefusesADeadEnd(t *testing.T) {
	f := newFake()
	// "deadend" is associated transitively but appears in no n-gram, so it has no
	// continuations at all.
	for range 5 {
		f.addTopicWord("bird", "roof", 0.5)
		f.addTopicWord("roof", "deadend", 0.5)
	}
	f.learn(5, "alice", "bird noises")

	g := New(f, testParams(), seeded(5, 6))
	for range 400 {
		if got := g.Seed(SeedInput{PromptWords: []string{"bird"}}); got == "deadend" {
			t.Fatal("seeded on \"deadend\", which has no continuations; the two-hop tier " +
				"must require HasSuccessors or it produces one-word replies")
		}
	}
}

// TestSeedTwoHopDoesNotFollowASingleCooccurrence. One co-occurrence in one message says
// nothing, and following it multiplies that nothing by the second-hop fan-out.
func TestSeedTwoHopDoesNotFollowASingleCooccurrence(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "noise words here")
	f.learn(5, "bob", "noise words here")

	// A continuable prefix that sorts before "noise", so the no-tier-matched fallback
	// has somewhere else to land. Without this the test cannot tell a two-hop hit from
	// FirstPrefix returning the lexicographically smallest prefix, which is what it did
	// on the first run: it failed while the code was correct.
	f.learn(5, "alice", "aardvark filler text")
	f.learn(5, "bob", "aardvark filler text")

	// Exactly one co-occurrence at each hop, below twoHopMinCount.
	f.addTopicWord("bird", "roof", 0.5)
	f.addTopicWord("roof", "noise", 0.5)

	g := New(f, testParams(), seeded(7, 8))
	for range 300 {
		if got := g.Seed(SeedInput{PromptWords: []string{"bird"}}); got == "noise" {
			t.Fatalf("followed a single co-occurrence to %q; twoHopMinCount exists because "+
				"one pairing in one message is noise", got)
		}
	}
}

// TestSeedIsDeterministicUnderASeededSource. The draw iterates a map, and Go randomizes
// that, so without sorting the candidates first the same seed would give different
// output on different runs even though the weighting was correct. That would break the
// golden harness silently.
func TestSeedIsDeterministicUnderASeededSource(t *testing.T) {
	f := newFake()
	for _, s := range []string{"the alpha", "the beta", "the gamma", "the delta", "the epsilon"} {
		f.learn(5, "alice", s)
		f.learn(5, "bob", s)
	}

	run := func() []string {
		g := New(f, testParams(), seeded(11, 13))
		out := make([]string, 0, 40)
		for range 40 {
			out = append(out, g.Seed(SeedInput{PromptWords: []string{"the"}, RecentWords: []string{"alpha", "beta"}}))
		}
		return out
	}

	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("draw %d differed between runs: %q vs %q", i, a[i], b[i])
		}
	}
}

// TestSeedFallsBackToAnyRealPrefix. When no tier matches, a real prefix beats returning
// nothing, because a prefix has continuations and the caller's alternative is echoing
// the prompt.
func TestSeedFallsBackToAnyRealPrefix(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "completely unrelated content")

	g := New(f, testParams(), seeded(1, 1))
	got := g.Seed(SeedInput{PromptWords: []string{"zzzznothing"}})
	if got == "" {
		t.Error("no tier matched and the corpus is non-empty, so the fallback must return " +
			"a real prefix")
	}
	if !g.corpus.HasSuccessors(got) {
		t.Errorf("fallback returned %q, which has no continuations", got)
	}
}

func TestSeedOnAnEmptyCorpusReturnsNothing(t *testing.T) {
	g := New(newFake(), testParams(), seeded(1, 1))
	if got := g.Seed(SeedInput{PromptWords: []string{"anything"}}); got != "" {
		t.Errorf("got %q, want empty: an empty corpus has no seed and inventing one would "+
			"be a fallback string the caller cannot distinguish from real output", got)
	}
}

// TestJumpFindsARelatedContinuableWord covers the dead-end pivot, which is the same
// machinery as the two-hop tier asked at a different moment. Legacy had two separate
// implementations here, one of which was the cluster pivot that had never fired.
func TestJumpFindsARelatedContinuableWord(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "gutter cleaning again")
	f.learn(5, "bob", "gutter cleaning again")
	for range 5 {
		// Mean position 0.5, the middle, which is what the pivot prefers: a word that
		// normally ends sentences produces an abrupt stop and one that normally starts
		// them reads as a restart.
		f.addTopicWord("bird", "gutter", 0.5)
	}

	g := New(f, testParams(), seeded(1, 2))
	got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, []string{"bird", "is"})
	if got != "gutter" {
		t.Errorf("Jump = %q, want \"gutter\": it is associated with the context, has "+
			"continuations, and sits mid-sentence", got)
	}
}

func TestJumpNeverReturnsAWordAlreadyInTheSentence(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "gutter cleaning again")
	f.learn(5, "bob", "gutter cleaning again")
	for range 5 {
		f.addTopicWord("bird", "gutter", 0.5)
	}

	g := New(f, testParams(), seeded(1, 2))
	got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, []string{"bird", "gutter"})
	if got == "gutter" {
		t.Error("Jump returned a word already in the sentence, which is a stutter at the " +
			"exact moment the sentence was already struggling")
	}
}

func TestJumpRefusesADeadEnd(t *testing.T) {
	f := newFake()
	for range 5 {
		f.addTopicWord("bird", "deadend", 0.5)
	}
	f.learn(5, "alice", "bird noises")

	g := New(f, testParams(), seeded(1, 2))
	if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, []string{"bird"}); got == "deadend" {
		t.Error("jumped to a word with no continuations, which ends the sentence on the " +
			"jump word: worse than ending it one word earlier")
	}
}
