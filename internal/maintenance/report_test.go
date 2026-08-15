package maintenance_test

import (
	"strings"
	"testing"

	"github.com/6586x57890143/peregrine/internal/corpus"
	"github.com/6586x57890143/peregrine/internal/maintenance"
)

// The rendering is the whole product of this mode: the numbers exist to be read by a
// person deciding what to do about a tuning constant. So these test what the operator
// can actually see, and in particular the two things a wrong report would hide.

func render(t *testing.T, st corpus.Stats, minAuthors int) string {
	t.Helper()
	var b strings.Builder
	if err := maintenance.CorpusReport(st, minAuthors, &b); err != nil {
		t.Fatalf("CorpusReport: %v", err)
	}
	return b.String()
}

func sampleStats() corpus.Stats {
	return corpus.Stats{
		Learned:             100,
		Edges:               10,
		Prefixes:            4,
		Vocabulary:          8,
		TotalTokens:         200,
		TotalEdgeMass:       40,
		SentinelCount:       100,
		Authors:             make([]int, corpus.AuthorHistogramMax+1),
		SingleAuthorByCount: make([]int, len(corpus.CountThresholds)),
		Admission: []corpus.Admission{
			{MinAuthors: 0, Edges: 10, EdgeShare: 1, Mass: 40, MassShare: 1},
			{MinAuthors: 1, Edges: 9, EdgeShare: 0.9, Mass: 38, MassShare: 0.95},
			{MinAuthors: 2, Edges: 2, EdgeShare: 0.2, Mass: 12, MassShare: 0.3},
		},
		Orders: []corpus.OrderStats{
			{Order: 1, Prefixes: 2, Edges: 6, MeanSuccessors: 3, MedianSucc: 3,
				MeanGatedSucc: 0.5, DeadPrefixRate: 0.5},
		},
		TopicCounts:     []uint64{1, 2, 3, 100},
		AssocCounts:     []uint64{1, 1, 2},
		AssocPerWord:    []int{1, 3},
		SuccessorCounts: []int{1, 1, 3},
	}
}

// Which value is running is the one piece of context the numbers do not carry, and an
// operator reading an admission curve without it cannot tell which row is their bot.
func TestTheReportMarksTheConfiguredThreshold(t *testing.T) {
	out := render(t, sampleStats(), 2)

	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, "k = 2") && !strings.Contains(line, "configured") {
			t.Errorf("the configured threshold is not marked: %q", line)
		}
		if strings.Contains(line, "k = 1") && strings.Contains(line, "configured") {
			t.Errorf("a threshold that is not configured is marked as one: %q", line)
		}
	}
}

// Both shares, because they are different claims and only the second predicts behaviour.
// A report that printed the edge share alone would understate what the gate costs by a
// factor of four on the real corpus, in the direction that makes it look harmless.
func TestTheReportPrintsBothAdmissionShares(t *testing.T) {
	out := render(t, sampleStats(), 2)

	if !strings.Contains(out, "20.00%") {
		t.Error("edge share missing from the admission curve")
	}
	if !strings.Contains(out, "30.00%") {
		t.Error("mass share missing from the admission curve")
	}
}

// Without this line the report reads as though something is double-counting messages,
// because the sentinel's count is exactly the number of messages learned and it is the
// maximum of every distribution below it.
func TestTheReportNamesTheSentinelAsNotAWord(t *testing.T) {
	out := render(t, sampleStats(), 2)

	if !strings.Contains(out, "end sentinel (not a word)") {
		t.Error("the sentinel is not called out, so its count reads as a word's")
	}
	if !strings.Contains(out, "50.0% of all tokens") {
		t.Errorf("the sentinel's share of all tokens is missing or wrong:\n%s", out)
	}
}

// The gated column is the half the gate decision rests on, and a mean at or near 1 is a
// deterministic walk however hot the sampler is.
func TestTheReportPrintsTheGatedBranchFactor(t *testing.T) {
	out := render(t, sampleStats(), 2)

	if !strings.Contains(out, "mean gated") {
		t.Error("the per-order table has no gated column")
	}
	if !strings.Contains(out, "50.00%") {
		t.Error("the dead-prefix rate is missing")
	}
}

// An empty corpus is what an operator gets by pointing the mode at the wrong file, and a
// panic there tells them nothing about which mistake they made.
func TestTheReportOnAnEmptyCorpusDoesNotPanic(t *testing.T) {
	st := corpus.Stats{
		Authors:             make([]int, corpus.AuthorHistogramMax+1),
		SingleAuthorByCount: make([]int, len(corpus.CountThresholds)),
	}

	out := render(t, st, 2)
	if !strings.Contains(out, "Scale") {
		t.Errorf("an empty corpus produced no report at all:\n%s", out)
	}
}
