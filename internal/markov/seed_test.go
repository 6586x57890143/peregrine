package markov

import (
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/corpus"
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

// jumpStep wraps a sentence in the minimum Step that Jump needs.
//
// The Length matters rather than being boilerplate: Jump refuses once the sentence has reached
// Length.Min, so a zero-value Length would refuse every jump and every test below would pass
// for the wrong reason.
func jumpStep(sentence ...string) *Step {
	return &Step{Sentence: sentence, Length: Length{Min: 4, Max: 18}}
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
	got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, jumpStep("bird", "screaming"))
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
	got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, jumpStep("bird", "gutter"))
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
	if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, jumpStep("bird")); got == "deadend" {
		t.Error("jumped to a word with no continuations, which ends the sentence on the " +
			"jump word: worse than ending it one word earlier")
	}
}

// TestTheJumpHonoursTheAuthorGate is the pin for a hole found in M11c.
//
// The author-diversity gate lives in eligible(), which filters the sampler's candidates. Jump
// is a SECOND producer of words: on a dead end it picks a token out of the co-occurrence
// indexes and appends it directly, and those indexes carry no author attribution. So a phrase
// one person repeated was refused by the sampler at every step and then handed back by the
// jump: A6 defeated by exactly the shape design principle 3 exists to prevent, a check at one
// of two producers.
//
// It survived because the test that would have caught it seeded a corpus with no mentioned
// users, and both association indexes are gated on a name being present, so the jump had
// nothing to find. Found in M11c when that fixture became realistic.
func TestTheJumpHonoursTheAuthorGate(t *testing.T) {
	f := newFake()

	// One author repeating a phrase, which is the shape of the real attack, plus the
	// association that lets the jump reach it.
	for range 40 {
		f.learn(3, "poisoner", "bird poison spreads")
		f.addTopicWord("bird", "poison", 0.5)
	}

	g := New(f, Params{MaxNGram: 3, MinDistinctAuthors: 2}, DefaultSource{})
	if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, jumpStep("bird")); got == "poison" {
		t.Error("Jump returned a word only one author's messages attest; the gate must apply " +
			"to the jump as well as to the sampler")
	}

	// The control: with a second author on that continuation the jump is allowed, so the test
	// above is measuring the gate rather than a jump that never fires.
	f.learn(3, "someone-else", "bird poison spreads")
	if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, jumpStep("bird")); got != "poison" {
		t.Errorf("Jump returned %q, want poison once two authors attest it", got)
	}
}

// TestTheSeedHonoursTheAuthorGate is the same hole at the other end of the sentence. A seed
// drawn from a co-occurrence tier is a token the bot emits as its first word.
func TestTheSeedHonoursTheAuthorGate(t *testing.T) {
	f := newFake()
	for range 40 {
		f.learn(3, "poisoner", "poison spreads fast")
		f.addTopicWord("bird", "poison", 0.5)
	}

	g := New(f, Params{MaxNGram: 3, MinDistinctAuthors: 2}, DefaultSource{})
	for range 20 {
		// "bird" itself was never learned, so no prompt tier qualifies and the only candidates
		// are the unattested associations.
		if got := g.Seed(SeedInput{PromptWords: []string{"bird"}}); got == "poison" {
			t.Fatal("seeded from a word only one author's messages attest")
		}
	}
}

// TestPromptWordsAreExemptFromAttestation. Echoing somebody's own words back is not
// poisoning, and refusing to seed from the prompt would make the bot least responsive exactly
// when it was addressed directly.
func TestPromptWordsAreExemptFromAttestation(t *testing.T) {
	f := newFake()
	f.learn(3, "one-person", "hello there friend") // one author, unattested at a threshold of 2

	g := New(f, Params{MaxNGram: 3, MinDistinctAuthors: 2}, DefaultSource{})
	got := g.Seed(SeedInput{PromptWords: []string{"hello"}})
	if got == "" {
		t.Fatal("Seed returned nothing for a prompt word the corpus holds: a prompt-derived " +
			"seed is what the user typed, not something the corpus taught the bot")
	}
}

