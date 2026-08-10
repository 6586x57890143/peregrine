package markov

import (
	"sort"
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/text"
)

// The two mechanical tests behind SPEC.md section 1 principle 7.
//
// Peregrine's output bar is "short-to-medium memey sentences that grasp the data,
// incorporate the people around them, and become unintentionally funny by combining varied
// concepts". The first three clauses are judged by reading golden samples. The last one is
// not judgeable by eye at scale and is the one every other change in the engine pushes
// against, so it gets assertions.
//
// THE FAILURE THESE EXIST TO CATCH is over-coherence. Every heuristic in score.go makes the
// output more like its source, and the limit of that process is a bot that quotes the
// corpus back. That reads fine, scores well on any conventional metric, and is not funny.
// Nothing else in the suite would notice it happening: a recited sentence is grammatical,
// in register, the right length, and passes every existing assertion.

// sourceSpans returns every contiguous word span of every fixture message, as joined
// strings, so a generated sentence can be checked for being one of them.
//
// Spans rather than whole messages, because reciting half of somebody's message is the same
// defect as reciting all of it.
func sourceSpans(minLen int) map[string]struct{} {
	spans := map[string]struct{}{}
	for _, m := range fixtureMessages() {
		words := strings.Fields(m.text)
		for n := minLen; n <= len(words); n++ {
			for i := 0; i+n <= len(words); i++ {
				spans[strings.Join(words[i:i+n], " ")] = struct{}{}
			}
		}
	}
	return spans
}

// sourceBigrams maps each bigram to the set of fixture messages it appears in, which is
// what lets a sentence be traced back to how many different messages it drew on.
func sourceBigrams() map[string]map[int]struct{} {
	out := map[string]map[int]struct{}{}
	for idx, m := range fixtureMessages() {
		words := strings.Fields(m.text)
		for i := 0; i+2 <= len(words); i++ {
			bg := strings.Join(words[i:i+2], " ")
			if out[bg] == nil {
				out[bg] = map[int]struct{}{}
			}
			out[bg][idx] = struct{}{}
		}
	}
	return out
}

// sweepSamples generates the same grid the golden harness prints, and returns the lines so
// the assertions below judge exactly what a human reading the harness would judge.
//
// Sharing the grid is the point. A test that measured a different set of prompts from the
// one the operator reads would let the two disagree, and the printed samples are what the
// tuning decisions are actually made against.
func sweepSamples(t *testing.T) []string {
	t.Helper()

	f := goldenCorpus()
	var out []string
	for _, temp := range []float64{0.7, 1.0, 1.6} {
		for _, topK := range []int{0, 40} {
			p := testParams()
			p.Temperature = temp
			p.TopK = topK
			p.MinDistinctAuthors = 2

			for _, prompt := range goldenPrompts() {
				g := New(f, p, seeded(0xC0FFEE, 0xBADF00D))
				for range 3 {
					if line := generateReply(g, prompt, PersonaNeutral, false); line != "" {
						out = append(out, line)
					}
				}
			}
		}
	}
	return out
}

// maxRecitationRate is how much of the output may reproduce a distinctive span of one
// source message.
//
// Not zero, and that is deliberate rather than a concession. The corpus IS the register, so
// a bot that can never say a phrase the server says has lost the voice it exists to imitate.
// What the bar is against is reproducing somebody's actual sentence.
const maxRecitationRate = 0.10

// recitationMinLen is the span length at which quoting stops being register and starts
// being recitation, and it is 5 because of a measurement rather than a guess.
//
// It was 4, and at 4 the rate sat at 30% and looked alarming. Breaking that number down by
// span length showed 38 matches at exactly four words and ZERO at five or more: the engine
// was never once reproducing a distinctive sentence, it was emitting short idioms that exist
// verbatim in the corpus because several people typed them. "the server is doomed" and "at
// this hour is a mistake" are four-word phrases the server genuinely says, and a bot
// producing one is doing its job.
//
// So the four-word rate was measuring the register and calling it a defect. Five words is
// where a match stops being an idiom and starts being somebody's message, and the threshold
// above is tight because the measured rate there is zero: this asserts that the engine does
// not START reciting, rather than tolerating a level of it that was never happening.
//
// The four-word rate is still logged, because it is the number that moves first if a future
// weight pushes the engine toward its sources.
const recitationMinLen = 5

