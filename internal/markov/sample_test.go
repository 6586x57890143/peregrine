package markov

import (
	"math"
	"testing"
)

// logits builds a candidate slice directly, so the sampler can be tested without a
// corpus at all.
func logits(pairs ...any) []candidate {
	out := make([]candidate, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, candidate{token: pairs[i].(string), logit: pairs[i+1].(float64)})
	}
	return out
}

// distribution samples many times and returns observed frequencies.
func distribution(t *testing.T, g *Generator, cands []candidate, n int) map[string]float64 {
	t.Helper()
	counts := map[string]int{}
	for range n {
		fresh := append([]candidate(nil), cands...)
		counts[g.sample(fresh)]++
	}
	out := make(map[string]float64, len(counts))
	for k, v := range counts {
		out[k] = float64(v) / float64(n)
	}
	return out
}

// TestTemperatureActuallyMovesTheDistribution is the pin for finding G3, and it is the
// test the old engine could not have passed.
//
// Creativity was applied as an exponent of 1/(Creativity+0.01), so at its 0.75 default
// the exponent was 1.316, which SHARPENED the distribution, and raising the knob toward
// 1 could only approach an exponent of 1.0 and never pass it. The dial could not reach
// the half of its own range that adds chaos. Temperature must move in both directions
// from 1.0, or the chaos dial is decorative again.
func TestTemperatureActuallyMovesTheDistribution(t *testing.T) {
	cands := logits("strong", 2.0, "weak", 0.0)

	entropyAt := func(temp float64) float64 {
		p := testParams()
		p.Temperature = temp
		p.TopK = 0
		p.TopP = 1
		g := New(newFake(), p, seeded(7, 11))
		d := distribution(t, g, cands, 20000)
		var h float64
		for _, v := range d {
			if v > 0 {
				h -= v * math.Log(v)
			}
		}
		return h
	}

	cold, warm, hot := entropyAt(0.5), entropyAt(1.0), entropyAt(4.0)

	if !(cold < warm && warm < hot) {
		t.Errorf("entropy must increase with temperature, got cold=%.4f warm=%.4f hot=%.4f. "+
			"If raising temperature does not flatten the distribution the dial is the "+
			"Creativity trap again (SPEC.md finding G3)", cold, warm, hot)
	}

	// And the interesting half of the range must be reachable: at high temperature the
	// weak candidate is picked a substantial fraction of the time.
	p := testParams()
	p.Temperature = 4.0
	p.TopK = 0
	p.TopP = 1
	g := New(newFake(), p, seeded(7, 11))
	if got := distribution(t, g, cands, 20000)["weak"]; got < 0.3 {
		t.Errorf("at T=4 the weaker candidate was picked %.1f%% of the time, want at least "+
			"30%%: temperature must be able to reach genuine chaos", got*100)
	}
}

func TestZeroTemperatureIsArgmax(t *testing.T) {
	p := testParams()
	p.Temperature = 0
	g := New(newFake(), p, seeded(1, 2))

	for range 50 {
		if got := g.sample(logits("weak", 0.0, "strong", 2.0)); got != "strong" {
			t.Fatalf("T=0 sampled %q, want the argmax. Zero is the degenerate end of the "+
				"dial and should be reachable on purpose, since it is what the old engine "+
				"did by accident", got)
		}
	}
}

// TestTopKTruncatesTheTail is why a high temperature is usable at all: the corpus is
// mostly one-count continuations, so without truncation raising temperature just
// promotes noise.
func TestTopKTruncatesTheTail(t *testing.T) {
	cands := logits("a", 1.0, "b", 0.9, "c", 0.8, "d", 0.7, "e", 0.6, "f", 0.5)

	p := testParams()
	p.Temperature = 5.0 // hot enough that everything would otherwise be sampled
	p.TopK = 2
	p.TopP = 1
	g := New(newFake(), p, seeded(3, 5))

	d := distribution(t, g, cands, 5000)
	for _, tok := range []string{"c", "d", "e", "f"} {
		if d[tok] != 0 {
			t.Errorf("token %q outside the top 2 was sampled %.4f of the time, want never", tok, d[tok])
		}
	}
	if d["a"] == 0 || d["b"] == 0 {
		t.Errorf("both survivors must be reachable, got a=%.4f b=%.4f", d["a"], d["b"])
	}
}

func TestTopPKeepsTheNucleusAndAlwaysAtLeastOne(t *testing.T) {
	cands := logits("dominant", 8.0, "tail1", 0.0, "tail2", -1.0)

	p := testParams()
	p.Temperature = 1.0
	p.TopK = 0
	p.TopP = 0.9
	g := New(newFake(), p, seeded(9, 13))

	d := distribution(t, g, cands, 5000)
	if d["dominant"] != 1.0 {
		t.Errorf("a candidate holding more than TopP of the mass must be the only survivor, "+
			"got %v", d)
	}

	// TopP of 0 must still leave one candidate rather than none, or a legal
	// configuration produces silence.
	p.TopP = 0.0001
	g = New(newFake(), p, seeded(9, 13))
	if got := g.sample(append([]candidate(nil), cands...)); got == "" {
		t.Error("a tiny TopP returned no token; the candidate that crosses the threshold is " +
			"kept, so there is always at least one survivor")
	}
}

// TestSampleIsStableUnderEqualLogits guards the property M6b had to delete a heuristic
// over. Candidates arrive in sorted order from a cursor scan, so a tie broken by slice
// position would hand a permanent advantage to whichever token sorts first, at every
// step of every sentence.
func TestSampleIsStableUnderEqualLogits(t *testing.T) {
	p := testParams()
	p.Temperature = 1.0
	p.TopK = 0
	p.TopP = 1
	g := New(newFake(), p, seeded(21, 22))

	d := distribution(t, g, logits("alpha", 1.0, "omega", 1.0), 20000)
	if math.Abs(d["alpha"]-d["omega"]) > 0.05 {
		t.Errorf("equal logits produced unequal frequencies alpha=%.3f omega=%.3f; a tie "+
			"must not resolve by position", d["alpha"], d["omega"])
	}
}

// TestSampleSurvivesExtremeLogits covers the max-subtraction. Without it, logits from a
// large corpus overflow to +Inf when exponentiated and the whole distribution becomes
// NaN, which does not error: it silently returns the last candidate forever.
func TestSampleSurvivesExtremeLogits(t *testing.T) {
	p := testParams()
	p.Temperature = 0.01 // divides logits by 100, so 900 becomes 90000
	g := New(newFake(), p, seeded(1, 1))

	got := g.sample(logits("huge", 900.0, "tiny", -900.0))
	if got != "huge" {
		t.Errorf("extreme logits sampled %q, want \"huge\". Exponentiating without "+
			"subtracting the maximum overflows to +Inf and yields NaN", got)
	}
}

func TestSampleIsReproducibleUnderASeededSource(t *testing.T) {
	cands := logits("a", 1.0, "b", 0.8, "c", 0.6, "d", 0.4)

	run := func() []string {
		g := New(newFake(), testParams(), seeded(42, 43))
		out := make([]string, 0, 30)
		for range 30 {
			out = append(out, g.sample(append([]candidate(nil), cands...)))
		}
		return out
	}

	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("draw %d differed between runs: %q vs %q. The golden harness cannot "+
				"attribute an output change to a weight if the sampler is not reproducible",
				i, a[i], b[i])
		}
	}
}
