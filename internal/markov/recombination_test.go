package markov

import (
	"fmt"
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
	out := make([]string, 0, 256)
	for _, s := range sweepAttributed(t) {
		out = append(out, s.line)
	}
	return out
}

// sample is one generated line and the prompt that produced it.
type sample struct {
	prompt string
	line   string

	// cell identifies the temperature, top-k and styled combination, so a variety gate can
	// ask whether the draws WITHIN one configuration differ without comparing across
	// configurations that are meant to differ.
	cell string
}

// sweepAttributed generates the grid and keeps which prompt produced each line.
//
// sweepSamples used to be this without the attribution, which the two gates added in M19 both
// need: "did the reply answer its prompt" and "is one phrase answering every prompt" are both
// questions about the pairing rather than about the lines.
func sweepAttributed(t *testing.T) []sample {
	t.Helper()

	f := goldenCorpus()
	var out []sample
	for _, temp := range []float64{0.7, 1.0, 1.6} {
		for _, topK := range []int{0, 40} {
			p := testParams()
			p.Temperature = temp
			p.TopK = topK
			p.MinDistinctAuthors = 2

			for _, prompt := range goldenPrompts() {
				// STYLED AND UNSTYLED BOTH, because the persona post-pass is a producer of
				// words like any other and the sweep could not see it. Style appends and
				// splices AFTER TrimDangling has run, so a sample that skips it cannot say
				// whether the finished string is well formed.
				for _, styled := range []bool{false, true} {
					// A DIFFERENT SEED PER PASS, and this was a real defect rather than a
					// refinement. Both passes used seeded(0xC0FFEE, 0xBADF00D), so the styled
					// run replayed the identical stream and produced the same text plus
					// filler: half the sweep was a decorated copy of the other half. Every
					// rate computed over it was therefore computed over a sample set that was
					// half duplicates, which matters most for the variety gate below, where
					// duplication is the thing being measured.
					seed := uint64(0xBADF00D)
					if styled {
						seed = 0x5CA1AB1E
					}
					g := New(f, p, seeded(0xC0FFEE, seed))

					cell := fmt.Sprintf("t=%.1f k=%d styled=%v", temp, topK, styled)
					for range 3 {
						if line := generateReply(g, prompt, PersonaNeutral, styled); line != "" {
							out = append(out, sample{prompt: prompt, line: line, cell: cell})
						}
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
	//
	// MaxWords bounds GENERATION, and the persona post-pass appends filler to the finished
	// string afterwards, so the posted message can legitimately exceed it. The allowance is
	// derived from the longest filler this package can actually add rather than guessed,
	// because a hardcoded slack would silently stop matching if a longer meta-comment were
	// added. That is worth stating: the two numbers are about different things and reading
	// MaxWords as a bound on what lands in the channel would be wrong.
	maxWords := testParams().MaxWords + longestFiller()

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

// longestFiller is the most words the persona post-pass can add to a finished sentence.
//
// Derived from the filler sets rather than hardcoded, so that adding a longer meta-comment
// cannot silently make the length assertion above stop matching what Style can produce.
func longestFiller() int {
	longest := 0
	for _, set := range [][]string{openers, closers, interjections, metaComments} {
		for _, f := range set {
			if n := len(strings.Fields(f)); n > longest {
				longest = n
			}
		}
	}
	return longest
}

// ---------------------------------------------------------------- criterion 7

// minTopicalRatio is how much more a reply must be about the person its prompt named than
// about the other four people in the fixture.
//
// MEASURED, not chosen, and measured twice. On the engine before M19's context work, 144
// name-bearing samples gave 40 on-topic hits against 30 off-topic, a ratio of 1.33. With graded
// recency at its shipped weight the same sweep gives 44 against 16, a ratio of 2.75.
//
// The floor sits well below the achieved value on purpose. The number that matters is not how
// high the ratio is but whether it can fall to 1.0, which is chance, or below it, which is the
// bot answering about somebody else entirely. The counts are small enough that the ratio is
// jumpy, so a floor close to the measurement would fail on noise rather than on a regression.
//
// It is a modest ratio and that is the fixture rather than the engine: the corpus is built on
// shared idioms so that recombination has somewhere to come from, and those bridges produce
// genuine cross-talk. A fixture with cleanly separated topics would score far higher here and
// would be a worse instrument for every other gate.
const minTopicalRatio = 1.40

// minEngagementRate is how often a reply to a prompt naming somebody must actually engage with
// that person, by naming them or by using vocabulary the corpus ties to them.
//
// This is the second half of the gate and it exists because the ratio above has a blind spot
// found by reading samples: a reply saying nothing about anybody contributes to neither side,
// so a bot that went quiet on every named prompt would score a perfect ratio. The two together
// say "engage, and engage with the right person".
//
// MEASURED at the shipped weights: of 144 name-bearing samples, 50.0% engage, 11.1% reach for
// the wrong person and 38.9% do neither. The floor is set at the operator's stated target of
// roughly one reply in three rather than at the measurement, because that target is the
// requirement and the surplus is headroom.
const minEngagementRate = 0.33

// TestGoldenSamplesRespondToTheirPrompt is SPEC.md section 5.5 criterion 7.
//
// # The hole it closes
//
// Nothing else in the suite asserts that a reply has anything to do with what was said to it.
// Recitation, spans-sources and the length-and-artifact gate would all pass a bot that ignored
// its prompt completely and emitted well-formed, recombined, correctly-lengthed corpus soup.
// Criterion 4 is pinned by TestSeedDrawsTheNameOftenEnoughToNotice, which measures the SEED
// draw and says nothing about the finished reply.
//
// # Why it is discriminative rather than a threshold on overlap
//
// An absolute "the reply must share N words with the prompt" is satisfied by a fixture that is
// topically narrow, and it would measure ECHO rather than aboutness: a reply that parrots the
// prompt back scores perfectly and is the failure mode next door. So this compares each reply
// against the person its prompt named versus the four it did not, using only the vocabulary
// that actually distinguishes them (see distinctiveWords, which is fixture data for exactly
// this reason).
//
// AGGREGATE, never per reply. The fixture's bridge words exist to make recombination possible
// and produce cross-talk on purpose; asserting per reply would be asserting that the bridges
// do not work.
func TestGoldenSamplesRespondToTheirPrompt(t *testing.T) {
	samples := sweepAttributed(t)
	if len(samples) == 0 {
		t.Fatal("no samples generated, so this gate would pass vacuously")
	}

	dist := distinctiveWords()
	people := make([]string, 0, len(dist))
	for p := range dist {
		people = append(people, p)
	}
	sort.Strings(people)

	var onTopic, offTopic, named, engaged, wrongPerson int
	for _, s := range samples {
		inPrompt := map[string]bool{}
		for _, p := range people {
			if strings.Contains(s.prompt, p) {
				inPrompt[p] = true
			}
		}
		if len(inPrompt) == 0 {
			continue
		}
		named++

		said := map[string]bool{}
		for _, w := range strings.Fields(s.line) {
			said[w] = true
		}
		var hitOwn, hitOther bool
		for _, person := range people {
			// Naming the person outright engages with them as surely as using their
			// vocabulary does, and the seed's name tier exists to make it happen.
			if inPrompt[person] && said[person] {
				hitOwn = true
			}
			for _, w := range dist[person] {
				if !said[w] {
					continue
				}
				if inPrompt[person] {
					onTopic++
					hitOwn = true
				} else {
					offTopic++
					hitOther = true
				}
			}
		}
		switch {
		case hitOwn:
			engaged++
		case hitOther:
			wrongPerson++
		}
	}

	if named == 0 {
		t.Fatal("no prompt in the sweep names anybody, so this gate cannot measure anything")
	}

	ratio := float64(onTopic) / float64(max(offTopic, 1))
	rate := float64(engaged) / float64(named)
	t.Logf("topicality: %d name-bearing samples, on-topic %d, off-topic %d, ratio %.2f (floor %.2f)",
		named, onTopic, offTopic, ratio, minTopicalRatio)
	t.Logf("engagement: %d of %d replies engage the person named (%.1f%%, floor %.0f%%); "+
		"%d reach for somebody else", engaged, named, rate*100, minEngagementRate*100, wrongPerson)

	if ratio < minTopicalRatio {
		t.Errorf("replies are only %.2fx more about the person their prompt named than about "+
			"somebody else, floor %.2f: at 1.0 the bot is answering at chance, which is the "+
			"failure SPEC.md section 5.5 criterion 7 exists to catch", ratio, minTopicalRatio)
	}
	if rate < minEngagementRate {
		t.Errorf("only %.1f%% of replies to a prompt naming somebody engage with that person, "+
			"floor %.0f%%: the ratio above cannot catch this on its own, because a reply that "+
			"says nothing about anybody counts on neither side of it",
			rate*100, minEngagementRate*100)
	}
}

// ---------------------------------------------------------------- criterion 8

// The variety bounds, all three MEASURED on the engine as it stood when this gate was written
// rather than chosen in advance.
//
// Baseline at that point, over 252 samples and 7 prompts: the worst trigram by share was
// "going to lose" in 18.3% of samples across 4 prompts; the widest-spread was "the bird
// ratioed" across 5 of 7 prompts at 8.7%; and 12 of 84 prompt-and-cell groups (14.3%) produced
// three identical draws.
//
// The caps sit above those with room to move, because the question this gate answers is not
// "is concentration low" but "is it getting worse". Some concentration is honest on a
// 149-message fixture where the author-diversity gate admits only well-attested paths: the
// engine is funnelled into the few phrases two people both said. A real corpus widens that
// funnel by orders of magnitude, which is why these are not tuned toward zero.
const (
	maxAttractorShare  = 0.25
	maxAttractorSpread = 6.0 / 7.0
	maxIdenticalDraws  = 0.25
)

// attractorMinContent is how many of a trigram's three words must carry meaning before it
// counts. Two, so that a run of function words shared by every sentence in English does not
// register as the bot repeating itself.
const attractorMinContent = 2

// TestGoldenSamplesDoNotCollapseToOnePhrase is SPEC.md section 5.5 criterion 8.
//
// # The hole it closes
//
// Nothing measured variety at all. Every other gate is about one sample against the corpus;
// none of them compares samples to each other, so the suite was silent on the failure where
// one phrase becomes the answer to everything. Two shapes of it were visible in live output
// and in the golden sweep, and both passed every existing assertion:
//
//   - CROSS-PROMPT. "i am going to lose it" answering "bro what", "the bird" and "bird what do
//     you know about beezle" alike. One well-attested path sits at the centre of the corpus
//     and the engine falls into it regardless of what was asked.
//   - WITHIN-PROMPT. Three consecutive draws for "the server is" returning "the server is
//     doomed" twice. Top-k and temperature are meaningless when the eligible set is one.
//
// # Why this ships with M19 rather than after it
//
// It is the gate the context work is most likely to break. Grading recency, recalling names
// and feeding a referenced message all pull every reply toward the same recent material, which
// is the same motion as falling into an attractor. A gate written after the change it polices
// gets its threshold chosen to pass.
//
// What it must NOT flag is the register. The fixture defends verbatim memes on purpose:
// "bird moment", "ratio ratio ratio", "no cap fr fr". A variety floor that treats those as
// repetition is measuring the voice, which is the mistake the four-word recitation threshold
// made once already.
func TestGoldenSamplesDoNotCollapseToOnePhrase(t *testing.T) {
	samples := sweepAttributed(t)
	if len(samples) == 0 {
		t.Fatal("no samples generated, so this gate would pass vacuously")
	}

	prompts := map[string]struct{}{}
	share := map[string]int{}
	spread := map[string]map[string]struct{}{}

	for _, s := range samples {
		prompts[s.prompt] = struct{}{}
		words := strings.Fields(s.line)

		// Counted once per sample, so a sentence that stutters does not inflate its own
		// trigram's share. Stuttering is the repetition penalties' problem, not this gate's.
		seen := map[string]struct{}{}
		for i := 0; i+3 <= len(words); i++ {
			content := 0
			for _, w := range words[i : i+3] {
				if !text.IsStopWord(w) {
					content++
				}
			}
			if content < attractorMinContent {
				continue
			}
			key := strings.Join(words[i:i+3], " ")
			if spread[key] == nil {
				spread[key] = map[string]struct{}{}
			}
			spread[key][s.prompt] = struct{}{}
			if _, dup := seen[key]; !dup {
				seen[key] = struct{}{}
				share[key]++
			}
		}
	}

	worstShare, worstShareKey := 0.0, ""
	worstSpread, worstSpreadKey := 0.0, ""
	for key, n := range share {
		if s := float64(n) / float64(len(samples)); s > worstShare {
			worstShare, worstShareKey = s, key
		}
		if s := float64(len(spread[key])) / float64(len(prompts)); s > worstSpread {
			worstSpread, worstSpreadKey = s, key
		}
	}
	t.Logf("attractors: worst share %.1f%% (%q), worst spread %.0f%% of prompts (%q)",
		worstShare*100, worstShareKey, worstSpread*100, worstSpreadKey)

	if worstShare > maxAttractorShare {
		t.Errorf("the trigram %q is in %.1f%% of all replies, cap %.0f%%: one phrase is "+
			"becoming the answer to everything", worstShareKey, worstShare*100, maxAttractorShare*100)
	}
	if worstSpread > maxAttractorSpread {
		t.Errorf("the trigram %q appears in replies to %.0f%% of distinct prompts, cap %.0f%%: "+
			"an answer that fits every question is not answering any of them",
			worstSpreadKey, worstSpread*100, maxAttractorSpread*100)
	}

	// The within-prompt half. Grouped by prompt AND configuration, so this asks whether the
	// three draws from one generator differ, not whether two different temperatures agree.
	groups := map[string][]string{}
	for _, s := range samples {
		key := s.cell + "|" + s.prompt
		groups[key] = append(groups[key], s.line)
	}

	identical, considered := 0, 0
	var examples []string
	for key, lines := range groups {
		if len(lines) < 2 {
			continue
		}
		considered++
		same := true
		for _, l := range lines[1:] {
			if l != lines[0] {
				same = false
				break
			}
		}
		if same {
			identical++
			examples = append(examples, key+" -> "+lines[0])
		}
	}
	if considered == 0 {
		t.Fatal("no prompt produced more than one draw, so the within-prompt half cannot measure anything")
	}

	rate := float64(identical) / float64(considered)
	t.Logf("identical draws: %d of %d prompt-and-cell groups (%.1f%%, cap %.0f%%)",
		identical, considered, rate*100, maxIdenticalDraws*100)

	if rate > maxIdenticalDraws {
		sort.Strings(examples)
		t.Errorf("%.1f%% of prompt-and-cell groups produced three identical replies, cap %.0f%%: "+
			"top-k and temperature are meaningless when the eligible set is one.\n  %s",
			rate*100, maxIdenticalDraws*100, strings.Join(dedupe(examples), "\n  "))
	}
}
