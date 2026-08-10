package tuning

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// The analysis half.
//
// A pile of JSONL is an archive rather than an instrument. What makes the loop repeatable is
// that the same questions get asked of every archive in the same way, so a number from one
// version can be put next to a number from the next and the difference attributed to the
// change between them. Reading a transcript by eye cannot do that, which is exactly the gap
// SPEC.md section 10 keeps recording: five open decisions all waiting on "revisit against
// real ingested text".
//
// The questions here are section 5.5's, asked of production instead of the fixture, plus the
// two things the fixture cannot answer at all: whether anybody reacted, and whether the
// author-diversity gate is ending sentences.

// Report reads every export file under path and writes a summary to w.
//
// path may be a directory or a single file, because both are things an operator ends up
// holding: a whole pulled-down archive, or one file somebody attached to a message.
//
// It opens no corpus and makes no network call, which is the same rule the other maintenance
// modes follow. Reading an archive on a laptop must not need a database, a token, or bbolt's
// exclusive flock.
func Report(path string, w io.Writer) error {
	files, err := reportFiles(path)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("tuning: no %s*%s files under %s", prefix, suffix, path)
	}

	var agg aggregate
	agg.init()
	for _, f := range files {
		if err := agg.readFile(f); err != nil {
			return err
		}
	}
	agg.write(w, files)
	return nil
}

// reportFiles resolves a path to the export files under it.
func reportFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("tuning: %w", err)
	}
	if !info.IsDir() {
		return []string{path}, nil
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("tuning: %w", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		out = append(out, filepath.Join(path, name))
	}
	// Names are fixed-width UTC, so lexical order is chronological order and the last
	// snapshot read is genuinely the most recent one.
	sort.Strings(out)
	return out, nil
}

// aggregate is everything the report counts, accumulated across files.
type aggregate struct {
	samples     int
	engagements int
	snapshots   int
	skipped     int

	byOutcome map[string]int
	byTrigger map[string]int
	bySeed    map[string]int
	// seedLanded is how many sent replies from each tier got an engagement record, and
	// engagedSeed is how many of those drew anything. Their ratio is the single most useful
	// number in the file: it is the only thing in the archive that connects a tuning
	// decision to a human reaction.
	seedLanded map[string]int

	replyWords []int
	tookMS     []int64

	sent      int
	roast     int
	roastSent int

	attemptsOver1 int
	jumped        int
	totalJumps    int
	deadEnds      int
	starved       int
	minOrders     []int
	candidates    []float64

	reacted     int
	repliedTo   int
	windows     []int
	engagedSeed map[string]int

	// versions and dials track whether the archive is homogeneous. A report averaged over
	// two different sets of weights is a number that describes neither, and this is the only
	// place that can notice.
	versions    map[string]int
	lastCorpus  *Corpus
	lastParams  map[string]float64
	lastWeight  map[string]float64
	paramsMoved bool

	maxExportDropped uint64
	maxQueueDropped  uint64
	maxEmitRejected  uint64

	// seedOf remembers which tier produced each sent message, so an Engagement record read
	// later can be attributed to it. Bounded by the archive rather than by a running
	// process, which is fine: this is an offline pass over a file of known size.
	seedOf map[string]string
}

func (a *aggregate) init() {
	a.byOutcome = map[string]int{}
	a.byTrigger = map[string]int{}
	a.bySeed = map[string]int{}
	a.seedLanded = map[string]int{}
	a.engagedSeed = map[string]int{}
	a.versions = map[string]int{}
	a.seedOf = map[string]string{}
}

