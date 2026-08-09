package markov

import (
	"strings"
	"testing"
)

// TestAuthorDiversityGateRefusesAPhraseOneAuthorRepeated is the pin for A6, and it is
// the single most important safety assertion in this package.
//
// n-gram weight is raw frequency, so repeating a phrase is a direct write to the
// model: one determined user can teach the bot to say anything, and the corpus has
// already been poisoned this way once. The gate requires k distinct authors before a
// continuation is eligible to be GENERATED, independent of how often it was seen,
// which turns the attack from persistence into collusion.
//
// 500 repetitions by one author is the shape of the real attack, and it must produce
// nothing.
func TestAuthorDiversityGateRefusesAPhraseOneAuthorRepeated(t *testing.T) {
	f := newFake()
	for range 500 {
		f.learn(5, "poisoner", "the bird should sayhorriblething")
	}

	p := testParams()
	p.MinDistinctAuthors = 2
	g := New(f, p, seeded(1, 2))

	for range 200 {
		got, err := g.Next(newStep([]string{"bird", "should"}))
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got != "" {
			t.Fatalf("generated %q from a continuation taught 500 times by a single author. "+
				"Frequency must not buy eligibility, or the anti-poisoning control is a "+
				"speed bump (SPEC.md section 4, A6)", got)
		}
	}
}

// TestAuthorDiversityGateAllowsAPhraseTwoAuthorsSaid is the other half: the gate must
// not be so strict that a genuinely shared phrase is unusable, or the bot is silent on
// a real server rather than safe on one.
func TestAuthorDiversityGateAllowsAPhraseTwoAuthorsSaid(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")
	f.learn(5, "bob", "the bird is loose")

	p := testParams()
	p.MinDistinctAuthors = 2
	g := New(f, p, seeded(1, 2))

	got, err := g.Next(newStep([]string{"bird", "is"}))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != "loose" {
		t.Errorf("got %q, want \"loose\": two distinct authors said it, so it is eligible", got)
	}
}

// TestAuthorDiversityGateExcludesTheBotsOwnOutput pairs with the write-side pin in
// legacy. Self-learning feeds the bot's replies back into the corpus, so if the bot
// counted as an author, anything it said once would carry a diversity count of one from
// the moment it said it and bootstrap itself toward eligibility.
//
// learnMessage passes an empty author for the bot, and the fake mirrors that: an empty
// author adds nothing to the set.
func TestAuthorDiversityGateExcludesTheBotsOwnOutput(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")
	for range 50 {
		f.learn(5, "", "the bird is loose") // the bot repeating itself
	}

	p := testParams()
	p.MinDistinctAuthors = 2
	g := New(f, p, seeded(1, 2))

	got, err := g.Next(newStep([]string{"bird", "is"}))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != "" {
		t.Errorf("generated %q: fifty repetitions by the bot itself plus one human must not "+
			"reach two distinct authors, or self-learning bootstraps eligibility", got)
	}
}

// TestTheEndSentinelIsNotGatedOnAuthorDiversity covers a trap the gate could easily
// fall into: the sentinel is structural rather than content, and gating it would mean a
// sentence cannot end until several people happened to end a message the same way.
// That is a length bug wearing a safety hat.
func TestTheEndSentinelIsNotGatedOnAuthorDiversity(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose "+EndToken)

	p := testParams()
	p.MinDistinctAuthors = 5
	g := New(f, p, seeded(1, 2))

	s := newStep([]string{"is", "loose"})
	// The sentence has earned its ending: floor and target both already reached.
	s.Length = Length{Min: 0, Max: 18, Target: 0}
	got, err := g.Next(s)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got != EndToken {
		t.Errorf("got %q, want the end sentinel: it must pass the author gate even at a "+
			"threshold no content can meet", got)
	}
}

func TestNextOnAnUnknownPrefixIsADeadEndNotAnError(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")

	g := New(f, testParams(), seeded(1, 2))
	got, err := g.Next(newStep([]string{"nothing", "like", "this"}))
	if err != nil {
		t.Fatalf("an unknown prefix must not error, it is ordinary in a sparse corpus: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want an empty dead end. A fallback string would be a new output to "+
			"reason about", got)
	}
}

