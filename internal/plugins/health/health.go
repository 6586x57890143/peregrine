// Package health reports what the bot is doing and whether anything is silently going wrong.
//
// It is one service with two loops, and it exists because the two things it replaced were both
// reporting into the void. The corpus status line printed sizes and nothing else; the latency
// monitor made a REST call every two minutes to measure something discordgo already tracks. And
// four counters that exist specifically so that a persistent problem is visible rather than
// inferred were read by nothing at all.
//
// # The counters are the point
//
// Peregrine drops work and refuses output by design, and every one of those decisions is
// deliberate: the dispatcher drops rather than blocking, because discordgo dispatches every
// event on its own goroutine and blocking would grow goroutines without bound; the safety gate
// refuses on both directions. Each is the correct behaviour and each is INVISIBLE, which is the
// problem. A queue that is persistently full and a blocklist that is firing constantly look
// exactly like a quiet server unless something says otherwise.
//
// So the status line carries them, and it carries them as deltas since the last report rather
// than only as lifetime totals: a lifetime count of 40,000 rejections tells an operator nothing
// about whether it is happening now.
package health

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// Queue is the dispatcher's accounting. internal/core's Dispatcher satisfies it.
type Queue interface {
	Dropped() uint64
	Queued() int
}

// Gate is the safety gate's accounting. internal/safety's Gate satisfies it.
type Gate interface {
	LearnRejected() uint64
	EmitRejected() uint64
	Paused() bool
}

// Latency reports the gateway heartbeat round trip. *discordgo.Session satisfies it.
//
// HeartbeatLatency rather than a REST call: the version this replaces called User("@me") every
// two minutes purely to time it, which is asking the network for something the library already
// measures. That is finding 17's shape (go to the network for what you already have) in a
// different feature, and the REST call also measures the wrong thing, since a slow REST endpoint
// and a struggling gateway connection are different problems.
type Latency interface {
	HeartbeatLatency() time.Duration
}

// Reporter receives the same status this package logs, for anything that wants it as data
// rather than as a line. internal/plugins/tuning satisfies it.
//
// It is here, taking what reportStatus has already computed, for the reason finding 17
// keeps restating: Reader.Status walks every page in several buckets, which is genuinely
// expensive and is why it lives on a ticker rather than on the message path. A second
// service asking the corpus the same question on its own ticker would pay that cost twice
// for one answer.
//
// The signature is wide on purpose. A struct would need one of the two packages to own it,
// and the counters do not belong to either: they come from the dispatcher and the safety
// gate, and health is only the thing that happens to read them.
type Reporter interface {
	Snapshot(st storage.Status, queueDropped, learnRejected, emitRejected uint64, paused bool)
}

// Options are the dials.
type Options struct {
	// StatusTick is how often the corpus and counter line is printed.
	StatusTick time.Duration

	// LatencyTick is how often the gateway is checked, and Threshold is the latency worth
	// mentioning. Below it, nothing is logged: a line that appears every tick regardless
	// trains an operator to stop reading it.
	LatencyTick time.Duration
	Threshold   time.Duration
}

// Service is the feature.
type Service struct {
	store    *storage.Store
	queue    Queue
	gate     Gate
	latency  Latency
	reporter Reporter
	opts     Options
	logger   *slog.Logger

	// Previous counter values, so the report can carry deltas. Only the status loop touches
	// them, and there is one status loop, so they need no lock.
	lastDropped uint64
	lastLearn   uint64
	lastEmit    uint64

	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
}

// New builds the service. A nil Reporter means nothing wants the status as data, which is
// the case whenever the tuning export is off.
func New(store *storage.Store, queue Queue, gate Gate, latency Latency, reporter Reporter, opts Options) *Service {
	return &Service{store: store, queue: queue, gate: gate, latency: latency, reporter: reporter, opts: opts}
}

func (s *Service) Name() string { return "health" }

// Init records the logger. There is no persistent state: everything reported here is either a
// counter in memory or a count in the corpus.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger
	return nil
}

// Start launches both loops.
func (s *Service) Start(ctx context.Context) error {
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	loops := []core.Loop{
		{
			Name:      "status",
			Every:     s.opts.StatusTick,
			Immediate: true, // wanted at startup: it is the first sign of life
			Fn:        func(context.Context) { s.reportStatus() },
		},
		{
			Name:  "latency",
			Every: s.opts.LatencyTick,
			Fn:    func(context.Context) { s.reportLatency() },
		},
	}
	for _, l := range loops {
		core.RunLoop(loopCtx, &s.loops, s.logger, l)
	}
	return nil
}

