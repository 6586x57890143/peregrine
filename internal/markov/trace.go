package markov

// Trace records what generation DID, as opposed to what it produced.
//
// # Why the engine grows an observer at all
//
// SPEC.md section 10 is six open decisions and five of them say the same thing: revisit
// against real ingested text. The blocker was never analysis, it was that the only
// instrument able to judge output is the golden harness and it runs against a synthetic
// fixture. A transcript of real replies does not close that gap on its own, because a
// sentence does not say where it started, how many times the author-diversity gate emptied
// the candidate set on the way, or whether the walk had to back off to a one-word context
// to find anything at all. Those are the numbers the open decisions are actually about.
//
// So this is the counterpart of the golden harness's printed samples: the same questions,
// asked of production, in a form that can be counted rather than read.
//
// # Where it lives, and why not on the Generator
//
// On the CALLER'S per-sentence state, never on the Generator. The Generator holds the
// corpus, the dials and the randomness, keeps no per-sentence state, and is therefore safe
// for every message goroutine to share without a lock. A counter hanging off it would be a
// data race on the bot's hottest path, which is SPEC.md finding 3 rebuilt on purpose: the
// fix there was to have no shared mutable state rather than to lock it.
//
// Step already owns exactly this kind of state (it counts Jumps for the same reason), so a
// Trace pointer on Step and on SeedInput is the shape that already exists.
//
// # Nil is the normal case and costs a nil check
//
// Every method here has a nil receiver guard, which is legal on a pointer receiver and is
// what keeps `if t != nil` out of the scoring loop. Production without the export
// configured allocates nothing and branches once per step.
type Trace struct {
	// SeedTier is the tier the sentence started from, by NAME rather than by the
	// unexported integer. The integer's values shift whenever a tier is added or removed
	// (two have been deleted as dead already, findings 34 and 37) and the export outlives
	// that.
	SeedTier string

	// SeedKey is the prefix the walk actually started on.
	SeedKey string

	// Steps is how many words the walk produced, including the end sentinel when it chose
	// one.
	Steps int

	// DeadEnds is how many times Next had nothing to offer. Starved is the subset of those
	// where candidates existed and the author-diversity gate removed all of them.
	//
	// The split is the point. A dead end on an unseen prefix is a sparse corpus; a starved
	// step is PEREGRINE_MIN_DISTINCT_AUTHORS ending sentences, which is the gate working
	// and is also the single most likely reason a deployed bot is quiet. Nothing has ever
	// been able to tell an operator which of the two they have.
	DeadEnds int
	Starved  int

	// GateRefused counts candidates the gate removed across the whole walk, including the
	// steps that survived it.
	GateRefused int

	// MinOrder is the shortest context, in words, that any step had to enumerate from.
	//
	// Section 10 records that backing off to a one-word context produces grammatically
	// disconnected joins, and that this is "probably a fixture-size artifact" because the
	// golden corpus is 150 messages. This is the number that settles it against real text
	// instead of leaving it a guess.
	MinOrder int

	// candidateTotal and candidateSteps are the running mean of the post-gate candidate set
	// size. Unexported because the useful form is the mean, which MeanCandidates returns: a
	// consumer reading a sum and a count would be a consumer that can compute the wrong
	// average.
	candidateTotal int
	candidateSteps int

	// Attempts is how many re-seeds the caller made, and ChoseEnd records whether the
	// sentence ended on the sentinel rather than running out of chain. Both are set by the
	// caller, which is the only place that knows them.
	Attempts int
	ChoseEnd bool

	// Jumps is copied from the Step by the caller, because Step.Jumps is already the
	// authority and a second counter incremented here could disagree with it.
	Jumps int
}

// seed records where the sentence started.
func (t *Trace) seed(tier seedTier, key string) {
	if t == nil {
		return
	}
	t.SeedTier = tier.String()
	t.SeedKey = key
}

// step records one call to Next that produced a token.
//
// order is the shortest context enumerated, eligible is the candidate count after the
// author-diversity gate, and refused is how many it removed.
func (t *Trace) step(order, eligible, refused int) {
	if t == nil {
		return
	}
	t.Steps++
	t.GateRefused += refused
	if eligible > 0 {
		t.candidateTotal += eligible
		t.candidateSteps++
	}
	if order > 0 && (t.MinOrder == 0 || order < t.MinOrder) {
		t.MinOrder = order
	}
}

// deadEnd records a step that produced nothing. starved distinguishes the case where the
// gate emptied a non-empty candidate set from the case where there were no candidates.
func (t *Trace) deadEnd(starved bool, refused int) {
	if t == nil {
		return
	}
	t.DeadEnds++
	t.GateRefused += refused
	if starved {
		t.Starved++
	}
}

// MeanCandidates is the average post-gate candidate set size, or 0 when nothing was
// enumerated.
//
// A mean of one is a deterministic walk however hot the sampler is, which is most of what
// made the previous engine feel canned, and it is invisible in the output because the
// output still varies at the seed.
func (t *Trace) MeanCandidates() float64 {
	if t == nil || t.candidateSteps == 0 {
		return 0
	}
	return float64(t.candidateTotal) / float64(t.candidateSteps)
}

// String names a seed tier for the export.
//
// Exhaustive by construction: numSeedTiers is the last constant in the iota block, so a
// tier added without a name here lands on the default and shows up in the data as
// "unknown" rather than as a silently mislabelled share.
func (t seedTier) String() string {
	switch t {
	case tierName:
		return "name"
	case tierPromptNgram:
		return "prompt-ngram"
	case tierNameTopic:
		return "name-topic"
	case tierTopicWord:
		return "topic-word"
	case tierTwoHop:
		return "two-hop"
	case tierRecent:
		return "recent"
	case numSeedTiers:
		return "unknown"
	}
	return "unknown"
}
