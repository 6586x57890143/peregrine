package legacy

import (
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/storage"
)

// This file exercises the whole read path against a real corpus: seed selection,
// candidate scoring, the prefix-shrink backoff, the jump-word fallback and the emit
// gate, all inside the single read transaction that generation is supposed to be.
//
// It exists because M6b rewired every one of those to take a *storage.Reader, and
// "it compiles" is not evidence that a scorer still finds anything. Before this,
// nothing in the suite proved that a message learned through learnMessage could be
// read back by the generator at all: the layer tests cover storage, and the gate tests
// only assert that blocked content is absent.

// teach seeds the corpus the way the bot does, through learnMessage, so the keys under
// test are the ones ingestion actually writes rather than ones a test invented.
func teach(t *testing.T, lines ...string) {
	t.Helper()
	for i, line := range lines {
		learn(t, store, line, snowflake(100+i))
	}
}

// teachAs is teach with a chosen author, for the author-diversity gate.
func teachAs(t *testing.T, authorID string, idBase int, lines ...string) {
	t.Helper()
	for i, line := range lines {
		if err := store.Update(func(w *storage.Writer) error {
			return learnMessage(w, line, snowflake(idBase+i), "999",
				MentionedUser{Name: authorID, UserID: authorID, Username: authorID}, nil)
		}); err != nil {
			t.Fatalf("learnMessage as %s: %v", authorID, err)
		}
	}
}

