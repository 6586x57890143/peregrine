package markov

import (
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/corpus"
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

	// The candidate is also the person under discussion and a learned name, so BOTH name
	// terms fire. Without this the test would pass just as well against an unbounded
	// PromptName, since the term simply would not be reached.
	s.PromptNames = map[string]struct{}{"loose": {}}
	f.names["loose"] = true

	g := New(f, testParams(), seeded(1, 2))
	assoc := g.loadAssoc(s)
	got := g.heuristics(s, candidate{token: "loose"}, assoc)

	// The sum of every positive weight is the ceiling. Anything above it means a term
	// is unbounded again.
	w := DefaultWeights()
	ceiling := w.TopicGravity + w.NameAssoc + w.CurrentTopic + w.Significance +
		w.IsName + w.PromptName + w.NameTopic + w.Persona + w.RecentContext +
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

// TestThePromptNameOutscoresABystander is the pin for the distinction IsName cannot make.
//
// IsName rewards every learned name equally, so a reply to "what is up with lachy" was as
// likely to name somebody else in the server as to name lachy. Answering a question about one
// person by naming a different one is the specific failure this closes, and it is invisible to
// any test that only checks a name gets some edge.
//
// Verified by reverting: drop the PromptNames term from heuristics and this fails.
func TestThePromptNameOutscoresABystander(t *testing.T) {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")
	f.learn(5, "bob", "the bird is loose")

	// Two learned names. Only one of them is who the message was about.
	f.names["lachy"] = true
	f.names["someoneelse"] = true

	s := newStep([]string{"the", "bird"})
	s.PromptNames = map[string]struct{}{"lachy": {}}

	g := New(f, testParams(), seeded(1, 2))
	assoc := g.loadAssoc(s)

	subject := g.heuristics(s, candidate{token: "lachy"}, assoc)
	bystander := g.heuristics(s, candidate{token: "someoneelse"}, assoc)

	if subject <= bystander {
		t.Errorf("the person under discussion scored %.4f against a bystander's %.4f. Both are "+
			"learned names, so IsName cannot separate them and only PromptName can",
			subject, bystander)
	}
}

// TestNameTopicRewardsAWordSaidAboutThePerson is the pin for finding 42.
//
// The fixture is built so ONLY the new term can move the candidate, and the exclusions are
// stated because SPEC.md section 5.4 records three tests that had to be corrected for passing
// for the wrong reason. "cope" is a word associated with the name; it is NOT a prompt word,
// NOT associated with any prompt word, and NOT reachable at two hops from the name, so
// TopicGravity, PromptRelevance, PromptName and NameAssoc are all zero on it.
func TestNameTopicRewardsAWordSaidAboutThePerson(t *testing.T) {
	f := newFake()
	for _, a := range []string{"alice", "bob"} {
		f.learn(5, a, "greg is cope")
		f.learn(5, a, "greg is fine")
	}
	f.names["greg"] = true

	g := New(f, testParams(), seeded(1, 2))

	step := newStep([]string{"greg", "is"})
	step.Position = 0.5
	// The direct association, and nothing else: no TopicWordsFor entry anywhere, so the
	// two-hop NameAssoc block cannot reach "cope" at all.
	step.NameAssoc = map[string]corpus.TopicAssoc{"cope": {Count: 9, PosSum: 9 * 0.5}}

	assoc := g.loadAssoc(step)
	withTerm := g.heuristics(step, candidate{token: "cope"}, assoc)
	control := g.heuristics(step, candidate{token: "fine"}, assoc)

	if withTerm <= control {
		t.Errorf("a word directly associated with the name scored %.4f against %.4f for one "+
			"that is not, so the direct name term is doing nothing", withTerm, control)
	}

	// And it must be the new term rather than something else: zeroing it collapses the gap.
	g.weights.NameTopic = 0
	if got := g.heuristics(step, candidate{token: "cope"}, assoc); got != control {
		t.Errorf("with NameTopic zeroed the two candidates still differ (%.4f vs %.4f), so this "+
			"test is measuring some other term", got, control)
	}
}

// TestNameTermsTogetherStayUnderPromptName.
//
// A candidate that is both one hop and two hops from the name collects NameTopic AND
// NameAssoc. At 0.75 + 0.45 that would be 1.20, above PromptName at 0.90, which would mean a
// merely associated word outranks the person's actual name. This is why NameAssoc came down
// to 0.30 in the same change rather than later.
func TestNameTermsTogetherStayUnderPromptName(t *testing.T) {
	w := DefaultWeights()
	if ceiling := w.NameTopic + w.NameAssoc; ceiling > w.PromptName+0.20 {
		t.Errorf("the two name-association terms sum to %.2f against PromptName at %.2f: a word "+
			"merely associated with somebody would outrank naming them", ceiling, w.PromptName)
	}
	if w.NameTopic <= w.NameAssoc {
		t.Errorf("the direct name term (%.2f) does not outrank its transitive form (%.2f), so "+
			"the ladder finding 42 exists to create is not there", w.NameTopic, w.NameAssoc)
	}
	if w.NameTopic <= w.TopicGravity {
		t.Errorf("the direct name term (%.2f) does not outrank one hop from an ordinary prompt "+
			"word (%.2f), though a name is more specific than any one word", w.NameTopic, w.TopicGravity)
	}
}

// TestCurrentTopicFiresOnAMultiWordSeed is the pin for finding 44, and it fails against a
// Step whose CurrentTopic is the raw seed.
//
// topic_word is keyed by single words, so TopicWordsFor("greg is coping") returns an empty
// non-nil map: it passes the nil guard in heuristics and never matches. Seeds are multi-word
// for roughly nine sentences in ten, so the 0.35 term was silently absent.
func TestCurrentTopicFiresOnAMultiWordSeed(t *testing.T) {
	f := newFake()
	for _, a := range []string{"alice", "bob"} {
		f.learn(5, a, "greg is coping about it")
	}
	for range 4 {
		f.addTopicWord("coping", "queue", 0.5)
	}

	const seed = "greg is coping"
	if topic := SeedTopic(seed); topic != "coping" {
		t.Fatalf("SeedTopic(%q) = %q, want the last content word", seed, topic)
	}

	g := New(f, testParams(), seeded(1, 2))
	step := newStep([]string{"greg", "is", "coping"})
	step.CurrentTopic = SeedTopic(seed)
	assoc := g.loadAssoc(step)

	with := g.heuristics(step, candidate{token: "queue"}, assoc)

	// The same Step with the raw seed, which is what the code used to do.
	raw := newStep([]string{"greg", "is", "coping"})
	raw.CurrentTopic = seed
	without := g.heuristics(raw, candidate{token: "queue"}, g.loadAssoc(raw))

	if with <= without {
		t.Errorf("CurrentTopic scored %.4f from the reduced topic against %.4f from the raw "+
			"multi-word seed; the term is still dead", with, without)
	}
}

// TestSeedTopicHandlesTheEdges.
func TestSeedTopicHandlesTheEdges(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"greg":              "greg",
		"the bird is loose": "loose",
		// All function words: returning the last beats returning the whole phrase, which
		// is guaranteed to miss.
		"of the": "the",
		// Trailing function words are skipped in favour of the last CONTENT word, because
		// that is the word whose associations describe the sentence.
		"coping about the": "coping",
	}
	for seed, want := range cases {
		if got := SeedTopic(seed); got != want {
			t.Errorf("SeedTopic(%q) = %q, want %q", seed, got, want)
		}
	}
}
