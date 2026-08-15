package markov

import (
	"math"

	"testing"
)

// The author-diversity gate grew a second and a third dimension in M24, and this file is
// the account of what each one is for. The gate is a safety control, so every test here is
// about a case where it must still refuse, except the two that are about the cost the
// measurement in SPEC.md finding 54 put a number on.

// The change M24 exists to make. Before it, an edge one person said once was refused
// exactly as hard as an edge one person said five hundred times, and on a real corpus that
// meant refusing 93.8% of the edges and 76.3% of the probability mass: 0.93 admissible
// continuations per prefix at order 1, which is a deterministic walk however hot the
// sampler is.
func TestASingleAuthorEdgeIsUsableWhileItCarriesNoWeight(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")

	p := testParams()
	p.MinDistinctAuthors = 2
	g := New(f, p, seeded(1, 2))

	got, err := g.Next(newStep([]string{"bird", "is"}))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != "loose" {
		t.Errorf("got %q, want \"loose\": one author said it once, which is a sparse corpus "+
			"rather than the concentration A6 describes", got)
	}
}

// The other half, and the reason the allowance keys on count rather than on anything else:
// weight in this model IS raw frequency, so the quantity that makes poisoning work is the
// quantity the allowance has to be measured in.
//
// TestAuthorDiversityGateRefusesAPhraseOneAuthorRepeated covers the same rule at 500
// repetitions. This one sits just over the line, because a control that only refuses the
// extreme case is not a control.
func TestASingleAuthorEdgeIsRefusedOnceItCarriesWeight(t *testing.T) {
	f := newFake()
	for range 3 {
		f.learn(5, "poisoner", "the bird is sayhorriblething")
	}

	p := testParams()
	p.MinDistinctAuthors = 2
	p.SoloRepeatLimit = 2
	g := New(f, p, seeded(1, 2))

	for range 50 {
		got, err := g.Next(newStep([]string{"bird", "is"}))
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got != "" {
			t.Fatalf("generated %q from a continuation one author produced three times "+
				"against a SoloRepeatLimit of 2", got)
		}
	}
}

// The order cap, which is the dimension the golden harness found and the corpus report could
// not have. A continuation reachable from a long context reproduces that much of somebody's
// original message, so admitting single-author edges at every order does not buy
// recombination, it buys recitation: measured on the golden fixture, allowing them at any
// order took the recitation rate from 2.4% to 19.8% against a 10% bar, AND lowered the
// share of distinct outputs, because a chain that follows one source message is not varying.
//
// TestGoldenSamplesAreNotRecitation is the pin that fails if this is removed. This test is
// the unit-level statement of the same rule.
func TestTheSingleAuthorAllowanceStopsAtLongContexts(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "somebody said a very distinctive sentence here")

	p := testParams()
	p.MinDistinctAuthors = 2
	p.SoloRepeatLimit = 2
	p.SoloMaxOrder = 2
	g := New(f, p, seeded(1, 2))

	// A four-word context: following it would reproduce five consecutive words of one
	// person's message, which is exactly recitationMinLen.
	got, err := g.Next(newStep([]string{"said", "a", "very", "distinctive"}))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != "" {
		t.Errorf("got %q from a four-word context only one person ever produced. The "+
			"allowance is for short shared contexts; a long one is somebody's sentence", got)
	}

	// The same corpus, the same author, at a context short enough to be vocabulary rather
	// than authorship.
	got, err = g.Next(newStep([]string{"very", "distinctive"}))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got == "" {
		t.Error("a two-word context produced nothing: the order cap has swallowed the " +
			"allowance entirely, which leaves the gate exactly where finding 54 found it")
	}
}

// The hole a count-keyed allowance could easily have opened, and the reason admissible
// requires at least one author rather than treating authors as a tie-break.
//
// An edge with ZERO authors is the bot's own output: learnMessage passes an empty author
// for its own messages specifically so self-learning cannot bootstrap a phrase into
// eligibility. The bot's replies re-enter the corpus through selfLearn, so an allowance
// that ignored authorship would make every sentence the bot produced admissible on its own
// authority, and the loop would tighten with every reply.
func TestTheAllowanceNeverAdmitsTheBotsOwnOutput(t *testing.T) {
	f := newFake()
	f.learn(5, "", "the bird is loose") // the bot, once: count 1, zero authors

	p := testParams()
	p.MinDistinctAuthors = 2
	p.SoloRepeatLimit = 2
	g := New(f, p, seeded(1, 2))

	for range 50 {
		got, err := g.Next(newStep([]string{"bird", "is"}))
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got != "" {
			t.Fatalf("generated %q from an edge with no attributed author at all. A count "+
				"allowance that ignores authorship lets the bot vouch for itself", got)
		}
	}
}

