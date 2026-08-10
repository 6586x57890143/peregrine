// Package tuning is the wire format and the file writer for the tuning export.
//
// # Why this exists
//
// SPEC.md section 10 has six open decisions and five of them say the same sentence:
// revisit against real ingested text. `mu` and `D` are "starting guesses",
// DefaultWeights is "a considered first guess, not a measurement", PEREGRINE_TEMPERATURE
// "may want to be higher", PEREGRINE_MIN_DISTINCT_AUTHORS "needs a real value", and the
// low-order backoff joins are "probably a fixture-size artifact". Every one of those is
// blocked on the same missing thing: the only instrument that can judge output is the
// golden harness, and it runs against a 150-line synthetic fixture.
//
// Finding 29 is what happens without this. Clustering shipped a MergeThreshold eighteen
// times too small for its own data and nobody could tell, because the constant had never
// been checked against a real corpus. The rule that came out of it is that a tuning
// constant nobody validated is not a default, it is a guess wearing one.
//
// So this package is the other half of the golden harness: the same questions, asked of
// real output from a real corpus, written somewhere an operator can pull down between
// versions.
//
// # This package is a leaf and imports only the standard library
//
// That is not tidiness, and the reason is specific to what this file IS. These structs are
// a WIRE FORMAT that has to stay comparable across versions: a record written by v1 and a
// record written by v2 are read side by side, and that is the entire point of exporting
// them. If Sample embedded markov.Trace or storage.Status directly, then an engine
// refactor or a new bucket would silently change the shape of an archive nobody can
// regenerate, because the corpus it described has moved on.
//
// So the flat types here are DELIBERATE DUPLICATES of the engine's, and the plugin does
// the mapping. Adding a field is additive and safe; renaming one is a decision about an
// archive rather than a rename.
//
// # What is deliberately not written
//
// Content refused by the emit gate. internal/safety never records the offending text
// anywhere, on purpose, and a tuning file must not become the exception: a Sample whose
// send was refused carries Sent=false and an empty Reply.
package tuning

import "time"

// Kind names a record type. Every line in the export is one JSON object carrying one of
// these, so a reader can dispatch without guessing from the field set.
type Kind string

const (
	// KindSample is one generation attempt, including the ones that produced nothing.
	KindSample Kind = "sample"

	// KindEngagement resolves a Sample once its observation window has closed.
	KindEngagement Kind = "engagement"

	// KindSnapshot is the periodic state of the bot: corpus, counters, and the dials in
	// force.
	KindSnapshot Kind = "snapshot"
)

// Record is anything writable to the export.
//
// The unexported method is what makes "every record carries a kind" a compile-time fact
// rather than a convention. A caller cannot hand Write an anonymous struct that would
// land in the file with no way to identify it, which matters more here than usual: these
// files are read months later by something that was not written yet.
type Record interface {
	recordKind() Kind
}

// Sample is one generation attempt.
//
// SILENCE IS RECORDED TOO, and it is the most valuable thing in the file. Outcome
// "too-short" means every re-seed dead-ended, which on a real corpus is the clearest
// available signal about whether PEREGRINE_MIN_DISTINCT_AUTHORS and the length floor are
// set right. Today that decision is logged and then forgotten (finding 32 made it visible
// to an operator reading logs; this makes it countable).
type Sample struct {
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at"`

	// ID is the message the bot sent, which is what an Engagement record is keyed by.
	// Empty when nothing was sent, which is why Engagement is a separate record rather
	// than a field: most Samples never get one.
	ID string `json:"id,omitempty"`

	// Version is PEREGRINE_VERSION, and Generation is learn.Generation as stamped in the
	// corpus. Both are here rather than only in Snapshot because a file can be truncated,
	// concatenated or partially copied, and a Sample that cannot say which build produced
	// it is not comparable to anything.
	Version    string `json:"version"`
	Generation int    `json:"generation,omitempty"`

	// Trigger is what asked for a sentence: "reply" or "autopost". They are different
	// questions (one is answering somebody, one is speaking into a room) and lumping them
	// would average two distributions that should never have been averaged.
	Trigger string `json:"trigger"`
	Channel string `json:"channel"`

	Prompt      string   `json:"prompt,omitempty"`
	PromptWords int      `json:"prompt_words"`
	HasContext  bool     `json:"has_context"`
	Names       []string `json:"names,omitempty"`
	Roast       bool     `json:"roast"`

	// Reply is omitted when Sent is false. See the package comment: a refused emission
	// leaves no text anywhere, and that rule does not get an exception for telemetry.
	Reply   string `json:"reply,omitempty"`
	Words   int    `json:"words"`
	Outcome string `json:"outcome"`
	Sent    bool   `json:"sent"`

	TookMS int64  `json:"took_ms"`
	Trace  *Trace `json:"trace,omitempty"`
}

func (Sample) recordKind() Kind { return KindSample }