// The four bounds on Jump, one test each. All of them exist because of live output: every
// unreadable line the bot posted broke at a jump seam, and the chain-generated ones read
// fine. A jump appends a word out of the co-occurrence indexes, so it has no n-gram
// relationship at all to the word before it and the join is guaranteed rough, not
// occasionally rough.

// TestJumpRefusesOnceTheSentenceIsLongEnough. The jump exists to save a reply from being too
// short to post, and Length.Min is the definition of too short, so past it the length model's
// decision to stop should stand rather than be overridden with a seam.
func TestJumpRefusesOnceTheSentenceIsLongEnough(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "gutter cleaning again")
	f.learn(5, "bob", "gutter cleaning again")
	for range 5 {
		f.addTopicWord("bird", "gutter", 0.5)
	}
	g := New(f, testParams(), seeded(1, 2))

	// One word short of the floor: still worth a seam.
	short := jumpStep("bird", "screaming", "loudly")
	if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, short); got == "" {
		t.Error("refused a jump below the floor, where a jump is the only thing standing " +
			"between a short reply and no reply at all")
	}

	// At the floor: end instead.
	//
	// Every word here is deliberately NOT a stop word. The first version of this test ended
	// the sentence on "again", which is one, so it passed with this rule reverted: it was
	// measuring the function-word rule and reporting on the length one. Caught by reverting,
	// which is the only thing that catches a test whose name promises more than it checks.
	atFloor := jumpStep("bird", "screaming", "loudly", "somewhere")
	if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, atFloor); got != "" {
		t.Errorf("Jump = %q at the length floor, want silence: the sentence is already long "+
			"enough to post, so a guaranteed seam buys nothing", got)
	}
}

// TestJumpAllowsOnlyOneSeamPerSentence. One reads as a change of subject; two read as broken.
// This was unbounded, so a sentence under the floor could jump at every word, which is how
// "u just what if you dont have get the raped" happened.
func TestJumpAllowsOnlyOneSeamPerSentence(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "gutter cleaning again")
	f.learn(5, "bob", "gutter cleaning again")
	for range 5 {
		f.addTopicWord("bird", "gutter", 0.5)
	}
	g := New(f, testParams(), seeded(1, 2))

	s := jumpStep("bird")
	if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, s); got == "" {
		t.Fatal("the first jump was refused, so this test measures nothing")
	}
	if s.Jumps != 1 {
		t.Errorf("Jumps = %d after one jump, want 1: Jump counts its own so the count cannot "+
			"drift from the sentence it describes", s.Jumps)
	}
	if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, s); got != "" {
		t.Errorf("Jump = %q for a second seam in one sentence, want silence", got)
	}
}

// TestJumpRefusesAfterAFunctionWord. A determiner or preposition demands a specific kind of
// continuation, so a jump there cannot read as anything but a fault: "back to the" + "go",
// "what's the point of even a" + "know". After a content word the same jump reads as changing
// the subject.
func TestJumpRefusesAfterAFunctionWord(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "gutter cleaning again")
	f.learn(5, "bob", "gutter cleaning again")
	for range 5 {
		f.addTopicWord("bird", "gutter", 0.5)
	}
	g := New(f, testParams(), seeded(1, 2))

	for _, stop := range []string{"the", "of", "is"} {
		s := jumpStep("bird", stop)
		if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, s); got != "" {
			t.Errorf("Jump = %q after the function word %q, want silence", got, stop)
		}
	}

	// The control, so the test above is measuring the stop word rather than a jump that
	// never fires.
	if got := g.Jump(SeedInput{PromptWords: []string{"bird"}}, jumpStep("bird", "screaming")); got == "" {
		t.Error("refused a jump after a content word too, so the check is not about stop words")
	}
}

