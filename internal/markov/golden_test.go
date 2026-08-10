package markov

import (
	"fmt"
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/corpus"
)

// This file is the instrument for the one thing assertions cannot settle.
//
// Peregrine's quality criterion is whether the output LANDS, not whether it parses: a
// six-word non-sequitur that gets a reaction beats a forty-word grammatical ramble
// (SPEC.md section 1). No test can assert that. So the harness generates from a fixed
// corpus with a seeded source across a sweep of temperature and top-k, and prints the
// result, which makes before-and-after directly comparable and lets the chaos dials be
// tuned by reading rather than by guessing.
//
// Run it with:
//
//	go test ./internal/markov/ -run TestGenerateGolden -v
//
// It asserts almost nothing on purpose. The assertions that do belong here are the
// mechanical ones, and they live in the tests below it.

// generate is ONE attempt, mirroring legacy.generateSentenceAttempt: seed selection, the
// length model, the dead-end jump. generateReply below adds the retry and the floor that
// legacy.generateSentenceWithContext adds, and that is the function the harness prints.
//
// Deliberately mirrors the production caller rather than being a simplified version of
// it, because the whole point is that reading these lines tells you what the channel will
// look like.
func generate(g *Generator, prompt string, persona Persona) []string {
	promptWords := strings.Fields(prompt)

	// THE NAMES IN THE PROMPT, which this harness did not resolve at all until now. Names and
	// NameTokens were both empty, so seed tiers 2, 3 and 5, the NameAssoc logit and the
	// PromptName logit had never once fired in a printed sample: every name-aware part of the
	// engine was invisible to the only instrument for judging it. Mirrors what
	// generate.attempt does, canonical form and surface form both.
	var names, nameTokens []string
	promptNames := map[string]struct{}{}
	nameAssoc := map[string]corpus.TopicAssoc{}
	for _, word := range promptWords {
		if !g.corpus.IsName(word) {
			continue
		}
		names = append(names, word)
		if _, dup := promptNames[word]; !dup {
			promptNames[word] = struct{}{}
			nameTokens = append(nameTokens, word)
		}
		found, err := g.corpus.NameTopicsFor(word)
		if err != nil {
			continue
		}
		for w, data := range found {
			existing := nameAssoc[w]
			existing.Count += data.Count
			existing.PosSum += data.PosSum
			nameAssoc[w] = existing
		}
	}

	seedIn := SeedInput{PromptWords: promptWords, Names: names, NameTokens: nameTokens}
	seed := g.Seed(seedIn)
	if seed == "" {
		if len(promptWords) == 0 {
			return nil
		}
		seed = promptWords[0]
	}
	words := strings.Fields(seed)

	length := NewLength(g.src, g.params.MinWords, g.params.MaxWords)

	s := &Step{
		Prompt:       prompt,
		PromptSet:    map[string]struct{}{},
		RecentSet:    map[string]struct{}{},
		Used:         map[string]int{},
		Ngrams:       map[string]struct{}{},
		CoreTopics:   map[string]float64{},
		NameAssoc:    nameAssoc,
		PromptNames:  promptNames,
		Length:       length,
		Persona:      persona,
		Prefix:       append([]string{}, words...),
		Sentence:     append([]string{}, words...),
		CurrentTopic: seed,
	}
	for _, w := range promptWords {
		s.PromptSet[w] = struct{}{}
		s.CoreTopics[w] = 1.0
	}
	for _, w := range words {
		s.Used[w]++
	}

	// Mirrors the production caller: whether the sentence CHOSE to end decides whether its
	// trailing function words are trimmed.
	chose := false

	for !length.Done(len(s.Sentence)) {
		if len(s.Prefix) == 0 {
			break
		}
		s.Position = float64(len(s.Sentence)) / float64(length.Max)

		next, err := g.Next(s)
		if err != nil {
			break
		}
		if next == EndToken {
			chose = true
			break
		}
		if next == "" {
			jump := g.Jump(seedIn, s)
			if jump == "" {
				break
			}
			next = jump
		}

		s.Sentence = append(s.Sentence, next)
		s.Used[next]++
		s.Ngrams[next] = struct{}{}
		if len(s.Sentence) >= 2 {
			s.Ngrams[lastN(s.Sentence, 2)] = struct{}{}
		}
		if len(s.Sentence) >= 3 {
			s.Ngrams[lastN(s.Sentence, 3)] = struct{}{}
		}
		s.Prefix = append(s.Prefix, next)
		if len(s.Prefix) > g.params.MaxNGram-1 {
			s.Prefix = s.Prefix[1:]
		}
	}

	s.Sentence = TrimDangling(s.Sentence, chose)
	return s.Sentence
}

// generateReply is what a caller actually posts: the bounded re-seed and the two-word
// floor from legacy.generateSentenceWithContext, on top of one or more attempts.
//
// Both belong here rather than only in legacy, because the harness exists to show what a
// channel will look like. Without them it printed one-word lines like "roof" and
// "coping", which the bot suppresses, and a harness that prints output the bot cannot
// produce is worse than no harness.
//
// The re-seed is not the discard-and-retry that M7b deleted. That one threw away an end
// token and continued from the same prefix, fighting the length decision; this abandons
// the attempt and draws a different seed, which is the only response available when a
// seed dead-ends on its first step because nothing it can reach is eligible.
func generateReply(g *Generator, prompt string, persona Persona, styled bool) string {
	var best []string
	const attempts = 3
	for range attempts {
		words := generate(g, prompt, persona)
		if len(words) > len(best) {
			best = words
		}
		if len(best) >= g.params.MinWords {
			break
		}
	}
	if len(best) < 2 {
		return ""
	}

	out := strings.Join(best, " ")
	if styled {
		out = g.Style(out, persona, false)
	}
	return out
}