// Shutdown stops the loops and reports once more.
//
// The final report is the useful one: it is where an operator reading a container's last output
// finds out whether the queue had been full or the gate had been busy. Printed before the wait,
// so a stuck loop cannot cost it.
func (s *Service) Shutdown(ctx context.Context) error {
	s.reportStatus()

	if s.cancelLoops != nil {
		s.cancelLoops()
	}
	done := make(chan struct{})
	go func() {
		s.loops.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// reportStatus logs the corpus size and the four counters.
//
// The corpus counts come from counters in the meta bucket, except inside Reader.Status where a
// page walk is the only way to answer. That is why this is on a ticker and not on the message
// path: the size field used to be filled by a Bucket.Stats() call once per message, and Stats()
// walks every page in the bucket (SPEC.md section 8, finding 11).
func (s *Service) reportStatus() {
	start := time.Now()

	var st storage.Status
	if err := s.store.View(func(r *storage.Reader) error {
		st = r.Status()
		return nil
	}); err != nil {
		s.logger.Error("reading corpus status", "err", err)
		return
	}

	dropped, learnRejected, emitRejected := s.counters()

	// Handed on as data BEFORE the log line, so a reporter that panics is caught by the
	// RunLoop wrapper with the status still unlogged rather than half-logged. The counters
	// passed are the TOTALS: this is one observation for an archive, where a delta is a
	// subtraction between two adjacent timestamped records, whereas the log line below is
	// skimmed once and a lifetime total there says nothing about now.
	if s.reporter != nil {
		s.reporter.Snapshot(st, dropped.total, learnRejected.total, emitRejected.total, s.gate.Paused())
	}

	// One record with everything, rather than a line per subsystem. An operator comparing two
	// reports wants them adjacent.
	s.logger.Info("status",
		"ngrams", st.Ngrams,
		"author_entries", st.AuthorEntries,
		"topics", st.Topics,
		"topic_word", st.TopicWords,
		"name_topic", st.NameTopics,
		"names", st.Names,
		"history", st.HistoryWindow,
		"images", st.ImageCache,
		"learned", st.Learned,
		"queued", s.queue.Queued(),
		"dropped_total", dropped.total,
		"dropped_since", dropped.delta,
		"learn_rejected_total", learnRejected.total,
		"learn_rejected_since", learnRejected.delta,
		"emit_rejected_total", emitRejected.total,
		"emit_rejected_since", emitRejected.delta,
		"paused", s.gate.Paused(),
		"took", time.Since(start),
	)

	// Said again, louder, when something is happening NOW. The line above is routine and gets
	// skimmed; these three are the ones worth a warning, and each names what to do about it.
	if dropped.delta > 0 {
		s.logger.Warn("the work queue dropped messages since the last report; the corpus is "+
			"keeping up badly or PEREGRINE_MESSAGE_QUEUE is too small",
			"dropped", dropped.delta)
	}
	if emitRejected.delta > 0 {
		s.logger.Warn("the emit gate refused output since the last report; this is the gate "+
			"working, and a sustained rate means somebody is trying",
			"refused", emitRejected.delta)
	}
	if learnRejected.delta > 0 {
		s.logger.Info("the learn gate dropped messages since the last report",
			"dropped", learnRejected.delta)
	}
}

// counter is a lifetime total plus the change since the previous report.
type counter struct {
	total uint64
	delta uint64
}

func (s *Service) counters() (dropped, learn, emit counter) {
	d, l, e := s.queue.Dropped(), s.gate.LearnRejected(), s.gate.EmitRejected()

	// Saturating subtraction, because a counter cannot go backwards but a future reset could
	// make it look as though it had, and a delta of 18 quintillion in a log line is worse than
	// a delta of zero.
	dropped = counter{total: d, delta: since(d, s.lastDropped)}
	learn = counter{total: l, delta: since(l, s.lastLearn)}
	emit = counter{total: e, delta: since(e, s.lastEmit)}

	s.lastDropped, s.lastLearn, s.lastEmit = d, l, e
	return dropped, learn, emit
}

func since(now, before uint64) uint64 {
	if now < before {
		return 0
	}
	return now - before
}

// reportLatency logs the gateway heartbeat round trip, but only when it is worth mentioning.
func (s *Service) reportLatency() {
	if s.latency == nil {
		return
	}
	got := s.latency.HeartbeatLatency()
	if got <= 0 {
		// No heartbeat has completed yet, which is normal in the first seconds and is not
		// something to report as a health problem.
		return
	}
	if got > s.opts.Threshold {
		s.logger.Warn("gateway latency is high", "latency", got, "threshold", s.opts.Threshold)
	}
}

// SessionLatency adapts a session to Latency, and exists so cmd/bot does not hand this package a
// *discordgo.Session it would then have to import discordgo to name.
func SessionLatency(session *discordgo.Session) Latency { return sessionLatency{s: session} }

type sessionLatency struct{ s *discordgo.Session }

func (l sessionLatency) HeartbeatLatency() time.Duration {
	if l.s == nil {
		return 0
	}
	return l.s.HeartbeatLatency()
}
