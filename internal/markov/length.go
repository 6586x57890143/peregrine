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

// Progress is how far through the sentence we are, in [0, 1], and it is what Step.Position
// must be set from.
//
// AGAINST THE TARGET, NOT THE CAP, and that is the whole reason this is a method.
//
// The scorer compares Step.Position against corpus.TopicAssoc.MeanPosition, which the learn
// path records as float64(j)/float64(len(words)): a fraction of a real message, spread over
// the whole of [0, 1). Both walk loops used to divide by Max instead, and the two are not
// the same scale. The target is a squared-uniform draw, so at the shipped 4-to-18 bounds the
// median lands around 7 words: a typical reply therefore swept Position from 0 to about 0.39
// and stopped, never entering the upper half of the range the corpus measured against.
//
// Everything that compares the two inherited that. exp(-5d) is 0.37 at d=0.2 and 0.05 at
// d=0.6, so a word the corpus usually sees late in a message was damped to nothing for the
// ENTIRE sentence rather than only at the start, and the net effect was a standing
// preference for sentence-initial vocabulary at every step. Note the contrast with seed.go,
// which prefers early-position words deliberately because it is choosing where a sentence
// STARTS; the defect was the scorer inheriting the seed's bias for the whole walk.
//
// Connective was worse than damped. Its late-dampening branch needs Position above 0.85,
// which under the old divisor meant word 16 of 18 and therefore essentially never, while its
// mid-sentence nudge covered words 4 through 14. In practice the term was a flat bias toward
// "and/but/then/because" for the whole sentence, in an engine whose length model exists to
// argue the opposite.
//
// A method rather than a line in each caller because there were two walk loops, production
// and the golden harness, and they had the same wrong copy. A harness that computes progress
// differently from the bot reports on a bot that does not exist.
func (l Length) Progress(length int) float64 {
	target := l.Target
	if target < 1 {
		// Only reachable from a zero-value Length, which a caller that skipped NewLength
		// could hand us. Falling back to the cap beats dividing by zero.
		target = l.Max
	}
	if target < 1 {
		return 0
	}
	if p := float64(length) / float64(target); p < 1 {
		return p
	}
	// Past the target the sentence is in the end-bonus regime and there is no more
	// progress to report: the position terms should read "at the end", not "beyond it",
	// because MeanPosition cannot exceed 1 either.
	return 1
}