// TestHeuristicsAreBoundedEvenUnderPiledUpEvidence is the pin for finding G2, the
// reason the whole scoring layer was rewritten.
//
// Topic gravity used to be 1 + sum(sqrt(count) * posScore * significance) MULTIPLIED
// into an unnormalized score, with nothing capping the sum. A word associated strongly
// with many prompt topics could therefore outscore the model by orders of magnitude,
// which is how the sampler became argmax with noise. Here every unbounded sum is
// squashed, so no heuristic can exceed its own weight however much evidence piles on.
func TestHeuristicsAreBoundedEvenUnderPiledUpEvidence(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")

	// Twenty topics, each associating the candidate thousands of times.
	s := newStep([]string{"bird", "is"})
	for i := range 20 {
		topic := "topic" + string(rune('a'+i))
		s.CoreTopics[topic] = 10.0
		for range 2000 {
			f.addTopicWord(topic, "loose", 0.5)
		}
	}
	s.Position = 0.5

	g := New(f, testParams(), seeded(1, 2))
	assoc := g.loadAssoc(s)
	got := g.heuristics(s, candidate{token: "loose"}, assoc)

	// The sum of every positive weight is the ceiling. Anything above it means a term
	// is unbounded again.
	w := DefaultWeights()
	ceiling := w.TopicGravity + w.NameAssoc + w.CurrentTopic + w.Significance +
		w.IsName + w.Persona + w.RecentContext + w.PromptGravity +
		g.params.PromptRelevance + w.Connective

	if got > ceiling {
		t.Errorf("heuristics returned %.4f against a ceiling of %.4f. An unbounded term is "+
			"exactly what made the old sampler argmax with noise (SPEC.md finding G2)",
			got, ceiling)
	}
	if got <= 0 {
		t.Errorf("heuristics returned %.4f with strong topic evidence present; the term "+
			"should be positive, just bounded", got)
	}
}

// TestPersonaLexiconIsNotRebuiltPerCandidate is a structural check rather than a
// behavioural one, because the defect it guards is a cost defect and no assertion about
// output can see it. The roast vocabulary used to be a fourteen-entry map literal
// allocated inside the per-candidate loop, with fourteen lowercase conversions of
// constants that were already lower case.
func TestPersonaLexiconIsNotRebuiltPerCandidate(t *testing.T) {
	if len(roastLexicon) == 0 {
		t.Fatal("the roast lexicon is empty")
	}
	for word := range roastLexicon {
		if word != strings.ToLower(word) {
			t.Errorf("lexicon key %q is not pre-normalized, so it needs a conversion at "+
				"lookup time, which is the cost this hoisting removed", word)
		}
	}
}

func TestPersonaBiasAppliesOnlyToTheRoastPersona(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "you are cringe")
	f.learn(5, "bob", "you are fine")

	g := New(f, testParams(), seeded(1, 2))

	neutral := newStep([]string{"you", "are"})
	roast := newStep([]string{"you", "are"})
	roast.Persona = PersonaRoast

	assoc := g.loadAssoc(neutral)
	c := candidate{token: "cringe"}

	nLogit := g.heuristics(neutral, c, assoc)
	rLogit := g.heuristics(roast, c, assoc)

	if rLogit <= nLogit {
		t.Errorf("roast persona gave logit %.4f, neutral gave %.4f: the persona bias must "+
			"raise roast vocabulary", rLogit, nLogit)
	}
	if diff := rLogit - nLogit; diff > DefaultWeights().Persona+1e-9 {
		t.Errorf("persona contributed %.4f, which exceeds its weight %.4f", diff, DefaultWeights().Persona)
	}
}

// TestRepetitionPenaltyIsCappedSoMemesSurvive. Memetic repetition is the desired
// register here: a copypasta cadence, a doubled emote, "ratio ratio ratio". The penalty
// must suppress stuttering without growing without bound, or the bot cannot produce the
// thing it exists to produce.
func TestRepetitionPenaltyIsCappedSoMemesSurvive(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "ratio ratio ratio ratio")

	g := New(f, testParams(), seeded(1, 2))
	w := DefaultWeights()

	var last float64
	for uses := 1; uses <= 20; uses++ {
		s := newStep([]string{"ratio"})
		s.Used["ratio"] = uses
		got := g.heuristics(s, candidate{token: "ratio"}, g.loadAssoc(s))
		if got < w.RepetitionCap+w.ImmediateRepeat-1e-9 {
			t.Fatalf("at %d uses the penalty reached %.4f, below the cap %.4f plus the "+
				"immediate-repeat term %.4f. An uncapped penalty flattens the register",
				uses, got, w.RepetitionCap, w.ImmediateRepeat)
		}
		if uses > 5 && got != last {
			t.Errorf("at %d uses the penalty was still moving (%.4f to %.4f); it should have "+
				"reached its cap", uses, last, got)
		}
		last = got
	}
}
