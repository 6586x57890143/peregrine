package markov

import (
	"strings"
	"testing"
)

// The seed tier the trace records has to be the tier that actually produced the seed. It
// is the field the whole export leans on: two seed tiers have shipped dead in this repo
// (findings 34 and 37) and a per-tier share of REAL draws is what would have shown either
// one on the first day rather than on a code read months later. A trace that mislabels the
// tier is worse than no trace, because it would make a dead tier look alive.
func TestTheTraceRecordsTheTierThatActuallyWon(t *testing.T) {
	f := goldenCorpus()
	p := testParams()
	p.MinDistinctAuthors = 2

	in := SeedInput{
		PromptWords: strings.Fields("bird what do you know about greg"),
		Names:       []string{"greg"},
		NameTokens:  []string{"greg"},
	}

	// Enough draws to visit several tiers, since one seeded run only ever exercises one.
	const runs = 300
	seen := map[string]int{}
	for i := range runs {
		g := New(f, p, seeded(uint64(i), 0x5EED))

		var tr Trace
		traced := in
		traced.Trace = &tr

		seed := g.Seed(traced)
		if seed == "" {
			continue
		}

		if tr.SeedKey != seed {
			t.Fatalf("run %d: trace recorded seed key %q, want %q", i, tr.SeedKey, seed)
		}
		if tr.SeedTier == "" {
			t.Fatalf("run %d: seed %q was drawn with no tier recorded", i, seed)
		}
		if tr.SeedTier == "unknown" {
			t.Fatalf("run %d: seed %q recorded as an unnamed tier, which means a tier was "+
				"added without a String case", i, seed)
		}

		// The name tier is the one that can be checked independently: "greg" is a
		// recognized name, and tierName precedes tierPromptNgram precisely so that the
		// prompt tier does not claim it.
		if seed == "greg" && tr.SeedTier != "name" {
			t.Errorf("run %d: the recognized name seeded from tier %q, want \"name\"; the tier "+
				"order in seedTier exists to stop the prompt tier claiming it", i, tr.SeedTier)
		}
		seen[tr.SeedTier]++
	}

	if len(seen) < 2 {
		t.Errorf("only %d distinct tier(s) recorded across %d draws (%v); this test cannot "+
			"tell a correct label from a constant one", len(seen), runs, seen)
	}
	t.Logf("tier shares over %d draws: %v", runs, seen)
}

// A nil trace has to change nothing at all. It is the production path whenever the export
// is off, and the reason every method on Trace guards its own receiver: the alternative is
// a nil check at each call site, and the one that gets forgotten is inside the innermost
// loop of generation.
func TestANilTraceChangesNothing(t *testing.T) {
	f := goldenCorpus()
	p := testParams()

	in := SeedInput{
		PromptWords: strings.Fields("bird what do you know about greg"),
		Names:       []string{"greg"},
		NameTokens:  []string{"greg"},
	}

	for i := range 50 {
		withNil := New(f, p, seeded(uint64(i), 7)).Seed(in)

		traced := in
		traced.Trace = &Trace{}
		withTrace := New(f, p, seeded(uint64(i), 7)).Seed(traced)

		if withNil != withTrace {
			t.Fatalf("run %d: tracing changed the seed, %q with a trace against %q without",
				i, withTrace, withNil)
		}
	}
}

