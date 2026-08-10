package tuning

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/generate"
	"github.com/6586x57890143/peregrine/internal/storage"
)

func testOptions(dir string) Options {
	return Options{
		Dir:              dir,
		Keep:             5,
		Sample:           1,
		EngagementWindow: time.Minute,
		TrackMax:         100,
		Version:          "test",
	}
}

// start builds a service with no session, which is what makes this package testable with no
// gateway connection at all: the only thing the session is used for is arming the reaction
// handler, and a test drives noteReaction directly.
func start(t *testing.T, opts Options) *Service {
	t.Helper()

	s := New(nil, opts)
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := s.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

// stop shuts the service down and returns every record it wrote, decoded.
func stop(t *testing.T, s *Service, dir string) []map[string]any {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	var out []map[string]any
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // a temp dir this test made
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("decode %q: %v", line, err)
			}
			out = append(out, rec)
		}
	}
	return out
}

func ofKind(records []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if r["kind"] == kind {
			out = append(out, r)
		}
	}
	return out
}

// The whole loop in one test: a reply is recorded, watched, reacted to and answered, and the
// two records come back sharing an ID. Without the shared ID an analysis has a pile of
// replies and a pile of reactions and no way to join them.
func TestASentReplyAndItsEngagementShareAnID(t *testing.T) {
	dir := t.TempDir()
	s := start(t, testOptions(dir))

	s.Record(Generation{
		ID: "msg-1", Trigger: "reply", Channel: "chan-1",
		Prompt: "what do you know about greg", Reply: "greg is cooked honestly",
		Outcome: "produced", Sent: true, Took: 40 * time.Millisecond,
		Trace: &generate.Trace{SeedTier: "name", SeedKey: "greg", Steps: 4},
	})

	s.noteReaction("msg-1", "user-a")
	s.noteReaction("msg-1", "user-b")
	s.noteReaction("msg-1", "user-a") // the same person twice is one reactor and two reactions
	s.NoteReply("msg-1")
	s.NoteActivity("chan-1")
	s.NoteActivity("chan-1")

	records := stop(t, s, dir)

	samples := ofKind(records, "sample")
	if len(samples) != 1 {
		t.Fatalf("got %d sample records, want 1: %v", len(samples), records)
	}
	sample := samples[0]
	if sample["id"] != "msg-1" {
		t.Errorf("sample id = %v, want msg-1", sample["id"])
	}
	if sample["reply"] != "greg is cooked honestly" {
		t.Errorf("sample reply = %v", sample["reply"])
	}
	if sample["words"] != float64(4) {
		t.Errorf("sample words = %v, want 4", sample["words"])
	}
	if sample["version"] != "test" {
		t.Errorf("sample version = %v, want test", sample["version"])
	}
	trace, ok := sample["trace"].(map[string]any)
	if !ok {
		t.Fatalf("sample carries no trace: %v", sample)
	}
	if trace["seed_tier"] != "name" {
		t.Errorf("trace seed_tier = %v, want name", trace["seed_tier"])
	}

	engagements := ofKind(records, "engagement")
	if len(engagements) != 1 {
		t.Fatalf("got %d engagement records, want 1: %v", len(engagements), records)
	}
	e := engagements[0]
	if e["id"] != sample["id"] {
		t.Fatalf("engagement id %v does not match the sample id %v; nothing can join them",
			e["id"], sample["id"])
	}
	if e["reactions"] != float64(3) {
		t.Errorf("reactions = %v, want 3", e["reactions"])
	}
	if e["distinct_reactors"] != float64(2) {
		t.Errorf("distinct_reactors = %v, want 2: the same person reacting twice is one person",
			e["distinct_reactors"])
	}
	if e["replied"] != true {
		t.Error("replied = false after NoteReply")
	}
	if e["followups"] != float64(2) {
		t.Errorf("followups = %v, want 2", e["followups"])
	}
}

// A reaction on a message nobody is watching has to be a map miss and nothing else. That is
// what makes it safe to observe every reaction in the guild rather than only the bot's own.
func TestAReactionOnAnUnwatchedMessageIsIgnored(t *testing.T) {
	dir := t.TempDir()
	s := start(t, testOptions(dir))

	s.noteReaction("some-other-message", "user-a")
	s.NoteReply("some-other-message")

	if got := len(ofKind(stop(t, s, dir), "engagement")); got != 0 {
		t.Errorf("got %d engagement records for a message that was never watched, want 0", got)
	}
}

