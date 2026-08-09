package generate

import (
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// This file exercises the whole read path against a real corpus: seed selection, candidate
// scoring, the Kneser-Ney backoff, the jump fallback and the length model, all inside the
// single read transaction generation is supposed to be.
//
// It exists because "it compiles" is not evidence that a scorer still finds anything. The
// engine's own tests cover the model against Go maps; these prove that a message learned
// through the learn path can be read back by the generator at all.

func snowflake(n int) string {
	return strconv.FormatUint((uint64(n)<<22)|1, 10)
}

// defaults are the shipped values rather than zeroes.
//
// A zero Temperature makes the sampler argmax and a zero TopP keeps only the single best
// candidate, which would quietly turn every test here into a deterministic-path test and hide
// anything to do with sampling. A zero MaxWords makes the length model cap every sentence at
// one word, so the generation tests would all pass while proving nothing.
//
// MinDistinctAuthors is the one deliberate exception, at 0: these fixtures are single-author
// by nature, so the shipped default of 2 would make generation correctly produce nothing and
// every assertion below would be testing the gate instead of what it says it tests.
func defaults() Options {
	return Options{
		MaxNGram:           3,
		MinWords:           4,
		MaxWords:           18,
		Temperature:        1.0,
		TopK:               40,
		TopP:               0.95,
		KNDiscount:         0.75,
		KNRawMix:           0.25,
		MinDistinctAuthors: 0,
		PromptRelevance:    0.6,
		RoastChance:        0.10,
	}
}

// fixture returns a corpus, a Learner that writes to it, and a Generator that reads it.
func fixture(t *testing.T, opts Options) (*storage.Store, *learn.Learner, *Generator) {
	t.Helper()
	s := dbtest.Store(t)
	gate := safety.NewGate(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	l := learn.New(gate, learn.Options{MaxNGram: opts.MaxNGram, MaxHistory: 1000, CooccurrenceWindow: 5})
	return s, l, New(s, opts)
}

// teach seeds the corpus the way the bot does, through the learn path, so the keys under test
// are the ones ingestion actually writes rather than ones a test invented.
func teach(t *testing.T, s *storage.Store, l *learn.Learner, authorID string, base int, lines ...string) {
	t.Helper()
	who := names.User{Name: authorID, UserID: authorID, Username: authorID}
	for i, line := range lines {
		if err := s.Update(func(w *storage.Writer) error {
			return l.Message(w, line, snowflake(base+i), who, []names.User{who})
		}); err != nil {
			t.Fatalf("learn %q as %s: %v", line, authorID, err)
		}
	}
}

// TestParamsMapsEveryConfiguredDial exists because the mapping from Options to markov.Params
// is eight assignments of the same two types, so a transposition (TopP taking TopK, say) would
// compile, would pass every behavioural test in the suite, and would silently mean the
// operator's dials do something other than what they say.
//
// Distinct values per field are what make a transposition detectable at all.
func TestParamsMapsEveryConfiguredDial(t *testing.T) {
	o := Options{
		MaxNGram:           5,
		Temperature:        1.25,
		TopK:               37,
		TopP:               0.81,
		KNDiscount:         0.66,
		KNRawMix:           0.42,
		MinDistinctAuthors: 3,
		PromptRelevance:    1.75,
	}
	p := o.Params()

	checks := map[string][2]any{
		"MaxNGram":           {p.MaxNGram, o.MaxNGram},
		"Temperature":        {p.Temperature, o.Temperature},
		"TopK":               {p.TopK, o.TopK},
		"TopP":               {p.TopP, o.TopP},
		"KNDiscount":         {p.KNDiscount, o.KNDiscount},
		"KNRawMix":           {p.KNRawMix, o.KNRawMix},
		"MinDistinctAuthors": {p.MinDistinctAuthors, o.MinDistinctAuthors},
		"PromptRelevance":    {p.PromptRelevance, o.PromptRelevance},
	}
	for name, pair := range checks {
		if fmt.Sprint(pair[0]) != fmt.Sprint(pair[1]) {
			t.Errorf("%s = %v, want %v: a dial is transposed or dropped, so the operator's "+
				"configuration does something other than what it says", name, pair[0], pair[1])
		}
	}
}

// TestGenerateFromALearnedCorpus is the end-to-end read: something learned comes back out.
func TestGenerateFromALearnedCorpus(t *testing.T) {
	s, l, g := fixture(t, defaults())
	teach(t, s, l, "u1", 100,
		"the bird is loose in the server again",
		"the bird is loose and it is bad",
		"the bird is on the roof",
		"the server is doomed honestly",
	)

	got, err := g.Sentence("the bird is", false, nil, nil)
	if err != nil {
		t.Fatalf("Sentence: %v", err)
	}
	if got == "" {
		t.Fatal("generated nothing from a corpus that holds the prompt's own prefix")
	}
	if strings.Contains(got, "<end>") {
		t.Errorf("the end sentinel reached the output: %q", got)
	}
}

// TestGenerationHonoursTheConfiguredAuthorGate is the wiring pin for A6 at the level that
// matters to an operator: the engine's own tests prove the gate works, and this proves the
// bot's configuration reaches it.
//
// Without this, PEREGRINE_MIN_DISTINCT_AUTHORS could be read into Config, dropped on the floor
// in Params, and every test in the repo would still pass while the anti-poisoning control did
// nothing.
//
// Verified by reverting: with MinDistinctAuthors dropped from Params, the poisoned phrase is
// generated and this fails.
func TestGenerationHonoursTheConfiguredAuthorGate(t *testing.T) {
	opts := defaults()
	opts.MinDistinctAuthors = 2
	s, l, g := fixture(t, opts)

	// One author, said many times. This is the shape of the real attack.
	var lines []string
	for range 40 {
		lines = append(lines, "the bird should saythepoison")
	}
	teach(t, s, l, "poisoner", 500, lines...)

	for range 30 {
		got, err := g.Sentence("the bird should", false, nil, nil)
		if err != nil {
			t.Fatalf("Sentence: %v", err)
		}
		if strings.Contains(got, "saythepoison") {
			t.Fatalf("generated %q from a phrase only one author ever said, with "+
				"MinDistinctAuthors=2. The gate is configured but not reaching the engine", got)
		}
	}
}

// TestGenerationAllowsWhatTwoAuthorsSaid is the other half: the gate must not be a mute
// button. Without this, a broken gate that refused everything would pass the test above.
func TestGenerationAllowsWhatTwoAuthorsSaid(t *testing.T) {
	opts := defaults()
	opts.MinDistinctAuthors = 2
	s, l, g := fixture(t, opts)

	lines := []string{
		"the bird is loose in the server again",
		"the bird is loose and everyone knows",
		"the bird is loose honestly",
	}
	teach(t, s, l, "u1", 600, lines...)
	teach(t, s, l, "u2", 700, lines...)

	found := false
	for range 30 {
		got, err := g.Sentence("the bird is", false, nil, nil)
		if err != nil {
			t.Fatalf("Sentence: %v", err)
		}
		if got != "" {
			found = true
			break
		}
	}
	if !found {
		t.Error("generated nothing in 30 attempts from a corpus two authors agreed on; " +
			"the author gate is refusing everything, which makes the poisoning test vacuous")
	}
}

// TestAMixedCasePromptFindsTheCorpus.
//
// Learning lowercases the prefix before storing it, and the lookup lowercases at the point of
// lookup. This does not distinguish the two implementations, and says so: every producer of a
// generation prefix is already lowercase, so the normalization added in M6b is defence rather
// than a fixed bug. A test that looks like a regression pin and is not one is worse than no
// test.
func TestAMixedCasePromptFindsTheCorpus(t *testing.T) {
	s, l, g := fixture(t, defaults())
	teach(t, s, l, "u1", 200,
		"the bird is loose in the server again",
		"the bird is loose and it is bad",
	)

	got, err := g.Sentence("The BIRD Is", false, nil, nil)
	if err != nil {
		t.Fatalf("Sentence: %v", err)
	}
	if got == "" {
		t.Error("a mixed-case prompt found nothing in a corpus that holds its lowercase form")
	}
}

// TestAnEmptyCorpusIsQuietAndDoesNotHang. Reader.CorpusEmpty is one cursor First(), replacing
// a Bucket.Stats() call that walked every page in the largest bucket on every reply.
func TestAnEmptyCorpusIsQuietAndDoesNotHang(t *testing.T) {
	_, _, g := fixture(t, defaults())

	got, err := g.Sentence("anything at all", false, nil, nil)
	if err != nil {
		t.Fatalf("Sentence on an empty corpus: %v", err)
	}
	if got != "" {
		t.Errorf("generated %q from an empty corpus, want silence", got)
	}
}

// TestBelowTwoWordsIsSilence.
//
// One word is not a punchy reply, it is a reply that looks broken. The floor exists because
// the golden samples printed one-word replies like "roof": a seed drawn from a non-prompt tier
// can dead-end on its first step, since the length floor is a logit penalty on the end token
// and a penalty does nothing when no candidate is eligible at all.
func TestBelowTwoWordsIsSilence(t *testing.T) {
	s, l, g := fixture(t, defaults())
	// A corpus with exactly one two-word message, so most seeds dead-end immediately.
	teach(t, s, l, "u1", 300, "roof")

	for range 20 {
		got, err := g.Sentence("nothing", false, nil, nil)
		if err != nil {
			t.Fatalf("Sentence: %v", err)
		}
		if got != "" && len(strings.Fields(got)) < 2 {
			t.Fatalf("generated the one-word reply %q; below two words the bot must stay silent", got)
		}
	}
}

// ---------------------------------------------------------------- conversation memory

// TestMemoryIsPerChannel closes finding G8.
//
// There used to be one package-level memory shared by every channel in every guild, so a reply
// in one channel was steered by whatever had been said in an unrelated one. That is not chaos,
// which would be fine, it is simply the wrong context: the reply reads as a non-sequitur to
// the thread it is in, so the bot looks like it is not paying attention rather than funny.
func TestMemoryIsPerChannel(t *testing.T) {
	ms := NewMemories(0)

	ms.For("channel-a").Add("alpha bravo charlie")
	ms.For("channel-b").Add("delta echo foxtrot")

	a := strings.Join(ms.For("channel-a").WeightedWords(), " ")
	b := strings.Join(ms.For("channel-b").WeightedWords(), " ")

	if !strings.Contains(a, "alpha") {
		t.Errorf("channel-a lost its own context: %q", a)
	}
	if strings.Contains(a, "delta") {
		t.Errorf("channel-a sees channel-b's context: %q", a)
	}
	if strings.Contains(b, "alpha") {
		t.Errorf("channel-b sees channel-a's context: %q", b)
	}
}

// TestMemoryIsBounded. The map is keyed by channel and grows with every guild the bot joins,
// and it is the kind of leak a test using one channel would never reveal.
func TestMemoryIsBounded(t *testing.T) {
	ms := NewMemories(10)

	for i := range 100 {
		ms.For(fmt.Sprintf("channel-%03d", i)).Add("something")
	}
	if got := ms.Len(); got > 10 {
		t.Errorf("remembering %d channels against a bound of 10", got)
	}
	// The most recently touched survives, which is what makes eviction useful rather than
	// arbitrary.
	if got := ms.For("channel-099").WeightedWords(); len(got) == 0 {
		t.Error("the channel touched last was evicted")
	}
}

// TestMemoryDecaysOlderMessages. Repetition is the weighting mechanism, because the consumer
// is a bag-of-words feature set that cannot take a weight.
func TestMemoryDecaysOlderMessages(t *testing.T) {
	m := &Memory{}
	m.Add("oldest")
	for range 10 {
		m.Add("filler")
	}
	m.Add("newest")

	words := m.WeightedWords()
	counts := map[string]int{}
	for _, w := range words {
		counts[w]++
	}
	if counts["newest"] <= counts["oldest"] {
		t.Errorf("newest appears %d times and oldest %d; recent context must weigh more",
			counts["newest"], counts["oldest"])
	}
}

// TestMemoryIsBoundedPerChannel, so one busy channel cannot grow without limit either.
func TestMemoryIsBoundedPerChannel(t *testing.T) {
	m := &Memory{}
	for i := range 500 {
		m.Add(fmt.Sprintf("message-%d", i))
	}
	m.mu.Lock()
	n := len(m.entries)
	m.mu.Unlock()
	if n > maxEntries {
		t.Errorf("one channel holds %d entries against a bound of %d", n, maxEntries)
	}
}