// TestGenParamsMapsEveryConfiguredDial exists because the mapping from Config to
// markov.Params is eight assignments of the same two types, so a transposition (TopP
// taking cfg.TopK, say) would compile, would pass every behavioural test in the suite,
// and would silently mean the operator's dials do something other than what they say.
//
// Distinct values per field are what make a transposition detectable at all.
func TestGenParamsMapsEveryConfiguredDial(t *testing.T) {
	gateFixture(t)

	cfg.MaxNGram = 4
	cfg.Temperature = 1.7
	cfg.TopK = 33
	cfg.TopP = 0.81
	cfg.KNDiscount = 0.62
	cfg.KNRawMix = 0.41
	cfg.MinDistinctAuthors = 3
	cfg.PromptRelevanceBoost = 0.9

	got := genParams()
	checks := []struct {
		name      string
		got, want any
	}{
		{"MaxNGram", got.MaxNGram, 4},
		{"Temperature", got.Temperature, 1.7},
		{"TopK", got.TopK, 33},
		{"TopP", got.TopP, 0.81},
		{"KNDiscount", got.KNDiscount, 0.62},
		{"KNRawMix", got.KNRawMix, 0.41},
		{"MinDistinctAuthors", got.MinDistinctAuthors, 3},
		{"PromptRelevance", got.PromptRelevance, 0.9},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("genParams().%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// TestGenerationHonoursTheConfiguredAuthorGate is the wiring pin for A6 at the level
// that matters to an operator: the engine's own tests prove the gate works, and this
// proves the bot's configuration reaches it.
//
// Without this, PEREGRINE_MIN_DISTINCT_AUTHORS could be read into Config, dropped on
// the floor in genParams, and every test in the repo would still pass while the
// anti-poisoning control did nothing.
func TestGenerationHonoursTheConfiguredAuthorGate(t *testing.T) {
	gateFixture(t)
	cfg.MinDistinctAuthors = 2

	// One author, said many times. This is the shape of the real attack.
	var lines []string
	for range 40 {
		lines = append(lines, "the bird should sayhorriblething")
	}
	teachAs(t, "poisoner", 500, lines...)

	for range 30 {
		got, err := generateSentenceWithContext(nil, "the bird should", false, &ConversationMemory{})
		if err != nil {
			t.Fatalf("generateSentenceWithContext: %v", err)
		}
		if strings.Contains(got, "sayhorriblething") {
			t.Fatalf("generated %q from a phrase only one author ever said, with "+
				"MinDistinctAuthors=2. The gate is configured but not reaching the engine", got)
		}
	}
}

// TestGenerationAllowsWhatTwoAuthorsSaid is the other direction, so the test above
// cannot be satisfied by a gate that refuses everything.
func TestGenerationAllowsWhatTwoAuthorsSaid(t *testing.T) {
	gateFixture(t)
	cfg.MinDistinctAuthors = 2

	teachAs(t, "alice", 600, "the bird is genuinelyloose today")
	teachAs(t, "bob", 700, "the bird is genuinelyloose today")

	var found bool
	for range 30 {
		got, err := generateSentenceWithContext(nil, "the bird is", false, &ConversationMemory{})
		if err != nil {
			t.Fatalf("generateSentenceWithContext: %v", err)
		}
		if strings.Contains(got, "genuinelyloose") {
			found = true
			break
		}
	}
	if !found {
		t.Error("a continuation two distinct authors produced was never generated in 30 " +
			"attempts; the gate must admit what it is meant to admit or the bot is silent " +
			"on a real server rather than safe on one")
	}
}

func TestGenerateFromALearnedCorpus(t *testing.T) {
	s := gateFixture(t)
	_ = s

	teach(t,
		"the bird is on the roof again",
		"the bird is loose in the server",
		"the bird knows what it did",
		"someone please contain the bird",
	)

	// A nil session is fine: the only thing generation wants one for is emoji
	// shortcode resolution, and sessionEmoji handles a nil session by declining.
	got, err := generateSentenceWithContext(nil, "the bird", false, &ConversationMemory{})
	if err != nil {
		t.Fatalf("generateSentenceWithContext: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("generated nothing from a corpus that was just taught four sentences. " +
			"Either seed selection found no candidate or the scorer found no successor: " +
			"both are silent failures, which is why this test exists")
	}
	if strings.Contains(got, "<end>") {
		t.Errorf("the end sentinel leaked into output: %q", got)
	}
	if strings.Contains(got, "\x00") {
		t.Errorf("a composite key separator leaked into output: %q. Something returned a raw "+
			"key where it should have returned a prefix or a token", got)
	}
}

// TestGenerateWithAMixedCasePromptFindsTheCorpus covers the case-normalization path,
// and it is worth saying plainly that it does NOT currently distinguish the two
// implementations: it passes with and without the lowercasing added in M6b.
//
// That is because every producer of the generation prefix happens to be lowercase
// already. The seed comes from findBestSeed, whose candidates are interned from
// lowercased sources; every later word is a stored successor token; and the one branch
// that could have introduced a raw prompt word is unreachable with a non-empty corpus.
// The lowercasing is defence against that stopping being true, not a fixed bug, and
// writing this comment beats leaving a test that looks like a pin and is not one.
//
// It earns its place anyway: a mixed-case prompt is the overwhelmingly common real
// input, and nothing else in the suite generates from one.
func TestGenerateWithAMixedCasePromptFindsTheCorpus(t *testing.T) {
	gateFixture(t)

	teach(t,
		"the bird is on the roof again",
		"the bird is loose in the server",
	)

	got, err := generateSentenceWithContext(nil, "The Bird", false, &ConversationMemory{})
	if err != nil {
		t.Fatalf("generateSentenceWithContext: %v", err)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatal("a mixed-case prompt generated nothing while the same words in lower case " +
			"generate fine, which means some prefix is being looked up in the case the user " +
			"typed rather than the case ingestion stores")
	}
}

// TestGenerateOnAnEmptyCorpusIsQuietAndDoesNotHang covers the branch that used to call
// Bucket.Stats() to answer "is there anything in here", which walked every page in the
// largest bucket on every reply (finding 11). It is Reader.CorpusEmpty now.
func TestGenerateOnAnEmptyCorpusIsQuietAndDoesNotHang(t *testing.T) {
	gateFixture(t)

	got, err := generateSentenceWithContext(nil, "hello there", false, &ConversationMemory{})
	if err != nil {
		t.Fatalf("generation against an empty corpus must not error: %v", err)
	}
	// The placeholder the empty-corpus branch produces, after cleaning. Asserting only
	// that nothing blows up and nothing substantial is claimed.
	if len(strings.Fields(got)) > 2 {
		t.Errorf("an empty corpus produced %q, which is more than a placeholder", got)
	}
}

// TestGeneratedOutputPassesTheEmitGate is the other half of A3, checked at the exit
// rather than at the entrance.
//
// Filtering the corpus cannot bound the output even in principle, because a Markov
// chain composes novel sequences from n-grams learned separately. This teaches two
// halves that are individually clean and whose join is not, then asserts the bot stays
// silent rather than substituting a fallback: silence is always safe, and a fallback is
// a new output to reason about.
func TestGeneratedOutputPassesTheEmitGate(t *testing.T) {
	gateFixture(t)

	// Each of these is learnable on its own; the shared prefix is what lets generation
	// walk from one into the other.
	teach(t,
		"you are the biggest exampleslur",
		"i think you are great",
	)

	for range 20 {
		got, err := generateSentenceWithContext(nil, "you are", false, &ConversationMemory{})
		if err != nil {
			t.Fatalf("generateSentenceWithContext: %v", err)
		}
		if strings.Contains(strings.ToLower(got), "exampleslur") {
			t.Fatalf("a blocked term reached the output: %q. CheckEmit sits at the generation "+
				"exit precisely because input filtering cannot bound what the chain composes "+
				"(SPEC.md section 4, A3)", got)
		}
	}
}

// ---------------------------------------------------------- the co-occurrence window

// TestCooccurrenceWindowBoundsWhatIsLearned is the write-path half of M7b and closes the
// rest of finding 12.
//
// The old loop was all-pairs, so it was quadratic in message length AND it ran inside the
// single write transaction that serializes every other write in the process. A 200-word
// message produced nearly 40,000 read-add-write pairs and blocked all ingestion while it
// did.
//
// The window is a model change as much as an optimization, and in the right direction:
// "co-occurs anywhere in the same message" is a claim that gets weaker as messages get
// longer, because a long message links every word in it to every other. Proximity is
// what association actually means.
//
// The fixture needs a mentioned user, because the association loop only runs when a
// message involves a recognized name.
func TestCooccurrenceWindowBoundsWhatIsLearned(t *testing.T) {
	s := gateFixture(t)
	cfg.CooccurrenceWindow = 2

	// Ten distinct non-stop words. "alpha" and "kilo" are eight apart, well outside a
	// window of two; "alpha" and "bravo" are adjacent.
	const msg = "alpha bravo charlie delta echo foxtrot golf hotel india kilo"
	if err := s.Update(func(w *storage.Writer) error {
		return learnMessage(w, msg, snowflake(4242), "999",
			MentionedUser{Name: "tester", UserID: "1", Username: "tester"},
			[]MentionedUser{{Name: "tester", UserID: "1", Username: "tester"}})
	}); err != nil {
		t.Fatalf("learnMessage: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		assoc, err := r.TopicWordsFor("alpha")
		if err != nil {
			return err
		}
		if _, ok := assoc["bravo"]; !ok {
			t.Error("an adjacent word was not associated, so the window is too tight to " +
				"record anything and the index is now useless")
		}
		if _, ok := assoc["kilo"]; ok {
			t.Error("a word eight positions away was associated with a window of 2, so the " +
				"window is not being applied and this is still the all-pairs loop")
		}
		// Exactly the two words on each side, minus the ones that fall off the start.
		if len(assoc) != 2 {
			t.Errorf("alpha has %d associations, want 2 (bravo and charlie): %v", len(assoc), assoc)
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestCooccurrenceWindowZeroIsUnbounded. 0 restores the old all-pairs behaviour, which
// has to remain expressible so an operator can compare, and .env.example says it warns.
func TestCooccurrenceWindowZeroIsUnbounded(t *testing.T) {
	s := gateFixture(t)
	cfg.CooccurrenceWindow = 0

	const msg = "alpha bravo charlie delta echo foxtrot golf hotel india kilo"
	if err := s.Update(func(w *storage.Writer) error {
		return learnMessage(w, msg, snowflake(4243), "999",
			MentionedUser{Name: "tester", UserID: "1", Username: "tester"},
			[]MentionedUser{{Name: "tester", UserID: "1", Username: "tester"}})
	}); err != nil {
		t.Fatalf("learnMessage: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		assoc, err := r.TopicWordsFor("alpha")
		if err != nil {
			return err
		}
		if _, ok := assoc["kilo"]; !ok {
			t.Error("with a window of 0 the furthest word must still be associated, or 0 " +
				"does not mean unbounded")
		}
		if len(assoc) != 9 {
			t.Errorf("alpha has %d associations, want 9 (every other word)", len(assoc))
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestCooccurrenceWindowRecordsBothDirections. The index is direction-sensitive: it
// stores the position of the ASSOCIATE, so (a, b) and (b, a) carry different position
// sums and the readers use that. A window that only looked forward would halve the
// index and silently change what every positional heuristic sees.
func TestCooccurrenceWindowRecordsBothDirections(t *testing.T) {
	s := gateFixture(t)
	cfg.CooccurrenceWindow = 2

	const msg = "alpha bravo charlie delta echo"
	if err := s.Update(func(w *storage.Writer) error {
		return learnMessage(w, msg, snowflake(4244), "999",
			MentionedUser{Name: "tester", UserID: "1", Username: "tester"},
			[]MentionedUser{{Name: "tester", UserID: "1", Username: "tester"}})
	}); err != nil {
		t.Fatalf("learnMessage: %v", err)
	}

	if err := s.View(func(r *storage.Reader) error {
		forward, err := r.TopicWord("alpha", "bravo")
		if err != nil {
			return err
		}
		backward, err := r.TopicWord("bravo", "alpha")
		if err != nil {
			return err
		}
		if forward.Count == 0 || backward.Count == 0 {
			t.Fatalf("both directions must be recorded, got forward=%d backward=%d",
				forward.Count, backward.Count)
		}
		if forward.MeanPosition() == backward.MeanPosition() {
			t.Error("the two directions carry the same mean position, which means the " +
				"position being stored is not the associate's and the positional " +
				"heuristics are reading a constant")
		}
		return nil
	}); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// ------------------------------------------------------- per-channel memory (G8)

// TestConversationMemoryIsPerChannel is the pin for finding G8. One shared memory meant a
// reply in one channel was steered by an unrelated conversation in another, which is not
// chaos, it is simply wrong context: the reply reads as a non-sequitur to the thread it
// is in.
func TestConversationMemoryIsPerChannel(t *testing.T) {
	resetChannelMemories(t)

	memoryFor("channel-a").AddMessage("alpha bravo charlie")
	memoryFor("channel-b").AddMessage("xray yankee zulu")

	a := memoryFor("channel-a").GetWeightedWords()
	b := memoryFor("channel-b").GetWeightedWords()

	if !containsWord(a, "alpha") {
		t.Error("channel A lost its own message")
	}
	if containsWord(a, "xray") {
		t.Error("channel A can see channel B's message, which is the bug G8 describes")
	}
	if !containsWord(b, "xray") {
		t.Error("channel B lost its own message")
	}
	if containsWord(b, "alpha") {
		t.Error("channel B can see channel A's message")
	}
}

// TestConversationMemoryIsBounded. The old single memory did not need a bound; a map
// keyed by channel ID does, because a bot can be added to any number of guilds and this
// is the kind of leak that never shows up in testing, since a test only ever uses one
// channel.
func TestConversationMemoryIsBounded(t *testing.T) {
	resetChannelMemories(t)

	for i := range maxRememberedChannels * 2 {
		memoryFor("channel-" + itoa(i)).AddMessage("some content here")
	}

	convMemoriesMu.Lock()
	n := len(convMemories)
	convMemoriesMu.Unlock()

	if n > maxRememberedChannels {
		t.Errorf("remembering %d channels against a cap of %d", n, maxRememberedChannels)
	}
	if n == 0 {
		t.Error("eviction emptied the map entirely")
	}

	// The most recently touched channel must have survived, or eviction is picking the
	// wrong end and the bot forgets the conversation it is currently in.
	last := "channel-" + itoa(maxRememberedChannels*2-1)
	convMemoriesMu.Lock()
	_, kept := convMemories[last]
	convMemoriesMu.Unlock()
	if !kept {
		t.Error("the most recently used channel was evicted")
	}
}

// resetChannelMemories clears the map and restores it afterwards, so these tests do not
// leak into each other or into the generation tests.
func resetChannelMemories(t *testing.T) {
	t.Helper()
	convMemoriesMu.Lock()
	saved := convMemories
	convMemories = map[string]*channelMemory{}
	convMemoriesMu.Unlock()

	t.Cleanup(func() {
		convMemoriesMu.Lock()
		convMemories = saved
		convMemoriesMu.Unlock()
	})
}

func containsWord(words []string, want string) bool {
	for _, w := range words {
		if w == want {
			return true
		}
	}
	return false
}