// Nothing is watched when nothing was sent, and a refused send carries no text. Both halves
// matter: an Engagement record for a message that does not exist would never resolve, and
// internal/safety's rule is that refused content is never written down anywhere.
func TestAnUnsentGenerationIsRecordedWithoutTextAndIsNotWatched(t *testing.T) {
	dir := t.TempDir()
	s := start(t, testOptions(dir))

	s.Record(Generation{
		ID: "", Trigger: "reply", Channel: "chan-1",
		Prompt: "say something awful", Reply: "the thing it would have said",
		Outcome: "produced", Sent: false,
	})
	// Also the ordinary silence: generation had nothing to say.
	s.Record(Generation{
		Trigger: "reply", Channel: "chan-1", Prompt: "hello", Outcome: "too-short", Sent: false,
	})

	records := stop(t, s, dir)

	samples := ofKind(records, "sample")
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2: silence is recorded too", len(samples))
	}
	for _, sample := range samples {
		if _, present := sample["reply"]; present {
			t.Errorf("an unsent sample carries reply text: %v", sample)
		}
		if sample["sent"] != false {
			t.Errorf("sent = %v, want false", sample["sent"])
		}
	}
	if got := len(ofKind(records, "engagement")); got != 0 {
		t.Errorf("got %d engagement records for replies that were never sent, want 0", got)
	}

	// The too-short outcome is the one an operator on a young corpus has to diagnose, so it
	// has to survive into the file rather than being filtered as uninteresting.
	found := false
	for _, sample := range samples {
		if sample["outcome"] == "too-short" {
			found = true
		}
	}
	if !found {
		t.Error("no sample carries the too-short outcome")
	}
}