// Trace is what generation actually did, flattened.
//
// This is the field set that makes the export actionable rather than a transcript. A
// transcript says the bot produced a sentence; this says the seed came from the two-hop
// tier, the walk took one jump, and the author-diversity gate removed nine candidates
// along the way. Correlated against Engagement it is how a weight gets moved for a reason.
type Trace struct {
	// SeedTier is the tier that won, by name. The engine's seedTier is an unexported int
	// whose values shift whenever a tier is added or removed, and this file outlives that.
	SeedTier string `json:"seed_tier,omitempty"`
	SeedKey  string `json:"seed_key,omitempty"`

	// Attempts is how many re-seeds it took. More than one means the first seed dead-ended
	// below the word floor.
	Attempts int `json:"attempts,omitempty"`

	Steps    int `json:"steps,omitempty"`
	Jumps    int `json:"jumps,omitempty"`
	DeadEnds int `json:"dead_ends,omitempty"`

	// GateRefused is how many candidates the author-diversity filter removed across the
	// whole walk, and Starved is how many steps it emptied entirely. The second is the one
	// that matters: a filter that trims is working, a filter that empties the set is
	// ending sentences.
	GateRefused int `json:"gate_refused,omitempty"`
	Starved     int `json:"starved,omitempty"`

	// MinOrder is the shortest context the walk had to back off to. Section 10 records
	// that low-order joins read as nonsense and are probably a fixture-size artifact; this
	// is the number that settles it against real text.
	MinOrder int `json:"min_order,omitempty"`

	// Candidates is the mean size of the candidate set after the gate, times 100 so the
	// wire format carries no float. A set of one is a deterministic step however hot the
	// sampler is, which is most of what made the old engine feel canned.
	CandidatesX100 int `json:"candidates_x100,omitempty"`

	// ChoseEnd distinguishes a sentence that ended because the model chose the sentinel
	// from one that ran out of chain. TrimDangling already treats those differently and
	// nothing has ever counted them.
	ChoseEnd bool `json:"chose_end,omitempty"`
}

// Engagement resolves a Sample once its window has closed.
//
// Separate from Sample rather than a field on it because the answer arrives minutes later
// and the file is append-only: rewriting a line to fill it in would mean holding samples
// in memory until they resolve, which is a map keyed by message ID with no bound. This
// repository has shipped that leak twice.
type Engagement struct {
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at"`

	// ID matches a Sample's ID.
	ID      string `json:"id"`
	Channel string `json:"channel,omitempty"`

	Reactions        int `json:"reactions"`
	DistinctReactors int `json:"distinct_reactors"`

	// Replied is a human replying to the bot's message, which is the strongest available
	// signal that a reply landed. Followups is traffic in the channel during the window,
	// which is the weakest: it counts a conversation the bot may have had nothing to do
	// with, and it is here as a denominator rather than as a result.
	Replied   bool `json:"replied"`
	Followups int  `json:"followups"`

	WindowS int `json:"window_s"`
}

func (Engagement) recordKind() Kind { return KindEngagement }

// Snapshot is the periodic state of the bot.
//
// The dials are in here, in full, and that is the field that makes a version-to-version
// diff attributable. Reading two archives and finding the output got better is worth
// nothing if the weights that produced them are not in the same file.
type Snapshot struct {
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at"`

	Version    string `json:"version"`
	Generation int    `json:"generation,omitempty"`
	UptimeS    int64  `json:"uptime_s"`

	Corpus   Corpus             `json:"corpus"`
	Counters Counters           `json:"counters"`
	Params   map[string]float64 `json:"params"`
	Weights  map[string]float64 `json:"weights"`
	Usage    map[string]uint64  `json:"usage,omitempty"`
}

func (Snapshot) recordKind() Kind { return KindSnapshot }

// Corpus mirrors storage.Status. See the package comment for why it is a copy.
type Corpus struct {
	Ngrams        int    `json:"ngrams"`
	AuthorEntries int    `json:"author_entries"`
	Topics        int    `json:"topics"`
	TopicWords    int    `json:"topic_words"`
	NameTopics    int    `json:"name_topics"`
	Names         int    `json:"names"`
	HistoryWindow uint64 `json:"history_window"`
	ImageCache    uint64 `json:"image_cache"`
	Learned       uint64 `json:"learned"`
}

// Counters are the accounting that internal/plugins/health reports, plus this package's
// own drop count.
//
// Dropped is here for the reason health's four counters exist at all: the recorder drops
// rather than blocking the reply path, that decision is correct, and it is invisible. A
// tuning file assembled from a queue that was full half the time is a biased sample, and
// nothing else in the file would say so.
type Counters struct {
	QueueDropped  uint64 `json:"queue_dropped"`
	LearnRejected uint64 `json:"learn_rejected"`
	EmitRejected  uint64 `json:"emit_rejected"`
	ExportDropped uint64 `json:"export_dropped"`
	Paused        bool   `json:"paused"`
}
