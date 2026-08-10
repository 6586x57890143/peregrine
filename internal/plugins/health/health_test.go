package health

import (
	"bytes"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/dbtest"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// fakeQueue and fakeGate are the two counter sources. Both are atomic, because in production they
// are read by this loop while dispatcher workers and the gate write them.
type fakeQueue struct {
	dropped atomic.Uint64
	queued  atomic.Int64
}

func (q *fakeQueue) Dropped() uint64 { return q.dropped.Load() }
func (q *fakeQueue) Queued() int     { return int(q.queued.Load()) }

type fakeGate struct {
	learn  atomic.Uint64
	emit   atomic.Uint64
	paused atomic.Bool
}

func (g *fakeGate) LearnRejected() uint64 { return g.learn.Load() }
func (g *fakeGate) EmitRejected() uint64  { return g.emit.Load() }
func (g *fakeGate) Paused() bool          { return g.paused.Load() }

type fakeLatency time.Duration

func (l fakeLatency) HeartbeatLatency() time.Duration { return time.Duration(l) }

// fixture returns a service logging into a buffer, so the report can be read.
func fixture(t *testing.T, latency Latency) (*Service, *fakeQueue, *fakeGate, *bytes.Buffer) {
	t.Helper()

	var buf bytes.Buffer
	queue := &fakeQueue{}
	gate := &fakeGate{}
	s := New(dbtest.Store(t), queue, gate, latency, nil, Options{
		StatusTick:  time.Minute,
		LatencyTick: time.Minute,
		Threshold:   500 * time.Millisecond,
	})
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(&buf, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, queue, gate, &buf
}

// TestTheStatusReportCarriesTheCountersNobodyWasReading.
//
// Peregrine drops work and refuses output by design, and every one of those decisions is correct
// and INVISIBLE. A queue that is persistently full and a blocklist that is firing constantly look
// exactly like a quiet server unless something says otherwise, and until M13 nothing did: all four
// counters existed and were read by no code at all.
func TestTheStatusReportCarriesTheCountersNobodyWasReading(t *testing.T) {
	s, queue, gate, buf := fixture(t, fakeLatency(0))

	queue.dropped.Store(7)
	queue.queued.Store(3)
	gate.learn.Store(11)
	gate.emit.Store(2)

	s.reportStatus()

	got := buf.String()
	for _, want := range []string{
		"dropped_total=7", "queued=3", "learn_rejected_total=11", "emit_rejected_total=2",
		"ngrams=", "learned=", "paused=false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the status report does not mention %q:\n%s", want, got)
		}
	}
}

// TestTheReportCarriesDeltasNotJustTotals.
//
// A lifetime count of 40,000 rejections tells an operator nothing about whether it is happening
// now, which is the only question worth asking of a counter on a ticker.
func TestTheReportCarriesDeltasNotJustTotals(t *testing.T) {
	s, queue, gate, buf := fixture(t, fakeLatency(0))

	queue.dropped.Store(10)
	gate.emit.Store(5)
	s.reportStatus()
	buf.Reset()

	// Nothing new since the last report.
	s.reportStatus()
	if got := buf.String(); !strings.Contains(got, "dropped_since=0") ||
		!strings.Contains(got, "emit_rejected_since=0") {
		t.Errorf("a quiet interval did not report zero deltas:\n%s", got)
	}
	buf.Reset()

	queue.dropped.Store(13)
	gate.emit.Store(6)
	s.reportStatus()

	got := buf.String()
	if !strings.Contains(got, "dropped_since=3") {
		t.Errorf("the drop delta is wrong:\n%s", got)
	}
	if !strings.Contains(got, "dropped_total=13") {
		t.Errorf("the drop total is wrong:\n%s", got)
	}
	if !strings.Contains(got, "emit_rejected_since=1") {
		t.Errorf("the emit-rejection delta is wrong:\n%s", got)
	}
}

// TestSomethingHappeningNowIsSaidLouder. The routine line gets skimmed, so a nonzero delta gets
// its own record at a level that carries, and each one names what to do about it.
func TestSomethingHappeningNowIsSaidLouder(t *testing.T) {
	s, queue, gate, buf := fixture(t, fakeLatency(0))

	queue.dropped.Store(4)
	gate.emit.Store(1)
	s.reportStatus()

	got := buf.String()
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("a full queue and a firing emit gate produced no warning:\n%s", got)
	}
	if !strings.Contains(got, "PEREGRINE_MESSAGE_QUEUE") {
		t.Errorf("the drop warning does not name the variable to change:\n%s", got)
	}
	buf.Reset()

	// And a quiet interval says nothing loud, so the warnings above mean something.
	s.reportStatus()
	if got := buf.String(); strings.Contains(got, "level=WARN") {
		t.Errorf("a quiet interval produced a warning:\n%s", got)
	}
}