// TestTrimDanglingOnlyAppliesToASentenceThatRanOut.
//
// The caller passes the distinction rather than this guessing it, because the two endings are
// different claims. An end sentinel means somebody really did finish a message on that word,
// and in this register that is worth keeping even when it looks abrupt. A dead end means the
// chain had nowhere left to go, and stopping on "back to the" reads as being cut off.
//
// Same asymmetry attested() already makes about EndToken: what the corpus witnessed is
// evidence, what generation merely ran into is not.
func TestTrimDanglingOnlyAppliesToASentenceThatRanOut(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"back", "to", "the"}, []string{"back"}},
		// Stops at "even", which is not a function word. "what is the point of even" is a
		// perfectly good chat line, which is the point: this trims a dangle, it does not
		// try to make the sentence grammatical.
		{[]string{"what", "is", "the", "point", "of", "even", "a"},
			[]string{"what", "is", "the", "point", "of", "even"}},
		{[]string{"the", "server", "is", "doomed"}, []string{"the", "server", "is", "doomed"}},
		{[]string{"the", "of", "a"}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		got := TrimDangling(c.in)
		if len(got) != len(c.want) {
			t.Errorf("TrimDangling(%v) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("TrimDangling(%v) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

// TestTheNameItselfIsASeed, in the two cases where the tier can actually decide anything.
//
// The first version of this test asserted that a prompt of "greg" seeds on "greg", and it PASSED
// with the whole tier deleted: a recognized name is usually also a prompt word, so tier 1 had
// already added it at 30.0 and the assertion was measuring tier 1. Caught by reverting, which is
// the fifth time that has caught a test here whose name promised more than it checked.
func TestTheNameItselfIsASeed(t *testing.T) {
	// CASE 1: the canonical spelling, which tier 1 cannot reach because the user typed a
	// nickname. This is the case only this tier can serve.
	t.Run("canonical spelling the user did not type", func(t *testing.T) {
		f := newFake()
		f.learn(5, "alice", "greg is coping again")
		f.learn(5, "bob", "greg is coping again")
		f.names["greg"] = true

		g := New(f, testParams(), seeded(7, 11))
		got := g.Seed(SeedInput{
			// "birdlover" is what was typed and appears in no n-gram, so tier 1 has nothing.
			PromptWords: []string{"birdlover"},
			Names:       []string{"greg"},
			NameTokens:  []string{"greg", "birdlover"},
		})
		if got != "greg" {
			t.Errorf("Seed = %q, want greg: the person named resolves to a canonical spelling "+
				"the prompt never contained, and no other tier can offer it", got)
		}
	})

	// CASE 2: outranking an ordinary prompt word. Both are single prompt words, so tier 1
	// offers both at 30.0 and only this tier separates them.
	t.Run("outranks an ordinary prompt word", func(t *testing.T) {
		f := newFake()
		f.learn(5, "alice", "greg is coping again")
		f.learn(5, "bob", "greg is coping again")
		f.learn(5, "alice", "everyone is watching this")
		f.learn(5, "bob", "everyone is watching this")
		f.names["greg"] = true

		g := New(f, testParams(), seeded(13, 17))
		in := SeedInput{
			PromptWords: []string{"greg", "everyone"},
			Names:       []string{"greg"},
			NameTokens:  []string{"greg"},
		}

		// Weighted draw, not argmax, so this is a distribution claim: 40 against 30 is about
		// 57 percent, and without the tier it would be an even split.
		const runs = 2000
		name := 0
		for range runs {
			if g.Seed(in) == "greg" {
				name++
			}
		}
		if name <= runs/2 {
			t.Errorf("the name was seeded %d/%d times, no better than an even split against an "+
				"ordinary prompt word", name, runs)
		}
	})
}

// TestTheNameSeedIsExemptFromTheAuthorGate.
//
// Deliberate, and the reasoning is not inherited by analogy: the seed contributes ONE token and
// every step after it is still filtered by eligible(), so repeating a username teaches the bot
// no sentence and there is no poisoning vector to close. Without the exemption the tier would be
// dead on exactly the young corpus that needs it most, since almost nothing has two authors yet.
//
// Uses the nickname case for the same reason as above: if the name were also a prompt word, tier
// 1 would supply it and tier 1 is exempt too, so the test would prove nothing about this tier.
func TestTheNameSeedIsExemptFromTheAuthorGate(t *testing.T) {
	f := newFake()

	// One author only, so the gate would refuse this if it applied.
	for range 10 {
		f.learn(3, "onlyperson", "greg is coping")
	}
	f.names["greg"] = true

	g := New(f, Params{MaxNGram: 3, MinDistinctAuthors: 2}, DefaultSource{})
	got := g.Seed(SeedInput{
		PromptWords: []string{"birdlover"},
		Names:       []string{"greg"},
		NameTokens:  []string{"greg", "birdlover"},
	})
	if got != "greg" {
		t.Errorf("Seed = %q, want greg: a name the user invoked is prompt-derived and exempt", got)
	}
}

// TestALoneFunctionWordIsNotASeed. Opening a reply on "is" or "of" can only read as though the
// first half went missing, and the golden samples had exactly that: "is peak bird behaviour
// honestly". Only tier 1 can produce it, because the association writers exclude stop words.
func TestALoneFunctionWordIsNotASeed(t *testing.T) {
	f := newFake()
	// "is" has continuations, so without the check it is a perfectly good tier 1 candidate.
	f.learn(5, "alice", "is peak bird behaviour")
	f.learn(5, "bob", "is peak bird behaviour")

	g := New(f, testParams(), seeded(1, 2))
	for range 200 {
		if got := g.Seed(SeedInput{PromptWords: []string{"is"}}); got == "is" {
			t.Fatal("seeded on the bare function word \"is\"")
		}
	}

	// The control: a multi-word key starting with the same stop word is fine, because "is
	// peak" opens acceptably and it is the word standing alone that is the problem.
	found := false
	for range 200 {
		if strings.HasPrefix(g.Seed(SeedInput{PromptWords: []string{"is", "peak"}}), "is ") {
			found = true
			break
		}
	}
	if !found {
		t.Error("a multi-word prompt phrase beginning with a stop word was rejected too, so " +
			"the check is broader than intended")
	}
}

// TestAssocWeightCannotEscapeItsTier is the pin for the unbounded-bonus bug.
//
// Each association tier used to add a bare math.Sqrt(count) to its base, so at 50 recorded
// co-occurrences the name-topic tier reached 32 and outranked the 30 a word the user actually
// typed gets. That is finding G2 in the seed selector: evidence with no cap turns a ladder into
// whatever the counts happen to say.
func TestAssocWeightCannotEscapeItsTier(t *testing.T) {
	for _, count := range []uint64{0, 1, 5, 50, 5_000, 1 << 40} {
		for _, pos := range []float64{0.0, 0.5, 1.0} {
			d := corpus.TopicAssoc{Count: count, PosSum: pos * float64(count)}
			// Closed at the top: an enormous count saturates tanh at 1.0 and a position of
			// 0.0 gives the full position share, so base+assocSpread is attainable. That is
			// fine, and the band check below is what makes it safe.
			got := assocWeight(weightTopicWord, d)
			if got < weightTopicWord || got > weightTopicWord+assocSpread {
				t.Errorf("assocWeight(count=%d, pos=%.1f) = %.4f, outside [%.1f, %.1f]",
					count, pos, got, weightTopicWord, weightTopicWord+assocSpread)
			}
		}
	}

	// And the bands genuinely do not touch, which is what makes the documented ordering real.
	ladder := []float64{weightTwoHop, weightNamePositional, weightTopicWord, weightNameTopic}
	for i := 1; i < len(ladder); i++ {
		if ladder[i-1]+assocSpread > ladder[i] {
			t.Errorf("tier at %.1f overlaps the tier at %.1f given a spread of %.1f",
				ladder[i-1], ladder[i], assocSpread)
		}
	}
	// The name seed has to clear a single prompt word or it is dominated by tier 1 for the
	// same token, which is exactly how weightPromptWord came to be dead code.
	if weightNameSeed <= weightPromptNgram {
		t.Errorf("weightNameSeed %.1f does not clear a single prompt word at %.1f, so the tier "+
			"cannot win for a name that is also a prompt word, which is all of them",
			weightNameSeed, weightPromptNgram)
	}
	if weightNameSeed >= 2*weightPromptNgram {
		t.Errorf("weightNameSeed %.1f outranks a two-word prompt phrase at %.1f; a phrase "+
			"somebody typed carries what they said as well as who they meant",
			weightNameSeed, 2*weightPromptNgram)
	}
}