func TestGenerateGolden(t *testing.T) {
	f := goldenCorpus()

	// The sweep covers the shapes that produced the live failures this milestone is about,
	// not a list of convenient prompts. In order: the exact failing case (a name behind a
	// question), a bare name, a two-word prompt with nothing to work from, two names at
	// once, a name the prompt asks ABOUT, and two controls with no name in them at all.
	prompts := goldenPrompts()

	for _, temp := range []float64{0.7, 1.0, 1.6, 2.5} {
		for _, topK := range []int{0, 8, 40} {
			p := testParams()
			p.Temperature = temp
			p.TopK = topK
			p.MinDistinctAuthors = 2

			t.Logf("--- T=%.1f top_k=%d min_authors=2 ---", temp, topK)
			for _, prompt := range prompts {
				// A fresh seeded source per line so the sweep is comparable across
				// runs and across changes to the weights.
				g := New(f, p, seeded(0xC0FFEE, 0xBADF00D))
				var lines []string
				for range 3 {
					lines = append(lines, generateReply(g, prompt, PersonaNeutral, false))
				}
				t.Logf("  %-16s -> %s", prompt, strings.Join(lines, " | "))
			}

			g := New(f, p, seeded(0xC0FFEE, 0xBADF00D))
			t.Logf("  %-16s -> %s", "[roast] greg is", generateReply(g, "greg is", PersonaRoast, true))
		}
	}
}

// TestGoldenOutputIsReproducible is what makes the harness above useful: if the same
// seed produced different text on two runs, no printed difference could be attributed
// to a change in a weight.
func TestGoldenOutputIsReproducible(t *testing.T) {
	f := goldenCorpus()
	p := testParams()
	p.MinDistinctAuthors = 2

	run := func() []string {
		g := New(f, p, seeded(5, 6))
		out := make([]string, 0, 10)
		for range 10 {
			out = append(out, generateReply(g, "the bird", PersonaNeutral, false))
		}
		return out
	}

	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("line %d differed between runs:\n  %q\n  %q", i, a[i], b[i])
		}
	}
}

// TestGoldenOutputSkewsShort is the length pin from SPEC.md section 5.4. A thirty to
// forty-five word Markov ramble reads as a malfunction; short and punchy reads as a
// joke. M7a inherits legacy's bounds, so this is the floor the M7b length model has to
// keep clearing rather than the final word on it.
func TestGoldenOutputSkewsShort(t *testing.T) {
	f := goldenCorpus()
	p := testParams()
	p.MinDistinctAuthors = 2
	g := New(f, p, seeded(11, 12))

	var total int
	const runs = 60
	for range runs {
		total += len(strings.Fields(generateReply(g, "the bird", PersonaNeutral, false)))
	}
	if avg := float64(total) / runs; avg > 14 {
		t.Errorf("average generated length %.1f words, want well under the 18-word cap. "+
			"Long output reads as a malfunction rather than a joke (SPEC.md section 5.1)", avg)
	}
}

// TestGoldenOutputNeverLeaksASentinelOrASeparator. The end sentinel is a corpus token
// and the NUL is a key separator; either one reaching a Discord message is a visible
// bug in a channel.
func TestGoldenOutputNeverLeaksASentinelOrASeparator(t *testing.T) {
	f := goldenCorpus()
	p := testParams()
	p.Temperature = 2.5 // hot, so the tail gets explored
	p.MinDistinctAuthors = 0
	g := New(f, p, seeded(13, 14))

	for _, prompt := range []string{"the bird", "greg is", "ratio", "the server is"} {
		for range 40 {
			got := generateReply(g, prompt, PersonaRoast, true)
			if strings.Contains(got, EndToken) {
				t.Fatalf("the end sentinel leaked into output: %q", got)
			}
			if strings.Contains(got, "\x00") {
				t.Fatalf("a key separator leaked into output: %q", got)
			}
		}
	}
}

// TestGoldenEmoteShortcodesSurvive. The bot has never been able to speak in the
// server's own emotes: the shortcode resolver walked s.State.Guilds while the session
// never requested IntentsGuilds, so the slice was always empty (finding 7). M3 added
// the intent, and in a meme server the emotes are most of the register, so the engine
// must at least be capable of emitting a shortcode for the resolver to act on.
func TestGoldenEmoteShortcodesSurvive(t *testing.T) {
	f := goldenCorpus()
	p := testParams()
	p.Temperature = 1.6
	p.MinDistinctAuthors = 0
	g := New(f, p, seeded(17, 18))

	var found bool
	for range 400 {
		if strings.Contains(generateReply(g, ":birdstare:", PersonaNeutral, false), ":birdstare:") {
			found = true
			break
		}
	}
	if !found {
		t.Error("no generated line contained the emote shortcode present in the corpus; " +
			"emote-bearing output is the largest single register improvement available")
	}
}

// Example keeps the harness honest about being readable output rather than a metric.
func ExampleGenerator_Next() {
	f := newFake()
	f.learn(5, "alice", "the bird is loose")
	f.learn(5, "bob", "the bird is loose")

	p := Params{MaxNGram: 5, Temperature: 1, TopK: 40, TopP: 0.95,
		KNDiscount: 0.75, KNRawMix: 0.25, MinDistinctAuthors: 2}
	g := New(f, p, seeded(1, 2))

	next, _ := g.Next(&Step{Prefix: []string{"bird", "is"}, Length: Length{Min: 4, Max: 18, Target: 8}})
	fmt.Println(next)
	// Output: loose
}
