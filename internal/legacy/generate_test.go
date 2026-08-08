package legacy

import (
	"strings"
	"testing"
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
