package markov

import "testing"

// TestNewLengthStaysInRangeAndSkewsShort is the pin for finding G7.
//
// The old arrangement had three competing length mechanisms and a cap of 30 + rand(15)
// words, which is a paragraph. A forty-word Markov ramble reads as a malfunction in a
// chat channel; short and punchy reads as a joke (SPEC.md section 1). The skew is the
// property that matters, not the exact distribution: the mode belongs at the floor with
// a tail that can still reach the cap.
func TestNewLengthStaysInRangeAndSkewsShort(t *testing.T) {
	src := seeded(1, 2)
	const runs = 20000
	const min, max = 4, 18

	counts := map[int]int{}
	var total int
	for range runs {
		l := NewLength(src, min, max)
		if l.Target < min || l.Target > max {
			t.Fatalf("target %d outside [%d, %d]", l.Target, min, max)
		}
		if l.Min != min || l.Max != max {
			t.Fatalf("bounds not carried through: %+v", l)
		}
		counts[l.Target]++
		total += l.Target
	}

	mean := float64(total) / runs
	midpoint := float64(min+max) / 2
	if mean >= midpoint {
		t.Errorf("mean target %.2f is at or above the midpoint %.2f, so the distribution "+
			"is not skewed short", mean, midpoint)
	}

	// The tail must still reach the cap, or the cap is decorative and length never
	// varies enough to be interesting.
	var nearMax int
	for target, n := range counts {
		if target >= max-2 {
			nearMax += n
		}
	}
	if nearMax == 0 {
		t.Error("no sampled target came within two words of the cap; the tail must reach it")
	}
}

// TestNewLengthToleratesABadRange. Length is not worth failing a reply over: an inverted
// range is a configuration mistake that validation should have caught, and generating a
// short sentence is a better response than panicking inside a message handler.
func TestNewLengthToleratesABadRange(t *testing.T) {
	src := seeded(1, 2)
	for _, c := range []struct{ min, max int }{
		{0, 0}, {-5, -1}, {10, 3}, {1, 1},
	} {
		l := NewLength(src, c.min, c.max)
		if l.Min < 1 || l.Max < l.Min || l.Target < l.Min || l.Target > l.Max {
			t.Errorf("NewLength(%d, %d) = %+v, which is not internally consistent", c.min, c.max, l)
		}
	}
}

// TestEndLogitHasThreeRegimes covers the shape of the length pressure, which is the
// whole mechanism now that the multiplier and the discard-and-retry are gone.
func TestEndLogitHasThreeRegimes(t *testing.T) {
	w := DefaultWeights()
	l := Length{Min: 4, Max: 18, Target: 9}

	// Below the floor: the full penalty, and NOT negative infinity. A corpus where the
	// only continuation is the sentinel must still be able to end.
	if got := l.endLogit(w, 2); got != w.EndEarly {
		t.Errorf("at length 2 (below floor %d) got %.3f, want the full penalty %.3f",
			l.Min, got, w.EndEarly)
	}

	// Between floor and target: easing toward zero, monotonically.
	prev := l.endLogit(w, l.Min)
	for length := l.Min + 1; length < l.Target; length++ {
		got := l.endLogit(w, length)
		if got <= prev {
			t.Errorf("penalty at length %d (%.3f) did not ease relative to %d (%.3f); the "+
				"target must be a preference, not a second floor", length, got, length-1, prev)
		}
		prev = got
	}

	// At the target: neutral.
	if got := l.endLogit(w, l.Target); got != 0 {
		t.Errorf("at the target got %.3f, want 0", got)
	}

	// Past the target: a growing bonus, capped.
	first := l.endLogit(w, l.Target+1)
	if first <= 0 {
		t.Errorf("past the target got %.3f, want a positive bonus so a long sentence can "+
			"end on its own rather than only at the cap", first)
	}
	if got := l.endLogit(w, l.Target+50); got != w.EndLateCap {
		t.Errorf("far past the target got %.3f, want the cap %.3f: an end token that is "+
			"certain regardless of context makes every long sentence stop in the same "+
			"grammatical place", got, w.EndLateCap)
	}
}

// TestEndLogitWithATargetAtTheFloor covers the degenerate range where Min equals Target,
// which the easing branch would divide by zero on.
func TestEndLogitWithATargetAtTheFloor(t *testing.T) {
	w := DefaultWeights()
	l := Length{Min: 4, Max: 18, Target: 4}
	if got := l.endLogit(w, 4); got != 0 {
		t.Errorf("at a target equal to the floor got %.3f, want 0", got)
	}
	if got := l.endLogit(w, 3); got != w.EndEarly {
		t.Errorf("below the floor got %.3f, want %.3f", got, w.EndEarly)
	}
}

func TestLengthDoneAtTheCap(t *testing.T) {
	l := Length{Min: 4, Max: 10, Target: 6}
	if l.Done(9) {
		t.Error("Done before the cap")
	}
	if !l.Done(10) {
		t.Error("not Done at the cap")
	}
	if !l.Done(11) {
		t.Error("not Done past the cap")
	}
}

// TestTheLengthModelActuallyEndsSentences is the end-to-end check that the three
// regimes add up to sentences that stop by themselves rather than only at the cap. If
// every sentence ran to Max, the model would be doing nothing and the cap would be the
// only mechanism, which is the situation before this milestone.
func TestTheLengthModelActuallyEndsSentences(t *testing.T) {
	f := goldenCorpus()
	p := testParams()
	p.MinDistinctAuthors = 0
	g := New(f, p, seeded(21, 22))

	var atCap, total int
	const runs = 200
	for range runs {
		n := len(generate(g, "the bird", PersonaNeutral))
		total += n
		if n >= p.MaxWords {
			atCap++
		}
	}
	if atCap*2 > runs {
		t.Errorf("%d/%d sentences ran to the hard cap; the end-token pressure is not doing "+
			"the work and the cap is the only mechanism", atCap, runs)
	}
	if avg := float64(total) / runs; avg > float64(p.MaxWords)*0.8 {
		t.Errorf("average length %.1f against a cap of %d, which is not short", avg, p.MaxWords)
	}
}

// generate returns the word slice directly, so no splitting helper is needed here.