// The recorder must drop rather than block. It is called from the reply path with somebody
// waiting for an answer, and telemetry that can stall the thing it measures is worse than no
// telemetry.
func TestTheRecorderDropsRatherThanBlockingWhenTheQueueIsFull(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	opts.QueueSize = 1

	s := New(nil, opts)
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Deliberately NOT started, so nothing drains the queue and the second record onwards
	// have nowhere to go. If submit blocked, this test would hang rather than fail, which is
	// the honest way to express "must not block".
	t.Cleanup(func() { _ = s.writer.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			s.Record(Generation{Trigger: "reply", Channel: "c", Outcome: "produced"})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked on a full queue; it must drop instead")
	}

	if s.Dropped() == 0 {
		t.Error("nothing was counted as dropped, so this test did not exercise a full queue")
	}
}

// The pending map is bounded, and going over the bound resolves the OLDEST early rather than
// discarding the newest. A dropped observation produces no record at all; one resolved early
// produces a record with a short window that an analysis can see and exclude.
func TestThePendingMapIsBoundedByResolvingTheOldestEarly(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	opts.TrackMax = 3

	s := start(t, opts)

	for i := range 6 {
		s.Record(Generation{
			ID: string(rune('a' + i)), Trigger: "reply", Channel: "chan-1",
			Reply: "something", Outcome: "produced", Sent: true,
		})
	}

	s.mu.Lock()
	pending := len(s.pending)
	s.mu.Unlock()
	if pending > opts.TrackMax {
		t.Errorf("pending holds %d observations, above the bound of %d", pending, opts.TrackMax)
	}

	records := stop(t, s, dir)
	if got := len(ofKind(records, "sample")); got != 6 {
		t.Errorf("got %d samples, want 6", got)
	}
	// Every one still resolves: three evicted early, three at shutdown.
	if got := len(ofKind(records, "engagement")); got != 6 {
		t.Errorf("got %d engagement records, want 6: an observation over the bound must be "+
			"resolved early rather than dropped", got)
	}
}

// The channel index is keyed by channel, so it grows with every guild the bot joins.
// Emptying the inner set is not enough; the key has to go. This repository has shipped that
// exact leak twice.
func TestResolvingAnObservationRemovesItsChannelIndexEntry(t *testing.T) {
	dir := t.TempDir()
	s := start(t, testOptions(dir))

	for i := range 20 {
		s.Record(Generation{
			ID:      "m" + string(rune('a'+i)),
			Channel: "c" + string(rune('a'+i)),
			Trigger: "reply", Reply: "x", Outcome: "produced", Sent: true,
		})
	}

	s.mu.Lock()
	before := len(s.byChannel)
	s.mu.Unlock()
	if before != 20 {
		t.Fatalf("byChannel holds %d channels, want 20; the test setup is wrong", before)
	}

	// Everything is due once the window has passed.
	s.sweep(time.Now().Add(2 * time.Minute))

	s.mu.Lock()
	after := len(s.byChannel)
	pending := len(s.pending)
	s.mu.Unlock()

	if pending != 0 {
		t.Errorf("pending holds %d observations after a sweep past every deadline", pending)
	}
	if after != 0 {
		t.Errorf("byChannel still holds %d channels after everything resolved; the map leaks "+
			"one key per channel the bot ever speaks in", after)
	}

	_ = stop(t, s, dir)
}

// With no directory configured the feature is off and every entry point is a no-op, rather
// than a nil writer waiting to be dereferenced on the reply path.
func TestTheFeatureOffIsANoOpEverywhere(t *testing.T) {
	s := New(nil, Options{})
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if s.Enabled() {
		t.Fatal("Enabled with no directory configured")
	}
	if err := s.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// None of these may panic, and none may write anything.
	s.Record(Generation{ID: "m1", Trigger: "reply", Reply: "hi", Sent: true})
	s.NoteReply("m1")
	s.NoteActivity("c1")
	s.Count("command:!leaderboard")
	s.noteReaction("m1", "u1")
	s.sweep(time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// The snapshot has to carry the weights. They are constants in the binary rather than
// environment variables, so an operator cannot reconstruct them from a deployment
// afterwards, which makes this the only place an archive records what produced its output.
func TestTheSnapshotCarriesTheWeightsAndTheParams(t *testing.T) {
	dir := t.TempDir()
	opts := testOptions(dir)
	opts.Dials = generate.Options{MaxNGram: 5, Temperature: 1.2, MinDistinctAuthors: 2}

	s := start(t, opts)
	s.Count("command:!leaderboard")
	s.Count("command:!leaderboard")
	s.Snapshot(storageStatus(), 1, 2, 3, false)

	snapshots := ofKind(stop(t, s, dir), "snapshot")
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snapshots))
	}
	snap := snapshots[0]

	weights, ok := snap["weights"].(map[string]any)
	if !ok || len(weights) == 0 {
		t.Fatalf("snapshot carries no weights: %v", snap)
	}
	for _, key := range []string{"prompt_name", "name_topic", "recent_context", "end_early"} {
		if _, present := weights[key]; !present {
			t.Errorf("weights are missing %q", key)
		}
	}

	params, ok := snap["params"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot carries no params: %v", snap)
	}
	if params["temperature"] != 1.2 {
		t.Errorf("params temperature = %v, want the configured 1.2", params["temperature"])
	}
	if params["min_distinct_authors"] != float64(2) {
		t.Errorf("params min_distinct_authors = %v, want 2", params["min_distinct_authors"])
	}

	usage, ok := snap["usage"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot carries no usage: %v", snap)
	}
	if usage["command:!leaderboard"] != float64(2) {
		t.Errorf("usage = %v, want the command counted twice", usage)
	}

	counters, ok := snap["counters"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot carries no counters: %v", snap)
	}
	if counters["emit_rejected"] != float64(3) {
		t.Errorf("emit_rejected = %v, want 3", counters["emit_rejected"])
	}
}

// storageStatus is a corpus status with distinguishable values, so a field mapped to the
// wrong place in map.go shows up as a wrong number rather than as a matching zero.
func storageStatus() storage.Status {
	return storage.Status{
		Ngrams: 11, AuthorEntries: 22, Topics: 33, TopicWords: 44,
		NameTopics: 55, Names: 66, HistoryWindow: 77, ImageCache: 88, Learned: 99,
	}
}
