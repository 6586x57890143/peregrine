package tuning

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The report has to read what the writer wrote, through the real file, with no shared
// in-memory shortcut. That round trip is the actual contract: the whole point of the export
// is that an archive is pulled off a host and read somewhere else, by a build that may not be
// the one that produced it.
func TestTheReportReadsWhatTheWriterWrote(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Now: clock.now})

	// Two sent replies from different tiers, one silence, and engagement for one of them.
	samples := []Sample{
		{
			Kind: KindSample, At: clock.now(), ID: "m1", Version: "v1", Trigger: "reply",
			Reply: "greg is cooked honestly", Words: 4, Outcome: "produced", Sent: true, TookMS: 30,
			Trace: &Trace{SeedTier: "name", Steps: 4, MinOrder: 2, CandidatesX100: 350},
		},
		{
			Kind: KindSample, At: clock.now(), ID: "m2", Version: "v1", Trigger: "autopost",
			Reply: "the queue is doomed", Words: 4, Outcome: "produced", Sent: true, TookMS: 50,
			Trace: &Trace{SeedTier: "two-hop", Steps: 4, Jumps: 1, MinOrder: 1, Starved: 2, DeadEnds: 3},
		},
		{
			Kind: KindSample, At: clock.now(), Version: "v1", Trigger: "reply",
			Outcome: "too-short", Sent: false,
			Trace: &Trace{SeedTier: "recent", Attempts: 3, Starved: 4, DeadEnds: 4},
		},
	}
	for _, s := range samples {
		if err := w.Write(s); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Write(Engagement{
		Kind: KindEngagement, At: clock.now(), ID: "m1", Reactions: 2,
		DistinctReactors: 2, Replied: true, WindowS: 600,
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Write(Snapshot{
		Kind: KindSnapshot, At: clock.now(), Version: "v1",
		Params:   map[string]float64{"temperature": 1.6},
		Weights:  map[string]float64{"prompt_name": 0.9},
		Counters: Counters{EmitRejected: 4},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var out strings.Builder
	if err := Report(dir, &out); err != nil {
		t.Fatalf("Report: %v", err)
	}
	got := out.String()
	t.Log("\n" + got)

	for _, want := range []string{
		"3 samples",
		"1 engagements",
		"too-short",
		"name",
		"two-hop",
		// The dials, which are the reason a snapshot exists at all.
		"temperature",
		"prompt_name",
		// The engagement rate per seed tier, which is the number the whole export is for.
		"by seed tier",
		// The gate refusing output is reported without any of the text it refused.
		"emit gate refused 4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not mention %q", want)
		}
	}
}

// An archive spanning two versions or two sets of dials produces averages that describe
// neither. The report has to say so rather than printing a confident number, because the
// operator pulling two months of files off a host has no other way to notice.
func TestTheReportWarnsWhenTheArchiveIsNotHomogeneous(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Now: clock.now})

	write := func(rec Record) {
		t.Helper()
		if err := w.Write(rec); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	write(Sample{Kind: KindSample, At: clock.now(), Version: "v1", Outcome: "produced", Sent: true})
	write(Snapshot{
		Kind: KindSnapshot, At: clock.now(), Version: "v1",
		Params: map[string]float64{"temperature": 1.0}, Weights: map[string]float64{"prompt_name": 0.9},
	})
	write(Sample{Kind: KindSample, At: clock.now(), Version: "v2", Outcome: "produced", Sent: true})
	write(Snapshot{
		Kind: KindSnapshot, At: clock.now(), Version: "v2",
		Params: map[string]float64{"temperature": 1.6}, Weights: map[string]float64{"prompt_name": 0.9},
	})
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var out strings.Builder
	if err := Report(dir, &out); err != nil {
		t.Fatalf("Report: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "2 versions in this archive") {
		t.Errorf("no warning about two versions:\n%s", got)
	}
	if !strings.Contains(got, "dials changed partway") {
		t.Errorf("no warning about the dials moving:\n%s", got)
	}
}

// A file copied off a running host routinely ends mid-line. Refusing to report on an
// otherwise good archive because of that would make the tool useless in exactly the situation
// somebody reaches for it, so a bad line is counted and skipped.
func TestAHalfWrittenLastLineIsSkippedRatherThanFatal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, prefix+"20260301-120000"+suffix)

	content := `{"kind":"sample","outcome":"produced","sent":true,"words":5}
{"kind":"sample","outcome":"produced","sent":true,"wor`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var out strings.Builder
	if err := Report(dir, &out); err != nil {
		t.Fatalf("Report on a truncated file: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "1 samples") {
		t.Errorf("the whole line was not counted:\n%s", got)
	}
	if !strings.Contains(got, "could not be decoded") {
		t.Errorf("the truncated line was skipped silently, so a mostly unreadable file would "+
			"look like a small one:\n%s", got)
	}
}

// A single file is as valid a target as a directory, because both are things somebody ends
// up holding.
func TestReportAcceptsASingleFileAndRefusesAnEmptyDirectory(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Now: clock.now})
	if err := w.Write(Sample{Kind: KindSample, At: clock.now(), Outcome: "produced", Sent: true}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var out strings.Builder
	if err := Report(filepath.Join(dir, w.Name()), &out); err != nil {
		t.Fatalf("Report on one file: %v", err)
	}
	if !strings.Contains(out.String(), "1 samples") {
		t.Errorf("reporting on one file found nothing:\n%s", out.String())
	}

	if err := Report(t.TempDir(), &out); err == nil {
		t.Error("Report on an empty directory succeeded; it must say there is nothing there")
	}
}

// The length band is section 5.5 criterion 1 asked of production. It is the headline number
// of the whole report, so a wrong percentile would quietly mislead every tuning decision made
// from it.
func TestTheLengthBandIsMeasuredAgainstCriterionOne(t *testing.T) {
	clock := newClock()
	dir := t.TempDir()
	w := mustWriter(t, Options{Dir: dir, Now: clock.now})

	// Six replies: four inside the 4 to 12 band, two outside it.
	for _, n := range []int{2, 4, 6, 8, 12, 20} {
		if err := w.Write(Sample{
			Kind: KindSample, At: clock.now(), Outcome: "produced", Sent: true, Words: n,
		}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	var out strings.Builder
	if err := Report(dir, &out); err != nil {
		t.Fatalf("Report: %v", err)
	}
	got := out.String()

	// Nearest rank, not interpolated: on an even count that lands on the upper of the two
	// middle values, so 2,4,6,8,12,20 reports 8 rather than 7. Pinned because the convention
	// is a choice and a report is compared against the last one, where a silent switch
	// between conventions would look like the bot's output changed.
	if !strings.Contains(got, "median 8 words") {
		t.Errorf("median is wrong for 2,4,6,8,12,20:\n%s", got)
	}
	if !strings.Contains(got, " 66.7% of sent replies fall in the 4 to 12 word band") {
		t.Errorf("the band share is wrong; four of six are inside it:\n%s", got)
	}
}

// The report must not need a corpus, a token or a clock. This is the property that lets it
// run on a laptop against an archive, and it is asserted by there being nothing to set up.
func TestTheReportNeedsNothingButFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, prefix+"20260101-000000"+suffix)
	if err := os.WriteFile(path, []byte(`{"kind":"sample","outcome":"corpus-empty"}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	start := time.Now()
	var out strings.Builder
	if err := Report(dir, &out); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("Report took long enough to suggest it opened something it should not have")
	}
	if !strings.Contains(out.String(), "corpus-empty") {
		t.Errorf("the outcome is missing:\n%s", out.String())
	}
}