// The two ways a step can produce nothing are different problems with different fixes, and
// the split is most of why the trace is worth having. A dead end on an unseen prefix means
// the corpus is sparse; a STARVED step means PEREGRINE_MIN_DISTINCT_AUTHORS ended the
// sentence, which is the gate working and is the single most likely reason a freshly
// deployed bot is quiet. An operator has never been able to tell which one they have.
func TestTheTraceTellsAStarvedStepFromADeadEnd(t *testing.T) {
	// One author says a phrase repeatedly. Its continuations exist and none of them can
	// pass a threshold of two, which is exactly the young-corpus case.
	f := newFake()
	for range 5 {
		f.learn(3, "solo", "the queue is cooked")
	}

	p := testParams()
	p.MinDistinctAuthors = 2
	g := New(f, p, seeded(1, 2))

	var starved Trace
	step := &Step{Prefix: []string{"the", "queue"}, Trace: &starved}
	if next, err := g.Next(step); err != nil || next != "" {
		t.Fatalf("Next = %q, %v; want a dead end for a phrase only one author said", next, err)
	}
	if starved.Starved != 1 {
		t.Errorf("Starved = %d, want 1: candidates existed and the gate removed all of them",
			starved.Starved)
	}
	if starved.GateRefused == 0 {
		t.Error("GateRefused = 0, but the gate is what emptied the set")
	}

	// A prefix the corpus has never seen has no candidates for the gate to refuse.
	var unseen Trace
	step = &Step{Prefix: []string{"nothing", "here"}, Trace: &unseen}
	if next, err := g.Next(step); err != nil || next != "" {
		t.Fatalf("Next = %q, %v; want a dead end on an unseen prefix", next, err)
	}
	if unseen.DeadEnds != 1 {
		t.Errorf("DeadEnds = %d, want 1", unseen.DeadEnds)
	}
	if unseen.Starved != 0 {
		t.Errorf("Starved = %d on an unseen prefix, want 0: there was nothing to refuse",
			unseen.Starved)
	}
}

// MinOrder is the number SPEC.md section 10 leaves open: low-order joins read as nonsense
// and the open question is whether that is a fixture-size artifact. It has to record the
// SHORTEST context that actually supplied candidates, not the longest one tried.
func TestMinOrderRecordsHowFarTheBackoffWent(t *testing.T) {
	f := newFake()
	// Two authors, so the gate admits the continuations.
	for _, who := range []string{"a", "b"} {
		f.learn(3, who, "the queue is cooked")
		f.learn(3, who, "everyone is cooked honestly")
	}

	p := testParams()
	p.MinDistinctAuthors = 2
	g := New(f, p, seeded(3, 4))

	// "nowhere is" has never been seen as a two-word context, so enumeration has to fall
	// back to the one-word context "is", which the corpus does know.
	var tr Trace
	step := &Step{Prefix: []string{"nowhere", "is"}, Trace: &tr}
	next, err := g.Next(step)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if next == "" {
		t.Fatal("Next found nothing; this test needs the one-word backoff to succeed")
	}
	if tr.MinOrder != 1 {
		t.Errorf("MinOrder = %d, want 1: the two-word context is unseen so only the one-word "+
			"context can have supplied this candidate", tr.MinOrder)
	}
	if tr.Steps != 1 {
		t.Errorf("Steps = %d, want 1", tr.Steps)
	}
	if tr.MeanCandidates() <= 0 {
		t.Error("MeanCandidates = 0 after a step that produced a token")
	}
}

// MeanCandidates must be a mean rather than a total. A set of one is a deterministic step
// however hot the sampler is, which was most of what made the previous engine feel canned,
// and it is invisible in the output because the seed still varies.
func TestMeanCandidatesAveragesAndSurvivesAnEmptyTrace(t *testing.T) {
	var tr Trace
	if got := tr.MeanCandidates(); got != 0 {
		t.Errorf("MeanCandidates on an empty trace = %v, want 0", got)
	}

	var nilTrace *Trace
	if got := nilTrace.MeanCandidates(); got != 0 {
		t.Errorf("MeanCandidates on a nil trace = %v, want 0", got)
	}

	tr.step(2, 4, 0)
	tr.step(2, 6, 1)
	if got := tr.MeanCandidates(); got != 5 {
		t.Errorf("MeanCandidates = %v after sets of 4 and 6, want 5", got)
	}
	if tr.GateRefused != 1 {
		t.Errorf("GateRefused = %d, want 1", tr.GateRefused)
	}
}

// Every tier has to have a name. An unnamed tier lands on "unknown", which would show up in
// an archive as a share that cannot be attributed to anything.
func TestEverySeedTierIsNamed(t *testing.T) {
	seen := map[string]bool{}
	for tier := tierName; tier < numSeedTiers; tier++ {
		name := tier.String()
		if name == "unknown" {
			t.Errorf("seed tier %d has no name; add a case to seedTier.String", int(tier))
			continue
		}
		if seen[name] {
			t.Errorf("two seed tiers share the name %q, so their shares cannot be told apart", name)
		}
		seen[name] = true
	}
}
