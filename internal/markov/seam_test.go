package markov_test

// This is an external test package on purpose. It imports internal/storage, which the
// production markov package must never do, and keeping that import in a _test.go file
// under a different package name means the constraint is visible rather than trusted:
// nothing here can leak into markov's own import list.
//
// Two things are checked, and the second is the one that matters.
//
// First, that *storage.Reader actually satisfies markov.Corpus. That is a compile-time
// fact, and the assertion below is what turns a future signature change in storage
// into a failed build here rather than into a runtime type error at the wiring site in
// legacy.
//
// Second, that the map-backed fake in fake_test.go agrees with the real writer. Every
// other test in this package is written against the fake, so a fake that diverges from
// storage would give a green suite over a model that does not match the corpus the bot
// actually has. The tests below deliberately re-check the same properties the fake
// tests assert, against real bbolt.

import (
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/markov"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// The seam, asserted at compile time.
var _ markov.Corpus = (*storage.Reader)(nil)

// learn ingests a message into a real corpus the way learnMessage does: order 2 and up,
// never 1, with the topic counts that the Kneser-Ney base case reads.
func learn(t *testing.T, s *storage.Store, maxNGram int, author, text string) {
	t.Helper()
	words := strings.Fields(text)

	err := s.Update(func(w *storage.Writer) error {
		for _, word := range words {
			if err := w.IncTopic(word); err != nil {
				return err
			}
		}
		for n := maxNGram; n >= 2; n-- {
			if len(words) < n {
				continue
			}
			for i := 0; i <= len(words)-n; i++ {
				prefix := strings.Join(words[i:i+n-1], " ")
				if err := w.LearnNgram(prefix, words[i+n-1], author); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("learn %q: %v", text, err)
	}
}

func params() markov.Params {
	return markov.Params{
		MaxNGram: 5, Temperature: 1.0, TopK: 40, TopP: 0.95,
		KNDiscount: 0.75, KNRawMix: 0.25, MinDistinctAuthors: 0,
		PromptRelevance: 0.6,
	}
}

func step(prefix ...string) *markov.Step {
	return &markov.Step{Prefix: prefix, Sentence: prefix, Length: markov.Length{Min: 4, Max: 18, Target: 8}}
}

// TestGenerationAgainstARealCorpus is the end-to-end proof that the engine reads the
// keys ingestion actually writes. It compiles and the fake tests pass, but neither of
// those says that a prefix built here matches a key stored there.
func TestGenerationAgainstARealCorpus(t *testing.T) {
	store := dbtest.Store(t)
	learn(t, store, 5, "alice", "the bird is loose again")
	learn(t, store, 5, "bob", "the bird is on the roof")
	learn(t, store, 5, "carol", "the bird knows what it did")

	err := store.View(func(r *storage.Reader) error {
		g := markov.New(r, params(), nil)
		got, err := g.Next(step("the", "bird"))
		if err != nil {
			return err
		}
		if got == "" {
			t.Error("generated nothing from a corpus that was just taught three sentences " +
				"beginning with the same two words. Either the prefix built here does not " +
				"match the key stored there, or the model found no mass: both are silent")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestRealCorpusAuthorDiversityMatchesTheFake re-checks the poisoning pin against real
// bbolt, because the presence-set semantics it depends on are enforced by storage and
// not by Go.
//
// This is the property that was a real bug in M6a: bbolt's Get returns nil both for a
// missing key and for a key stored with an empty value, so a presence set written with
// a zero-length value reported every member absent and the author count silently
// counted occurrences. 500 repetitions by one person reported 500 distinct authors.
func TestRealCorpusAuthorDiversityMatchesTheFake(t *testing.T) {
	store := dbtest.Store(t)
	for range 500 {
		learn(t, store, 5, "poisoner", "the bird should sayhorriblething")
	}

	p := params()
	p.MinDistinctAuthors = 2

	err := store.View(func(r *storage.Reader) error {
		g := markov.New(r, p, nil)
		for range 50 {
			got, err := g.Next(step("bird", "should"))
			if err != nil {
				return err
			}
			if got != "" {
				t.Fatalf("generated %q from a phrase 500 times repeated by one author against "+
					"a REAL corpus. The fake agreeing is not enough here: the distinct-author "+
					"count is a bbolt presence set, and a presence set that stores an empty "+
					"value cannot be distinguished from a missing key", got)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestRealCorpusHigherOrderIsPreferred is the finding G1 pin against real storage, with
// the same fixture shape as the fake version: raw frequency points the other way, so
// only a model that weights the order can get this right.
func TestRealCorpusHigherOrderIsPreferred(t *testing.T) {
	store := dbtest.Store(t)
	learn(t, store, 5, "alice", "the bird is on the roof")
	for range 6 {
		learn(t, store, 5, "bob", "a bird went loose")
		learn(t, store, 5, "carol", "something loose happened")
	}

	// Sampled rather than compared directly, because the probabilities are internal to
	// the package. At temperature 0 the sampler is argmax, so the winner is exactly the
	// highest-logit candidate and the assertion is deterministic.
	p := params()
	p.Temperature = 0

	err := store.View(func(r *storage.Reader) error {
		g := markov.New(r, p, nil)
		got, err := g.Next(step("the", "bird", "is", "on", "the"))
		if err != nil {
			return err
		}
		if got != "roof" {
			t.Errorf("argmax picked %q, want \"roof\": the continuation with high-order "+
				"evidence must beat the one that is merely more frequent (finding G1)", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestRealCorpusTopicTotalTracksIncTopic covers the counter added for the KNRawMix base
// case, since a zero there silently turns mu off.
func TestRealCorpusTopicTotalTracksIncTopic(t *testing.T) {
	store := dbtest.Store(t)
	learn(t, store, 5, "alice", "one two three four")

	err := store.View(func(r *storage.Reader) error {
		if got := r.TotalTopicCount(); got != 4 {
			t.Errorf("TotalTopicCount = %d after four words, want 4", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}

	learn(t, store, 5, "bob", "five six")
	err = store.View(func(r *storage.Reader) error {
		if got := r.TotalTopicCount(); got != 6 {
			t.Errorf("TotalTopicCount = %d after six words total, want 6", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestRealCorpusPrefixTotalMatchesASuccessorSum pins the one place this layer
// deliberately walks instead of reading a counter. If PrefixTotal ever disagreed with
// the successors it sums, every Kneser-Ney lambda would be wrong and no output would
// look broken.
func TestRealCorpusPrefixTotalMatchesASuccessorSum(t *testing.T) {
	store := dbtest.Store(t)
	learn(t, store, 5, "alice", "the bird is loose")
	learn(t, store, 5, "bob", "the bird is fine")
	learn(t, store, 5, "carol", "the bird is loose")
	learn(t, store, 5, "dave", "the cat is loose")

	err := store.View(func(r *storage.Reader) error {
		for _, prefix := range []string{"the", "the bird", "bird is", "is"} {
			succ, err := r.Successors(prefix)
			if err != nil {
				return err
			}
			var want uint64
			for _, s := range succ {
				want += s.Count
			}
			if got := r.PrefixTotal(prefix); got != want {
				t.Errorf("PrefixTotal(%q) = %d, sum of successors = %d", prefix, got, want)
			}
		}

		// And the prefix range must not bleed across the separator. The fixture has
		// "the" followed by "bird" three times and "cat" once, and nothing else, so the
		// total is exactly 4. A scan that wandered into "the bird\x00..." keys would
		// report more, which would inflate the lambda of every context ending in a
		// common word and silently reweight the whole model.
		if got := r.PrefixTotal("the"); got != 4 {
			t.Errorf("PrefixTotal(%q) = %d, want 4 (bird three times, cat once). Anything "+
				"larger means the scan crossed the NUL separator into longer prefixes",
				"the", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
}

// TestRealCorpusOnlyEverReturnsACorpusToken sweeps the arithmetic hazards from the
// outside: log of zero, division by an empty total, exponentiation overflow at an
// extreme temperature, and an empty or unknown prefix.
//
// None of those produce an error. They produce a distribution the sampler cannot
// compare, and its failure mode is returning SOMETHING: the last candidate, or a token
// picked for a reason that no longer exists. So the assertion is that every token that
// comes back is one the corpus actually contains, which is the observable form of "the
// arithmetic stayed finite".
func TestRealCorpusOnlyEverReturnsACorpusToken(t *testing.T) {
	store := dbtest.Store(t)
	learn(t, store, 5, "alice", "the bird is loose again and again")

	vocab := map[string]bool{}
	for _, w := range strings.Fields("the bird is loose again and again") {
		vocab[w] = true
	}

	// 0.01 divides logits by 100, which is where exponentiation would overflow without
	// the max subtraction; 50 is the other end.
	for _, temp := range []float64{0.01, 1.0, 50.0} {
		p := params()
		p.Temperature = temp

		err := store.View(func(r *storage.Reader) error {
			g := markov.New(r, p, nil)
			for _, prefix := range [][]string{
				{"the", "bird"}, {"bird"}, {"nothing", "here"}, {""}, {},
			} {
				for range 20 {
					got, err := g.Next(&markov.Step{
						Prefix:   prefix,
						Sentence: append([]string{}, prefix...),
						Length:   markov.Length{Min: 4, Max: 18, Target: 8},
					})
					if err != nil {
						return err
					}
					if got == "" {
						continue // a dead end is a legitimate answer
					}
					if !vocab[got] {
						t.Errorf("at T=%v prefix %q returned %q, which is not in the corpus. "+
							"A NaN or infinite weight does not error, it makes the sampler "+
							"return whatever it was last holding", temp, prefix, got)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("View at T=%v: %v", temp, err)
		}
	}
}
