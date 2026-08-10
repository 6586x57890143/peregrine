package markov

import (
	"fmt"
	"math"
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
			out = append(out, g.Seed(SeedInput{PromptWords: []string{"the"}, RecentMessages: [][]string{[]string{"alpha", "beta"}}}))
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

// TestTrimDanglingHasTwoBands.
//
// The caller passes whether the model CHOSE to end, because for most function words the two
// endings are different claims: an end sentinel means somebody really did finish a message on
// that word, and in this register that is worth keeping even when it looks abrupt.
//
// But that is true of the TOKEN and not of the CONSTRUCTION, and a preposition is the case
// where the difference bites. Two people ended a message with "what are you talking about",
// so "about" is attested before the sentinel; generation cashed that in on "nurock is coping
// about", where the phrase never arrives. Nearly a third of golden samples ended this way and
// every existing assertion passed, which is why the fixture-driven harness found it and no
// unit test did.
//
// So: text.IsDanglingTail trims either way, everything else only when the chain ran out.
func TestTrimDanglingHasTwoBands(t *testing.T) {
	cases := []struct {
		name  string
		in    []string
		chose bool
		want  []string
	}{
		{"ran out, trims the whole tail",
			[]string{"back", "to", "the"}, false, []string{"back"}},

		// Stops at "even", which is not a function word. "what is the point of even" is a
		// perfectly good chat line, which is the point: this trims a dangle, it does not
		// try to make the sentence grammatical.
		{"ran out, stops at the first content word",
			[]string{"what", "is", "the", "point", "of", "even", "a"}, false,
			[]string{"what", "is", "the", "point", "of", "even"}},

		{"ran out, nothing to trim",
			[]string{"the", "server", "is", "doomed"}, false,
			[]string{"the", "server", "is", "doomed"}},

		{"ran out, trims to nothing", []string{"the", "of", "a"}, false, nil},
		{"empty", nil, false, nil},

		// The band that had to be carved out. The model chose to end here, and the old rule
		// kept it for that reason.
		{"chose to end, a preposition still goes",
			[]string{"nurock", "is", "coping", "about"}, true,
			[]string{"nurock", "is", "coping"}},

		// The other half of the asymmetry, which must survive: a pronoun genuinely ends a
		// sentence in this register, so an attested ending on one is kept.
		{"chose to end, a pronoun is kept",
			[]string{"i", "am", "going", "to", "lose", "it"}, true,
			[]string{"i", "am", "going", "to", "lose", "it"}},

		// And the same sentence when the chain merely ran out loses it, because then nobody
		// attested the ending at all.
		{"ran out, the same pronoun goes",
			[]string{"i", "am", "going", "to", "lose", "it"}, false,
			[]string{"i", "am", "going", "to", "lose"}},
	}
	for _, c := range cases {
		got := TrimDangling(c.in, c.chose)
		if len(got) != len(c.want) {
			t.Errorf("%s: TrimDangling(%v, %v) = %v, want %v", c.name, c.in, c.chose, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: TrimDangling(%v, %v) = %v, want %v", c.name, c.in, c.chose, got, c.want)
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

	// CASE 2: the share does not collapse as the prompt gets longer.
	//
	// This subtest used to assert that the name beats a single ordinary prompt word, and it
	// passed for the whole of M14 while the tier was effectively decorative in production.
	// That is finding 36 in one sentence: with a two-word prompt the old weights gave the
	// name 57%, and with a seven-word prompt they gave it 3%, because the prompt tier's
	// influence was its weight TIMES its candidate count. The test asserted the good case and
	// nothing asserted the bad one.
	//
	// So the claim worth pinning is not "beats one word", it is STABILITY: the name tier
	// spends a fixed share of the draw however many prompt candidates there are. Under
	// budgets a short prompt gives it less than the old weights did, and that is the trade
	// being made deliberately.
	t.Run("its share survives a long prompt", func(t *testing.T) {
		f := newFake()
		for _, a := range []string{"alice", "bob"} {
			f.learn(5, a, "greg is coping again")
			f.learn(5, a, "everyone is watching this")
			f.learn(5, a, "what do you know about it")
			f.learn(5, a, "do you know what i mean")
		}
		f.names["greg"] = true

		measure := func(prompt []string) float64 {
			in := SeedInput{PromptWords: prompt, Names: []string{"greg"}, NameTokens: []string{"greg"}}
			const runs = 4000
			name := 0
			for i := range runs {
				g := New(f, testParams(), seeded(uint64(i), 17))
				if g.Seed(in) == "greg" {
					name++
				}
			}
			return float64(name) / float64(runs)
		}

		short := measure([]string{"greg", "everyone"})
		long := measure(strings.Fields("what do you know about greg everyone"))
		t.Logf("name share: short prompt %.1f%%, long prompt %.1f%%", short*100, long*100)

		for _, c := range []struct {
			name  string
			share float64
		}{{"short prompt", short}, {"long prompt", long}} {
			if c.share < 0.20 || c.share > 0.45 {
				t.Errorf("%s seeded the name %.1f%% of the time, want roughly a third; the tier "+
					"must not depend on how many prompt windows happen to exist", c.name, c.share*100)
			}
		}

		// The point of the whole change: the two must not diverge the way they used to.
		if math.Abs(short-long) > 0.15 {
			t.Errorf("name share moved from %.1f%% to %.1f%% purely because the prompt got "+
				"longer, which is finding 36 returning", short*100, long*100)
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
// TestASeedDoesNotOpenOnAConjunction.
//
// The lone-function-word rule below exempts multi-word windows, because "the bird" and "the
// server is" open perfectly well. That exemption let "and lachy are" through, from the
// prompt "greg and lachy are both", and it generated "and lachy are you know what i am going
// to lose it": a reply whose first half is visibly missing.
//
// Found by reading golden samples. No assertion would have caught it, because the seed was a
// real prompt window with real continuations and the sentence that followed was perfectly
// well-formed Markov output.
func TestASeedDoesNotOpenOnAConjunction(t *testing.T) {
	f := newFake()
	for _, a := range []string{"alice", "bob"} {
		f.learn(5, a, "and lachy are both in queue")
		f.learn(5, a, "lachy is malding again")
	}

	g := New(f, testParams(), seeded(3, 5))
	in := SeedInput{PromptWords: strings.Fields("greg and lachy are both")}

	for range 300 {
		seed := g.Seed(in)
		if strings.HasPrefix(seed, "and") {
			t.Fatalf("seeded on %q, which opens on a conjunction and promises a clause that "+
				"was never there", seed)
		}
	}

	// The control, so this is measuring the conjunction and not a seed selector that
	// returns nothing at all for this prompt.
	found := false
	for range 300 {
		if strings.HasPrefix(g.Seed(in), "lachy") {
			found = true
			break
		}
	}
	if !found {
		t.Error("no seed starting at lachy was ever drawn, so the test above passes vacuously")
	}
}

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

// TestAssocScoreStaysWithinItsTier pins the within-tier half of the bound.
//
// The between-tier half is now structural rather than arithmetic: a candidate is normalized
// into its tier's budget, so it cannot escape the tier no matter what this returns. This is
// what let assocSpread be deleted (finding 36's generalization of finding 34), and what is
// left to check is that one candidate cannot dominate its own tier's share.
func TestAssocScoreStaysWithinItsTier(t *testing.T) {
	for _, count := range []uint64{0, 1, 5, 50, 5_000, 1 << 40} {
		for _, pos := range []float64{0.0, 0.5, 1.0} {
			d := corpus.TopicAssoc{Count: count, PosSum: pos * float64(count)}
			got := assocScore(d)
			if got < 1.0 || got > 2.0 {
				t.Errorf("assocScore(count=%d, pos=%.1f) = %.4f, outside [1, 2]", count, pos, got)
			}
		}
	}
}

// TestSeedTierBudgetsHoldWhateverTheCandidateCount is the direct pin for finding 36, and it
// fails hard against the pre-budget implementation.
//
// The defect was that drawSeed samples proportional to weight over ALL candidates, so a
// tier's influence was its weight TIMES its candidate count. One tier with two hundred
// candidates buried a tier with one, however the two weights compared. This asserts the
// property that makes the documented ladder true: a tier's share of the draw is its budget,
// full stop, and the number of candidates carrying it changes nothing.
func TestSeedTierBudgetsHoldWhateverTheCandidateCount(t *testing.T) {
	c := newSeedCands()

	// One name candidate against two hundred prompt candidates, which is the shape that
	// produced 4.57% in the live measurement.
	c.add(tierName, "greg", 1.0)
	for i := range 200 {
		c.add(tierPromptNgram, fmt.Sprintf("w%d", i), 1.0)
	}

	w := c.weights()

	var nameMass, promptMass float64
	for k, v := range w {
		if k == "greg" {
			nameMass += v
			continue
		}
		promptMass += v
	}

	if math.Abs(nameMass-seedBudget[tierName]) > 1e-9 {
		t.Errorf("name tier got %.4f of the draw, want its budget %.1f", nameMass, seedBudget[tierName])
	}
	if math.Abs(promptMass-seedBudget[tierPromptNgram]) > 1e-9 {
		t.Errorf("prompt tier got %.4f, want its budget %.1f", promptMass, seedBudget[tierPromptNgram])
	}

	share := nameMass / (nameMass + promptMass)
	if share < 0.30 || share > 0.36 {
		t.Errorf("one name against two hundred prompt candidates draws %.1f%%, want about a third",
			share*100)
	}
}

// TestSeedResolvesANameToTheNameTier.
//
// A recognized name is almost always ALSO a prompt unigram. If the prompt tier claimed it
// first the name tier would be empty on exactly the prompts it exists for, and its budget
// would go unspent: finding 34's "a weight below the tier that already covers your keys is
// not a weak preference, it is no preference", restated for budgets.
func TestSeedResolvesANameToTheNameTier(t *testing.T) {
	c := newSeedCands()
	c.add(tierPromptNgram, "greg", 1.0)
	c.add(tierName, "greg", 1.0)

	if got := c.tierOf["greg"]; got != tierName {
		t.Errorf("a name that is also a prompt word landed in tier %d, want the name tier", got)
	}
	if len(c.byTier[tierPromptNgram]) != 0 {
		t.Error("the prompt tier kept a copy, so the key would be counted in two budgets")
	}
}

// TestSeedRedrawDoesNotDonateARejectedCandidatesShare.
//
// Under a flat weight map, deleting a rejected candidate hands its share to every remaining
// candidate in proportion, which silently moves mass between TIERS. If the name tier's only
// candidate fails attestation the name tier should contribute nothing, not give a quarter of
// the draw to the prompt tier.
func TestSeedRedrawDoesNotDonateARejectedCandidatesShare(t *testing.T) {
	c := newSeedCands()
	c.add(tierName, "greg", 1.0)
	c.add(tierPromptNgram, "the bird", 1.0)
	c.add(tierTopicWord, "cope", 1.0)

	c.drop("greg")
	w := c.weights()

	if _, ok := w["greg"]; ok {
		t.Fatal("dropped candidate is still in the draw")
	}
	if math.Abs(w["the bird"]-seedBudget[tierPromptNgram]) > 1e-9 {
		t.Errorf("prompt tier got %.4f after an unrelated drop, want its unchanged budget %.1f",
			w["the bird"], seedBudget[tierPromptNgram])
	}
	if math.Abs(w["cope"]-seedBudget[tierTopicWord]) > 1e-9 {
		t.Errorf("topic-word tier got %.4f after an unrelated drop, want its unchanged budget %.1f",
			w["cope"], seedBudget[tierTopicWord])
	}
}

// TestSeedDrawsTheNameOftenEnoughToNotice is the measurable form of the goal this milestone
// exists for: a reply should engage the person under discussion roughly one time in three,
// against the 4.57% that was measured before.
//
// A band rather than a point, because the share depends on which tiers have candidates at
// all, and that is the mechanism working: an empty tier spends nothing, so a starved corpus
// gives the name tier a larger share than a full one. Both ends of that range are inside this
// band on purpose.
func TestSeedDrawsTheNameOftenEnoughToNotice(t *testing.T) {
	f := goldenCorpus()
	p := testParams()
	p.MinDistinctAuthors = 2

	in := SeedInput{
		PromptWords: strings.Fields("bird what do you know about greg"),
		Names:       []string{"greg"},
		NameTokens:  []string{"greg"},
	}

	const runs = 20000
	hits := 0
	for i := range runs {
		g := New(f, p, seeded(uint64(i), 0x5EED))
		if g.Seed(in) == "greg" {
			hits++
		}
	}

	share := float64(hits) / float64(runs)
	t.Logf("name drawn as the seed %.1f%% of the time", share*100)
	if share < 0.18 || share > 0.40 {
		t.Errorf("name seed share %.1f%%, want roughly a quarter to a third; before the budget "+
			"change this was 4.6%% and the tier was effectively decorative", share*100)
	}
}

// TestEverySeedTierProducesASurvivingCandidate is the pin that was missing when finding 34
// and finding 37 each happened.
//
// Both were dead tiers, found by reading rather than by a failure, and both were dead for the
// same reason: another tier already covered every key they could produce. A third one now
// becomes a test failure instead of a code review.
func TestEverySeedTierProducesASurvivingCandidate(t *testing.T) {
	f := goldenCorpus()
	g := New(f, testParams(), seeded(1, 2))

	in := SeedInput{
		PromptWords:    strings.Fields("bird what do you know about greg"),
		RecentMessages: [][]string{strings.Fields("the queue is cooked honestly")},
		Names:          []string{"greg"},
		NameTokens:     []string{"greg"},
	}

	c := g.collectSeedCands(in, map[string]struct{}{})
	names := map[seedTier]string{
		tierName: "name", tierPromptNgram: "prompt-ngram", tierNameTopic: "name-topic",
		tierTopicWord: "topic-word", tierTwoHop: "two-hop", tierRecent: "recent",
	}
	for tier := range numSeedTiers {
		if len(c.byTier[tier]) == 0 {
			t.Errorf("tier %q contributed no candidate, so it cannot decide anything and is "+
				"either dead or shadowed by a higher tier (findings 34 and 37)", names[tier])
		}
	}
}

// TestTheRecentTierHasTheSameOpenerRulesAsThePrompt is the pin for finding 47.
//
// The prompt tier refused a lone function word and refused any window opening on a conjunction;
// the recent tier had neither check, so conversation memory could seed a reply on "is" or
// "and" while the identical window from the prompt was refused. The two are now one function,
// because the rule belongs to the question "may a reply start here", which is not a property
// of where the words came from.
func TestTheRecentTierHasTheSameOpenerRulesAsThePrompt(t *testing.T) {
	f := newFake()
	for _, a := range []string{"alice", "bob"} {
		f.learn(5, a, "is loose in the server")
		f.learn(5, a, "and lachy are both here")
		f.learn(5, a, "lachy is malding again")
	}

	g := New(f, testParams(), seeded(11, 13))
	in := SeedInput{
		RecentMessages: [][]string{
			strings.Fields("is loose in the server"),
			strings.Fields("and lachy are both here"),
		},
	}

	for range 300 {
		seed := g.Seed(in)
		if seed == "is" {
			t.Fatal("seeded on a lone function word from conversation memory")
		}
		if strings.HasPrefix(seed, "and") {
			t.Fatalf("seeded on %q, which opens on a conjunction", seed)
		}
	}
}

// TestARecentWindowDoesNotSpanTwoMessages.
//
// The recent tier forms n-gram windows, and the old flat slice had no message boundary in it,
// so a window could join the tail of one message to the head of the next: a phrase nobody said.
func TestARecentWindowDoesNotSpanTwoMessages(t *testing.T) {
	f := newFake()
	for _, a := range []string{"alice", "bob"} {
		// "charlie delta" exists only as a cross-message join, never as a real phrase.
		f.learn(5, a, "alpha bravo charlie")
		f.learn(5, a, "delta echo foxtrot")
		f.learn(5, a, "charlie delta something")
	}

	g := New(f, testParams(), seeded(3, 7))
	in := SeedInput{RecentMessages: [][]string{
		strings.Fields("alpha bravo charlie"),
		strings.Fields("delta echo foxtrot"),
	}}

	for range 300 {
		if seed := g.Seed(in); strings.Contains(seed, "charlie delta") {
			t.Fatalf("seeded on %q, which joins the end of one message to the start of the next", seed)
		}
	}
}

// TestRecalledNamesSteerButDoNotSeed.
//
// The rule memory of people obeys: what the channel was recently discussing should colour what
// the bot talks about, and starting a reply AT somebody nobody just mentioned reads as a
// non-sequitur rather than as memory. So a recalled name reaches the association tier and never
// the name seed tier.
func TestRecalledNamesSteerButDoNotSeed(t *testing.T) {
	f := newFake()
	for _, a := range []string{"alice", "bob"} {
		f.learn(5, a, "greg is coping again")
		f.learn(5, a, "cope is all he does")
	}
	f.names["greg"] = true
	for range 6 {
		f.addNameTopic("greg", "cope", 0.2)
	}

	g := New(f, testParams(), seeded(5, 9))
	in := SeedInput{
		PromptWords:   []string{"what"},
		RecalledNames: []string{"greg"},
	}

	sawTopic := false
	for range 400 {
		seed := g.Seed(in)
		if seed == "greg" {
			t.Fatal("seeded ON a recalled name: the bot would address somebody nobody just mentioned")
		}
		if seed == "cope" {
			sawTopic = true
		}
	}
	if !sawTopic {
		t.Error("a recalled name contributed no association candidate either, so recall does nothing")
	}
}