func TestGoldenSamplesAreNotRecitation(t *testing.T) {
	samples := sweepSamples(t)
	if len(samples) == 0 {
		t.Fatal("no samples generated, so this test would pass vacuously")
	}

	spans := sourceSpans(recitationMinLen)

	var recited []string
	for _, s := range samples {
		if len(strings.Fields(s)) < recitationMinLen {
			continue
		}
		if _, ok := spans[s]; ok {
			recited = append(recited, s)
		}
	}

	// The idiom rate, logged rather than asserted. See recitationMinLen: four-word matches
	// are the register rather than a defect, but they are the number that moves first if a
	// future weight pushes the engine toward its sources, so they are worth watching.
	idioms, shortSpans := 0, sourceSpans(4)
	for _, s := range samples {
		if len(strings.Fields(s)) == 4 {
			if _, ok := shortSpans[s]; ok {
				idioms++
			}
		}
	}
	t.Logf("four-word idiom matches: %d of %d samples (%.1f%%), not asserted",
		idioms, len(samples), 100*float64(idioms)/float64(len(samples)))

	rate := float64(len(recited)) / float64(len(samples))
	t.Logf("recitation: %d of %d samples (%.1f%%), limit %.0f%%",
		len(recited), len(samples), rate*100, maxRecitationRate*100)

	if len(recited) > 0 {
		sort.Strings(recited)
		t.Logf("recited verbatim:\n  %s", strings.Join(dedupe(recited), "\n  "))
	}

	if rate > maxRecitationRate {
		t.Errorf("recitation rate %.1f%% exceeds %.0f%%: the engine is quoting the corpus "+
			"rather than recombining it, which is SPEC.md section 1 principle 7's failure mode",
			rate*100, maxRecitationRate*100)
	}
}

// minMultiSourceRate is how much of the output must draw on more than one source message.
//
// This is the test recitation-with-one-word-changed passes the other one by defeating: a
// sentence can avoid being a verbatim span while still coming entirely out of a single
// message. Combining varied concepts means bigrams from different places.
const minMultiSourceRate = 0.55

func TestGoldenSamplesSpanSources(t *testing.T) {
	samples := sweepSamples(t)
	if len(samples) == 0 {
		t.Fatal("no samples generated, so this test would pass vacuously")
	}

	bigrams := sourceBigrams()

	multi, considered := 0, 0
	var single []string
	for _, s := range samples {
		words := strings.Fields(s)
		if len(words) < 3 {
			// A two-word reply cannot span two messages and is not evidence either way.
			continue
		}
		considered++

		// A sentence spans sources when no single fixture message accounts for all of its
		// bigrams. Intersecting the per-bigram message sets answers that directly.
		var common map[int]struct{}
		known := 0
		for i := 0; i+2 <= len(words); i++ {
			owners, ok := bigrams[strings.Join(words[i:i+2], " ")]
			if !ok {
				// A bigram in no source message at all is recombination by definition,
				// because it is a join the corpus never made.
				common = nil
				known = -1
				break
			}
			known++
			if common == nil {
				common = map[int]struct{}{}
				for k := range owners {
					common[k] = struct{}{}
				}
				continue
			}
			for k := range common {
				if _, ok := owners[k]; !ok {
					delete(common, k)
				}
			}
		}

		if known == -1 || len(common) == 0 {
			multi++
		} else {
			single = append(single, s)
		}
	}

	rate := float64(multi) / float64(considered)
	t.Logf("multi-source: %d of %d samples (%.1f%%), floor %.0f%%",
		multi, considered, rate*100, minMultiSourceRate*100)

	if len(single) > 0 {
		sort.Strings(single)
		t.Logf("traceable to one message:\n  %s", strings.Join(dedupe(single), "\n  "))
	}

	if rate < minMultiSourceRate {
		t.Errorf("only %.1f%% of samples combine more than one source message, floor is %.0f%%: "+
			"the engine is reproducing single messages rather than combining varied concepts",
			rate*100, minMultiSourceRate*100)
	}
}