// TestACounterThatWentBackwardsReportsZero rather than eighteen quintillion. A counter cannot go
// backwards today, but a future reset would make it look as though it had, and unsigned
// subtraction is how that becomes a log line nobody can read.
func TestACounterThatWentBackwardsReportsZero(t *testing.T) {
	s, queue, _, buf := fixture(t, fakeLatency(0))

	queue.dropped.Store(100)
	s.reportStatus()
	buf.Reset()

	queue.dropped.Store(5)
	s.reportStatus()
	if got := buf.String(); !strings.Contains(got, "dropped_since=0") {
		t.Errorf("a counter that went backwards did not report a zero delta:\n%s", got)
	}
}

// TestThePauseSwitchIsVisible. PEREGRINE_PAUSE_ALL_WRITES refuses every send process-wide, and a
// silent bot must never be a mystery.
func TestThePauseSwitchIsVisible(t *testing.T) {
	s, _, gate, buf := fixture(t, fakeLatency(0))
	gate.paused.Store(true)

	s.reportStatus()
	if got := buf.String(); !strings.Contains(got, "paused=true") {
		t.Errorf("the pause switch is not in the status report:\n%s", got)
	}
}

// TestLatencyIsOnlyReportedWhenItIsWorthMentioning. A line that appears every tick regardless
// trains an operator to stop reading it.
func TestLatencyIsOnlyReportedWhenItIsWorthMentioning(t *testing.T) {
	s, _, _, buf := fixture(t, fakeLatency(50*time.Millisecond))
	s.reportLatency()
	if got := buf.String(); got != "" {
		t.Errorf("healthy latency was logged:\n%s", got)
	}

	s2, _, _, buf2 := fixture(t, fakeLatency(2*time.Second))
	s2.reportLatency()
	if got := buf2.String(); !strings.Contains(got, "gateway latency is high") {
		t.Errorf("high latency was not logged:\n%s", got)
	}
}

// TestNoHeartbeatYetIsNotAHealthProblem. Zero means no heartbeat has completed, which is normal in
// the first seconds and is not something to report.
func TestNoHeartbeatYetIsNotAHealthProblem(t *testing.T) {
	s, _, _, buf := fixture(t, fakeLatency(0))
	s.reportLatency()
	if got := buf.String(); got != "" {
		t.Errorf("a zero heartbeat latency was reported as a problem:\n%s", got)
	}

	// And a nil source, which is what a test or a pre-READY session gives.
	s2, _, _, buf2 := fixture(t, nil)
	s2.reportLatency()
	if got := buf2.String(); got != "" {
		t.Errorf("a nil latency source was reported as a problem:\n%s", got)
	}
}

// TestShutdownReportsOnceMore. That final report is where an operator reading a container's last
// output finds out whether the queue had been full or the gate had been busy.
func TestShutdownReportsOnceMore(t *testing.T) {
	s, queue, _, buf := fixture(t, fakeLatency(0))
	queue.dropped.Store(9)

	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "dropped_total=9") {
		t.Errorf("shutdown did not report the counters:\n%s", got)
	}
}

// TestAnUnreadableCorpusIsReportedRatherThanPanicking. The store is closed under it, which is what
// a report racing shutdown would hit.
func TestAnUnreadableCorpusIsReportedRatherThanPanicking(t *testing.T) {
	var buf bytes.Buffer
	store := dbtest.Store(t)
	s := New(store, &fakeQueue{}, &fakeGate{}, fakeLatency(0), nil, Options{
		StatusTick: time.Minute, LatencyTick: time.Minute, Threshold: time.Second,
	})
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(&buf, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s.reportStatus()
	if got := buf.String(); !strings.Contains(got, "level=ERROR") {
		t.Errorf("a failed corpus read was not reported:\n%s", got)
	}
}

// TestTheCorpusCountsComeFromTheStore, so the report is about the real thing rather than zeroes.
func TestTheCorpusCountsComeFromTheStore(t *testing.T) {
	var buf bytes.Buffer
	store := dbtest.Store(t)
	if err := store.Update(func(w *storage.Writer) error {
		return w.LearnNgram("the bird", "flew", "author-1")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := New(store, &fakeQueue{}, &fakeGate{}, fakeLatency(0), nil, Options{
		StatusTick: time.Minute, LatencyTick: time.Minute, Threshold: time.Second,
	})
	if err := s.Init(core.Deps{Logger: slog.New(slog.NewTextHandler(&buf, nil))}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	s.reportStatus()

	if got := buf.String(); strings.Contains(got, "ngrams=0") {
		t.Errorf("the report says the corpus is empty after a write:\n%s", got)
	}
}