// readFile folds one export file into the aggregate.
//
// A line that will not decode is COUNTED AND SKIPPED rather than failing the run. The last
// line of a file copied while the bot was running is routinely half-written, and refusing to
// report on an otherwise good archive because of it would make the tool useless in the one
// situation an operator reaches for it. The count is printed, so a file that is mostly
// unreadable does not look like a small one.
func (a *aggregate) readFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // an operator-supplied archive path, which is the point
	if err != nil {
		return fmt.Errorf("tuning: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Records carry message text, so a line can be long. The default 64 KiB token limit
	// would silently truncate rather than error, which is the failure mode this repository
	// keeps designing against.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var envelope struct {
			Kind Kind `json:"kind"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			a.skipped++
			continue
		}
		switch envelope.Kind {
		case KindSample:
			var rec Sample
			if err := json.Unmarshal(line, &rec); err != nil {
				a.skipped++
				continue
			}
			a.addSample(rec)
		case KindEngagement:
			var rec Engagement
			if err := json.Unmarshal(line, &rec); err != nil {
				a.skipped++
				continue
			}
			a.addEngagement(rec)
		case KindSnapshot:
			var rec Snapshot
			if err := json.Unmarshal(line, &rec); err != nil {
				a.skipped++
				continue
			}
			a.addSnapshot(rec)
		default:
			a.skipped++
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("tuning: reading %s: %w", path, err)
	}
	return nil
}

func (a *aggregate) addSample(rec Sample) {
	a.samples++
	a.byOutcome[rec.Outcome]++
	a.byTrigger[rec.Trigger]++
	if rec.Version != "" {
		a.versions[rec.Version]++
	}
	if rec.Roast {
		a.roast++
	}
	if rec.TookMS > 0 {
		a.tookMS = append(a.tookMS, rec.TookMS)
	}
	if rec.Sent {
		a.sent++
		a.replyWords = append(a.replyWords, rec.Words)
		if rec.Roast {
			a.roastSent++
		}
	}

	tr := rec.Trace
	if tr == nil {
		return
	}
	tier := tr.SeedTier
	if tier == "" {
		tier = "(none)"
	}
	a.bySeed[tier]++
	if rec.Sent && rec.ID != "" {
		a.seedOf[rec.ID] = tier
	}
	if tr.Attempts > 1 {
		a.attemptsOver1++
	}
	if tr.Jumps > 0 {
		a.jumped++
		a.totalJumps += tr.Jumps
	}
	a.deadEnds += tr.DeadEnds
	a.starved += tr.Starved
	if tr.MinOrder > 0 {
		a.minOrders = append(a.minOrders, tr.MinOrder)
	}
	if tr.CandidatesX100 > 0 {
		a.candidates = append(a.candidates, float64(tr.CandidatesX100)/100)
	}
}

func (a *aggregate) addEngagement(rec Engagement) {
	a.engagements++
	a.windows = append(a.windows, rec.WindowS)

	landed := rec.Reactions > 0 || rec.Replied
	if rec.Reactions > 0 {
		a.reacted++
	}
	if rec.Replied {
		a.repliedTo++
	}
	if tier, ok := a.seedOf[rec.ID]; ok {
		a.seedLanded[tier]++
		if landed {
			a.engagedSeed[tier]++
		}
	}
}

func (a *aggregate) addSnapshot(rec Snapshot) {
	a.snapshots++
	if rec.Version != "" {
		a.versions[rec.Version]++
	}
	if a.lastParams != nil && (!sameDials(a.lastParams, rec.Params) || !sameDials(a.lastWeight, rec.Weights)) {
		a.paramsMoved = true
	}
	corpus := rec.Corpus
	a.lastCorpus = &corpus
	a.lastParams, a.lastWeight = rec.Params, rec.Weights

	a.maxExportDropped = maxU64(a.maxExportDropped, rec.Counters.ExportDropped)
	a.maxQueueDropped = maxU64(a.maxQueueDropped, rec.Counters.QueueDropped)
	a.maxEmitRejected = maxU64(a.maxEmitRejected, rec.Counters.EmitRejected)
}

// write renders the report.
//
// Plain text on purpose. It is read in a terminal after an scp, pasted into a message, and
// diffed against the last one, and every one of those is worse with a table format that
// needs a tool to read.
func (a *aggregate) write(w io.Writer, files []string) {
	// The error is deliberately discarded. This writes to stdout for an operator reading a
	// terminal, and there is nothing useful to do about a failed write to it: the only
	// plausible cause is a closed pipe, which means the reader has already stopped reading.
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }

	p("tuning report over %d file(s), %d samples, %d engagements, %d snapshots",
		len(files), a.samples, a.engagements, a.snapshots)
	if a.skipped > 0 {
		p("  %d line(s) could not be decoded and were skipped", a.skipped)
	}
	if len(a.versions) > 1 {
		p("  WARNING: %d versions in this archive (%s). Split it before attributing a "+
			"difference to a change.", len(a.versions), strings.Join(sortedKeys(a.versions), ", "))
	}
	if a.paramsMoved {
		p("  WARNING: the dials changed partway through this archive, so every average below " +
			"describes two configurations at once.")
	}
	if a.maxQueueDropped > 0 {
		p("  the work queue dropped %d message(s) over this archive, so some conversation "+
			"never reached the bot at all", a.maxQueueDropped)
	}
	if a.maxEmitRejected > 0 {
		p("  the emit gate refused %d message(s) over this archive; none of their text is in "+
			"here, by design", a.maxEmitRejected)
	}
	if a.maxExportDropped > 0 {
		p("  WARNING: the export dropped %d record(s), so this is a biased sample: it is "+
			"missing whatever was happening when the queue was full.", a.maxExportDropped)
	}
	if a.samples == 0 {
		return
	}

	p("")
	p("OUTCOMES")
	for _, k := range sortedKeys(a.byOutcome) {
		p("  %-14s %5d  %5.1f%%", k, a.byOutcome[k], pct(a.byOutcome[k], a.samples))
	}
	p("  %-14s %5d  %5.1f%% of all attempts", "sent", a.sent, pct(a.sent, a.samples))
	// The number SPEC.md section 10 is really about. On a young corpus this is the
	// author-diversity gate doing its job, and the operator's fix is more people rather than
	// a lower threshold; on a mature one it is a tuning problem.
	if tooShort := a.byOutcome["too-short"]; tooShort > 0 {
		p("  a too-short rate this high usually means PEREGRINE_MIN_DISTINCT_AUTHORS is " +
			"refusing continuations only one person has said")
	}
	p("")
	p("TRIGGERS")
	for _, k := range sortedKeys(a.byTrigger) {
		p("  %-14s %5d", k, a.byTrigger[k])
	}
	if a.roast > 0 {
		p("  %-14s %5d attempts, %d sent", "roast persona", a.roast, a.roastSent)
	}

	p("")
	p("LENGTH (section 5.5 criterion 1: the median wants to be 4 to 12 words)")
	if len(a.replyWords) > 0 {
		sort.Ints(a.replyWords)
		p("  median %d words, p10 %d, p90 %d, max %d",
			percentileInt(a.replyWords, 0.5), percentileInt(a.replyWords, 0.1),
			percentileInt(a.replyWords, 0.9), a.replyWords[len(a.replyWords)-1])
		inBand := 0
		for _, n := range a.replyWords {
			if n >= 4 && n <= 12 {
				inBand++
			}
		}
		p("  %5.1f%% of sent replies fall in the 4 to 12 word band", pct(inBand, len(a.replyWords)))
	}
	if len(a.tookMS) > 0 {
		slices.Sort(a.tookMS)
		p("  generation took a median of %d ms, p90 %d ms",
			percentileInt64(a.tookMS, 0.5), percentileInt64(a.tookMS, 0.9))
	}

	p("")
	p("SEED TIERS (a tier at 0%% is a dead tier, which this repo has shipped twice)")
	for _, k := range sortedKeys(a.bySeed) {
		p("  %-14s %5d  %5.1f%%", k, a.bySeed[k], pct(a.bySeed[k], a.samples))
	}

	p("")
	p("THE WALK")
	p("  %5.1f%% of attempts needed a re-seed", pct(a.attemptsOver1, a.samples))
	p("  %5.1f%% of attempts took a jump (%d in total; a jump has no n-gram relationship "+
		"to the word before it, so the seam is rough by construction)",
		pct(a.jumped, a.samples), a.totalJumps)
	p("  %.2f dead ends per attempt, of which %.2f were the author-diversity gate emptying "+
		"the set", perAttempt(a.deadEnds, a.samples), perAttempt(a.starved, a.samples))
	if len(a.minOrders) > 0 {
		sort.Ints(a.minOrders)
		p("  the backoff reached a median context of %d word(s), p10 %d",
			percentileInt(a.minOrders, 0.5), percentileInt(a.minOrders, 0.1))
	}
	if len(a.candidates) > 0 {
		p("  mean post-gate candidate set: %.1f (a set of 1 is a deterministic step however "+
			"hot the sampler is)", mean(a.candidates))
	}

	p("")
	p("ENGAGEMENT")
	if a.engagements == 0 {
		p("  no engagement records yet; they resolve one window after each reply")
	} else {
		p("  %5.1f%% of watched replies drew a reaction", pct(a.reacted, a.engagements))
		p("  %5.1f%% drew a human reply", pct(a.repliedTo, a.engagements))
		if len(a.windows) > 0 {
			sort.Ints(a.windows)
			p("  median observation window %d s (a short one is a restart, not a result)",
				percentileInt(a.windows, 0.5))
		}
		if len(a.seedLanded) > 0 {
			p("  by seed tier, share that drew a reaction or a reply:")
			for _, k := range sortedKeys(a.seedLanded) {
				p("    %-14s %5.1f%%  (%d resolved)", k,
					pct(a.engagedSeed[k], a.seedLanded[k]), a.seedLanded[k])
			}
		}
	}

	// The corpus, because most of the numbers above are only interpretable against it. A
	// too-short rate of a third means one thing at 400k n-grams and something else entirely
	// in a corpus's first hours, where the author-diversity gate refusing nearly everything
	// is the control working rather than a fault.
	if a.lastCorpus != nil {
		p("")
		p("CORPUS (at the most recent snapshot)")
		p("  %d n-grams, %d author entries, %d words, %d names, %d messages learned",
			a.lastCorpus.Ngrams, a.lastCorpus.AuthorEntries, a.lastCorpus.Topics,
			a.lastCorpus.Names, a.lastCorpus.Learned)
	}

	if len(a.lastWeight) > 0 || len(a.lastParams) > 0 {
		p("")
		p("DIALS IN FORCE (from the most recent snapshot)")
		for _, k := range sortedKeys(a.lastParams) {
			p("  param  %-22s %g", k, a.lastParams[k])
		}
		for _, k := range sortedKeys(a.lastWeight) {
			p("  weight %-22s %g", k, a.lastWeight[k])
		}
	}
}

func sameDials(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		other, ok := b[k]
		if !ok || math.Abs(v-other) > 1e-9 {
			return false
		}
	}
	return true
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}

func perAttempt(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total)
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// percentileInt reads a percentile off an already sorted slice, NEAREST RANK rather than
// interpolated.
//
// The convention is pinned by a test rather than left to taste, because these numbers exist
// to be compared against the previous archive's: a silent switch between conventions would
// move every percentile in the report and look exactly like the bot's output changing.
func percentileInt(sorted []int, q float64) int {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[percentileIndex(len(sorted), q)]
}

func percentileInt64(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[percentileIndex(len(sorted), q)]
}

func percentileIndex(n int, q float64) int {
	i := int(math.Round(q * float64(n-1)))
	if i < 0 {
		return 0
	}
	if i >= n {
		return n - 1
	}
	return i
}

func maxU64(a, b uint64) uint64 {
	if b > a {
		return b
	}
	return a
}