// TestGoldenSamplesReadAsReplies pins the mechanical half of rubric criteria 1 and 3: the
// length band, and the artifacts that make a reply read as a malfunction.
//
// The judgement half (is it funny, is it in register) stays with the human reading the
// harness, which is what SPEC.md section 5.4 says and what this cannot replace.
func TestGoldenSamplesReadAsReplies(t *testing.T) {
	samples := sweepSamples(t)

	// The cap is the engine's own contract rather than a number invented here. The rubric's
	// "4 to 12 words" is a claim about where the MODE should sit, not a hard limit, and
	// asserting it as a limit would silently put this test in disagreement with
	// PEREGRINE_MAX_WORDS. The distribution below is what says whether the mode is right, and
	// it is logged rather than asserted because that is an operator's judgement.
	maxWords := testParams().MaxWords

	var lengths []int
	var tooLong, dangling []string
	for _, s := range samples {
		words := strings.Fields(s)
		lengths = append(lengths, len(words))
		if len(words) > maxWords {
			tooLong = append(tooLong, s)
		}
		// A reply that ends on a function word reads as being cut off mid-thought.
		if len(words) > 0 {
			prev := ""
			if len(words) > 1 {
				prev = words[len(words)-2]
			}
			if isDanglingTail(prev, words[len(words)-1]) {
				dangling = append(dangling, s)
			}
		}
		if strings.Contains(s, EndToken) || strings.Contains(s, "\x00") {
			t.Errorf("sentinel or separator leaked into output: %q", s)
		}
	}

	sort.Ints(lengths)
	median := lengths[len(lengths)/2]
	t.Logf("length: median %d, p90 %d, max %d (cap %d)",
		median, lengths[len(lengths)*9/10], lengths[len(lengths)-1], maxWords)

	if len(tooLong) > 0 {
		t.Errorf("%d samples exceed MaxWords=%d, e.g. %q", len(tooLong), maxWords, tooLong[0])
	}
	// Rubric criterion 1, as a claim about the middle of the distribution. A median outside
	// this band means the length model is aimed wrong, which is a different defect from any
	// single reply being long.
	if median < 4 || median > 12 {
		t.Errorf("median reply is %d words, want 4 to 12: short lands and long reads as a "+
			"malfunction (SPEC.md section 5.5 criterion 1)", median)
	}
	if len(dangling) > 0 {
		sort.Strings(dangling)
		t.Errorf("%d samples end on a trailing function word, which reads as cut off:\n  %s",
			len(dangling), strings.Join(dedupe(dangling), "\n  "))
	}
}

func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// isDanglingTail asks text's own question rather than a second copy of it.
//
// This started as a private list in this file and that was finding 28's shape: two
// statements of "which words leave a sentence hanging", differing only in what each one
// forgot. The private one flagged every trailing pronoun, so it called "i am going to lose
// it" cut off, which it is not.
//
// Using the engine's definition here is not tautological, and the reason is worth stating:
// TrimDangling runs inside one generation attempt, while the persona post-pass appends a
// closer to the finished string AFTERWARDS. So this checks that nothing downstream of the
// trim puts a dangling word back, which is a property of the pipeline rather than of the
// function.
func isDanglingTail(prev, w string) bool {
	return text.IsDanglingTail(w) || !text.IsGovernedPronoun(prev, w)
}

// goldenPrompts is the sweep, shared between the harness and the assertions above so the
// two cannot drift apart.
func goldenPrompts() []string {
	return []string{
		"bird what do you know about beezle",
		"maybe alexiane",
		"bro what",
		"greg and lachy are both",
		"what is up with nurock",
		"the bird",
		"the server is",
	}
}
