package corpus

import "slices"

// Stats is a whole-corpus measurement, for the -corpus-report maintenance mode.
//
// # Why this exists at all
//
// SPEC.md section 10 is six open decisions and five of them say the same sentence:
// revisit against real ingested text. The blocker was never analysis. It was that the
// only instrument able to judge the engine ran against a 150-line synthetic fixture,
// and finding 29's rule is that a tuning constant nobody validated against real data is
// a guess wearing a default's clothes.
//
// The first real measurement immediately corrected two estimates and made a third much
// worse, which is the whole argument for the mode: a mean word in a real corpus occurs
// on the order of ten times, not the several hundred that a scaling argument had
// assumed, so terms that looked saturated grade fine and the author-diversity gate
// turned out to be refusing most of the graph.
//
// # It is raw distributions and nothing else
//
// Nothing here evaluates a weight, and that is deliberate. Teaching a maintenance mode
// the scorer's formulas would couple it to markov's constants and give the two a way to
// disagree, which is finding 28's shape. This reports what the corpus contains; what
// that implies for a logit belongs in internal/markov, in a test built at these
// magnitudes.
type Stats struct {
	// Scale.
	Learned       uint64 // messages ever learned
	Edges         int    // ngram keys: distinct (prefix, next) pairs
	Prefixes      int    // distinct prefixes across all orders
	Vocabulary    int    // topic keys: distinct words
	TopicWords    int    // word-to-word co-occurrence pairs
	NameTopics    int    // name-to-word co-occurrence pairs
	Names         int
	TotalTokens   uint64 // sum of every topic count
	TotalEdgeMass uint64 // sum of every edge count

	// SentinelCount is the end sentinel's own topic count, reported separately because
	// it is not a word and it is the largest entry in the bucket by a wide margin.
	//
	// The learn path appends the sentinel before the topic loop runs, so its count is
	// exactly the number of messages learned. Every distribution below therefore has it
	// as the maximum, and a reader who does not know that reads "max 19387, messages
	// learned 19387" and starts looking for the double-count bug that is not there.
	//
	// It is also the number behind finding 53: the sentinel is a topic-bucket entry, so
	// the scorer's Significance term pays it the saturated maximum at every step.
	SentinelCount uint64

	// Authors is the distribution of DISTINCT AUTHORS per edge, indexed by author
	// count and capped at AuthorHistogramMax, where the last bucket is "that many or
	// more". Index 0 counts edges with no attributed author, which is what the bot's
	// own output produces: learnMessage passes an empty author for its own messages
	// so self-learning cannot bootstrap a phrase into eligibility.
	Authors []int

	// SingleAuthorByCount answers the question that sizes a concentration gate, and
	// it is the number that exists nowhere else in the system.
	//
	// The current gate refuses any edge below MinDistinctAuthors regardless of how
	// often it was seen, which conflates "rare" with "unattested by several people".
	// An edge one person said once carries negligible probability mass and is not an
	// attack; the poisoning shape SPEC.md section 4 A6 describes is an edge one person
	// said hundreds of times. Telling those apart needs the joint distribution of
	// (authors, count), not either margin.
	//
	// Each entry is how many SINGLE-AUTHOR edges have a count at or above its
	// threshold. Thresholds are in CountThresholds.
	SingleAuthorByCount []int

	// Admission is what share of the graph survives the gate at each candidate value
	// of MinDistinctAuthors, by edge and by probability mass.
	//
	// BOTH, because they are different claims and only the second predicts behaviour:
	// generation samples proportional to probability, so refusing 86% of edges and
	// refusing 86% of the mass are very different outcomes.
	Admission []Admission

	// Orders is the branch factor per prefix order, which is what decides how much
	// the backoff has to work. SPEC.md section 10 records low-order joins as
	// "probably a fixture-size artifact"; this is the number that settles it.
	Orders []OrderStats

	// Distributions. Sorted ascending so Percentile can index them directly.
	TopicCounts     []uint64 // per word: how often it was seen
	AssocCounts     []uint64 // per (word, associate) pair
	AssocPerWord    []int    // per word: how many associates it has
	SuccessorCounts []int    // per prefix: how many continuations, ungated
}

// AuthorHistogramMax is the largest author count tracked individually. Above it
// everything lands in the final bucket, because the decisions this informs are all
// about the boundary between one author and two.
const AuthorHistogramMax = 8

// CountThresholds are the occurrence counts SingleAuthorByCount is bucketed at.
//
// Geometric rather than linear, because the question is which order of magnitude a
// poisoning threshold belongs in, not whether it is 6 or 7.
var CountThresholds = []uint64{1, 2, 3, 5, 10, 20, 50, 100}

// Admission is the share of the corpus a given author threshold admits.
type Admission struct {
	MinAuthors int
	Edges      int
	EdgeShare  float64
	Mass       uint64
	MassShare  float64
}

// OrderStats is one prefix order's branch factor.
//
// Ungated against gated is the comparison the gate decision rests on: a mean of one
// is a deterministic walk however hot the sampler is, and that is invisible in the
// output because the output still varies at the seed.
type OrderStats struct {
	Order          int // prefix length in words
	Prefixes       int
	Edges          int
	MeanSuccessors float64
	MedianSucc     int

	// Gated is the same, counting only edges two or more distinct authors produced,
	// which is what generation actually gets to choose between today.
	GatedPrefixes  int // prefixes with at least one admissible continuation
	MeanGatedSucc  float64
	DeadPrefixRate float64 // share of prefixes with NO admissible continuation
}

// Percentile returns the q quantile of a sorted slice, q in [0, 1].
//
// Nearest-rank on an already-sorted slice. It lives here rather than in the report so
// that the storage walk and any future consumer cannot hold two different definitions
// of p90, which is the shape finding 17 records about StartOfWeekUTC having had three
// implementations.
//
// internal/tuning carries its own unexported copies of this and of Mean. Switching them
// over is worth doing and is deliberately not done in the same change as a new mode.
func Percentile[T int | uint64 | float64](sorted []T, q float64) T {
	var zero T
	if len(sorted) == 0 {
		return zero
	}
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	i := int(q * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// Mean of a slice, or zero when it is empty.
func Mean[T int | uint64 | float64](xs []T) float64 {
	if len(xs) == 0 {
		return 0
	}
	var total float64
	for _, x := range xs {
		total += float64(x)
	}
	return total / float64(len(xs))
}

// SortUint64 and SortInt put a distribution in the order Percentile requires.
func SortUint64(xs []uint64) { slices.Sort(xs) }
func SortInt(xs []int)       { slices.Sort(xs) }