// The escape hatch, and it has to be exact rather than approximate: an operator turning the
// allowance off during an incident is asking for the control they read about in SPEC.md
// section 4, not for a slightly stricter version of the new one.
func TestSoloRepeatLimitZeroRestoresTheOlderGate(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")

	p := testParams()
	p.MinDistinctAuthors = 2
	p.SoloRepeatLimit = 0
	g := New(f, p, seeded(1, 2))

	got, err := g.Next(newStep([]string{"bird", "is"}))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != "" {
		t.Errorf("got %q with the allowance disabled: one author is one author, and 0 must "+
			"mean the pre-M24 gate rather than a smaller allowance", got)
	}
}

// Generation puts a word into a sentence from three places: the sampler, the dead-end jump
// and the seed. The gate has to mean the same thing at all three, and this repo has already
// shipped the version where it did not (finding 31, where the jump and the seed handed back
// phrases the sampler had refused at every step).
//
// So this asserts the rule rather than one caller's behaviour: attested must agree with
// admits about an identical edge.
func TestTheJumpAndTheSamplerAgreeAboutWhatIsAdmissible(t *testing.T) {
	cases := []struct {
		name    string
		count   uint64
		authors uint32
	}{
		{"one author, no weight", 1, 1},
		{"one author, weight", 40, 1},
		{"two authors, no weight", 1, 2},
		{"no author at all", 1, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			// One edge out of "pivot", built to the case's shape. The fake counts distinct
			// (prefix, next, author) triples exactly as storage does, so repeating an
			// author moves the count and not the author total.
			author := "alice"
			if tc.authors == 0 {
				author = ""
			}
			for range tc.count {
				f.learnNgram("pivot", "onward", author)
			}
			for i := uint32(1); i < tc.authors; i++ {
				f.learnNgram("pivot", "onward", string(rune('a'+i)))
			}

			p := testParams()
			p.MinDistinctAuthors = 2
			p.SoloRepeatLimit = 2
			p.SoloMaxOrder = 2
			g := New(f, p, seeded(1, 2))

			succ, err := f.Successors("pivot")
			if err != nil {
				t.Fatalf("Successors: %v", err)
			}
			if len(succ) != 1 {
				t.Fatalf("fixture built %d successors, want 1", len(succ))
			}

			sampler := g.admits(succ[0].Token, succ[0].Count, succ[0].Authors, 1)
			jump := g.attested("pivot")
			if sampler != jump {
				t.Errorf("the sampler admits=%v and the jump attests=%v for the same edge "+
					"(count %d, authors %d). Two answers to one safety question is finding "+
					"31's shape", sampler, jump, succ[0].Count, succ[0].Authors)
			}
		})
	}
}

// Finding 53. The learn path appends the end sentinel before the topic loop runs, so its
// topic count is exactly the number of messages learned: on a real corpus 19,387, which is
// 15.5% of all tokens and 5.7 times the next entry. Significance is tanh(sqrt(n)/12), which
// at that magnitude is 1.000 to three places, so the sentinel was collecting the full weight
// at every step while a median candidate collected a twelfth of it.
//
// That made Significance a second length model, and the length model's own comment says it
// is the only place length influences generation.
func TestTheEndSentinelCollectsNoSignificance(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")
	// The production shape: the sentinel is far and away the most frequent topic entry,
	// because every message contributes exactly one.
	for range 5000 {
		f.topics[EndToken]++
		f.topicTotal++
	}

	g := New(f, testParams(), seeded(1, 2))
	s := newStep([]string{"bird", "is"})
	assoc := g.loadAssoc(s)

	got := g.heuristics(s, candidate{token: EndToken}, assoc)
	// Every other term is zero for the sentinel on this fixture, and the length model is
	// applied separately below, so anything left is Significance leaking in.
	if math.Abs(got-s.Length.endLogit(DefaultWeights(), len(s.Sentence))) > 1e-9 {
		t.Errorf("the sentinel scored %.4f, which is not its length logit alone (%.4f). "+
			"Global word frequency must not reach the end token, or the corpus size is "+
			"quietly deciding how long sentences are",
			got, s.Length.endLogit(DefaultWeights(), len(s.Sentence)))
	}
}

// The other half of finding 53: the exemption must be exactly one token wide. A real word
// that happens to be frequent is what Significance is for.
func TestSignificanceStillRewardsAFrequentWord(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")
	for range 5000 {
		f.topics["loose"]++
		f.topicTotal++
	}

	g := New(f, testParams(), seeded(1, 2))
	s := newStep([]string{"bird", "is"})
	assoc := g.loadAssoc(s)

	got := g.heuristics(s, candidate{token: "loose"}, assoc)
	if got <= 0 {
		t.Errorf("a word seen five thousand times scored %.4f from the heuristics; the "+
			"sentinel exemption has taken the whole term with it", got)
	}
}
