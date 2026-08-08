// Package corpus holds the value types that cross the storage boundary, and
// nothing else.
//
// It is a leaf with ZERO imports from this module, and that is the constraint that
// makes the rest of the layout possible. internal/storage returns these types and
// internal/markov consumes them, so the engine can be tested against Go maps with
// no database at all: markov never imports storage, storage never imports markov,
// and both agree on the shapes here.
//
// No methods that do I/O, no JSON tags that imply a wire format, no behaviour. If
// something here needs a database, it belongs in storage; if it needs a decision,
// it belongs in markov.
package corpus

import "time"

// Successor is one continuation of an n-gram prefix, with everything generation
// needs to score it.
type Successor struct {
	// Token is the word that follows the prefix.
	Token string

	// Count is how many times this continuation was seen. Raw frequency, which is
	// exactly why Authors exists.
	Count uint64

	// Authors is how many DISTINCT authors produced this continuation.
	//
	// This is the anti-poisoning control, and it is a count of people rather than
	// occurrences on purpose. n-gram weight is raw frequency, so repeating a phrase
	// is a direct write to the model and one determined user can teach the bot to
	// say anything (SPEC.md section 4, A6). Requiring k distinct authors before a
	// continuation is eligible to be GENERATED turns that attack from persistence
	// into collusion.
	//
	// The bot's own output is excluded from these counts, so self-learning cannot
	// bootstrap a phrase into eligibility.
	Authors uint32
}

// TopicAssoc is a co-occurrence count plus the sum of relative positions, which
// together give a mean position without storing every observation.
type TopicAssoc struct {
	Count  uint64
	PosSum float64
}

// MeanPosition returns the average relative position, or zero if unseen. Relative
// position matters because a word that always appears at the start of a sentence
// behaves differently from one that always ends it.
func (t TopicAssoc) MeanPosition() float64 {
	if t.Count == 0 {
		return 0
	}
	return t.PosSum / float64(t.Count)
}

// Name is a learned display name and the canonical name it resolves to.
//
// Aliases point at a canonical name rather than being merged, so "dave", "davey"
// and a Discord nickname can all resolve to one person without losing which form
// was actually used.
type Name struct {
	Count         uint64
	DiscordUserID string
	Canonical     string
}

// WeeklyStat is a per-user message counter for the chat leaderboard.
type WeeklyStat struct {
	Count         uint64
	LastTimestamp time.Time
}

// WordPos carries the positional and association data generation uses to bias
// candidate selection.
//
// Named for what it is rather than what it was: this was WordPosData, a struct with
// eight fields of which several were never read. What survives is what the scorer
// actually consumes.
type WordPos struct {
	Word       string
	Count      uint64
	PosSum     float64
	IsName     bool
	Associated []string
}

// MeanPosition mirrors TopicAssoc.MeanPosition.
func (w WordPos) MeanPosition() float64 {
	if w.Count == 0 {
		return 0
	}
	return w.PosSum / float64(w.Count)
}

// KNStats are the two counts interpolated Kneser-Ney needs beyond raw frequency,
// and both are counts of DISTINCT things rather than occurrences.
//
// The whole reason they are stored rather than derived is cost. Counting distinct
// successors with a cursor is O(successors) on every single lookup, on the hot path
// of every generated token, so they are maintained incrementally instead. That is
// the honest price of KN: roughly one extra read and two extra writes per n-gram
// per message (SPEC.md section 3.2).
type KNStats struct {
	// DistinctSuccessors is N1+(prefix .), the number of distinct tokens observed
	// following this prefix. It is the lambda normalizer: how much probability mass
	// to hand to the lower-order model.
	DistinctSuccessors uint64

	// DistinctPredecessors is N1+(. token), the number of distinct contexts this
	// token was observed following. This is KN's central quantity, and the one that
	// this bot deliberately does not apply at full strength.
	//
	// The textbook use is to demote a token that is frequent but appears in few
	// contexts, "Francisco" being the canonical example: common, but almost always
	// preceded by "San", so raw frequency overrates it as a fallback. The problem
	// is that a meme, a copypasta and an inside joke are statistically
	// indistinguishable from "Francisco". Pure KN would systematically suppress
	// exactly the register this server runs on, which is why PEREGRINE_KN_RAW_MIX
	// interpolates back toward raw counts (SPEC.md section 5.2).
	DistinctPredecessors uint64
}
