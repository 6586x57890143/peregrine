package markov

import "math"

// The length model: ONE mechanism, replacing three that competed.
//
// The old arrangement had an end-token multiplier applied below 40% progress, a
// discard-and-retry that threw away an end token if the sentence was too short, and a
// loop bound of 30 + rand(15) words. Three mechanisms, no single place that decided how
// long a sentence should be, and a cap that produces a paragraph. In a chat channel a
// forty-word Markov ramble reads as a malfunction; short and punchy reads as a joke
// (SPEC.md section 1).
//
// What replaces them is: sample a target once per sentence from a distribution skewed
// short, shift the end-token logit relative to that target, and force the end at the
// cap. The target is sampled rather than fixed so that length varies, which matters for
// a bot that posts often enough for a constant rhythm to become noticeable.

// Length carries the bounds and the sampled target for one sentence.
type Length struct {
	// Min is the floor. Below it the end token is penalized hard.
	Min int

	// Max is the hard cap. At it the sentence ends whatever the model wants.
	Max int

	// Target is the sampled preferred length, between Min and Max.
	Target int
}

// NewLength samples a target between min and max, skewed toward the short end.
//
// The skew is a squared uniform, which is the simplest distribution with the property
// wanted here: the mode is at the floor, the tail reaches the cap, and there is no
// tuning constant to argue about. Concretely, with a 4 to 18 range the median lands
// around 8 words and roughly a fifth of sentences exceed 12.
//
// A caller passing a bad range gets a sane one rather than a panic, because length is
// not worth failing a reply over: an inverted range is a configuration mistake that
// config validation should have caught, and generating a short sentence is a better
// response to it than generating none.
func NewLength(src Source, min, max int) Length {
	if min < 1 {
		min = 1
	}
	if max < min {
		max = min
	}

	span := max - min
	target := min
	if span > 0 {
		u := src.Float64()
		target = min + int(math.Floor(u*u*float64(span+1)))
		if target > max {
			target = max
		}
	}
	return Length{Min: min, Max: max, Target: target}
}

// endLogit is the adjustment applied to the end sentinel at a given sentence length.
//
// Three regimes, and the shape matters more than the numbers:
//
//   - Below Min, a hard penalty. Not infinite: a corpus where the only continuation is
//     the sentinel must still be able to end, and a -Inf here would make that step
//     produce nothing at all, which the caller would turn into a dead end anyway but
//     without the model having said so.
//   - Between Min and Target, a penalty easing linearly to zero. This is what makes
//     the target a preference rather than a second floor.
//   - Past Target, a growing bonus, so a sentence that has outstayed its welcome ends
//     on its own rather than only at the cap. Without this the cap does all the work
//     and every long sentence ends abruptly at exactly Max.
func (l Length) endLogit(w Weights, length int) float64 {
	switch {
	case length < l.Min:
		return w.EndEarly

	case length < l.Target:
		// Linear ease from EndEarly at Min to 0 at Target.
		span := float64(l.Target - l.Min)
		if span <= 0 {
			return 0
		}
		remaining := float64(l.Target-length) / span
		return w.EndEarly * remaining

	default:
		// Past the target. Grows with how far past, capped so it cannot swamp the
		// model outright: an end token that is certain regardless of context produces
		// sentences that all stop in the same grammatical place.
		over := float64(length - l.Target)
		bonus := w.EndLate * over
		if bonus > w.EndLateCap {
			bonus = w.EndLateCap
		}
		return bonus
	}
}

// Done reports whether the sentence must stop regardless of what the model wants.
func (l Length) Done(length int) bool { return length >= l.Max }
