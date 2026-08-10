// Package tuning is the service that writes the tuning export and watches what happened to
// what the bot said.
//
// internal/tuning is the wire format and the file. This package is what fills it in: the
// non-blocking recorder the reply path calls, the bounded map that waits to see whether a
// reply drew a reaction, and the gateway handler that notices when it does.
//
// # Why the recorder never blocks
//
// Record is called from the reply path, on a message somebody is waiting for an answer to.
// It does a non-blocking send into a buffered channel and DROPS when the channel is full,
// which is core.Dispatcher's contract and is here for the same reason: a slow disk must not
// turn into unbounded memory or a stalled reply. Telemetry that can degrade the thing it
// measures is worse than no telemetry.
//
// The drop count rides in the next snapshot, because a file assembled from a queue that was
// full half the time is a BIASED sample and nothing else in it would say so. That is the
// lesson of the four counters internal/plugins/health exists to read: peregrine drops work
// by design, every one of those decisions is correct, and every one is invisible.
//
// # Why engagement is a second record rather than a field
//
// The answer to "did that land" arrives minutes after the reply. Filling it into the Sample
// would mean either rewriting a line in an append-only file or holding every sample in
// memory until it resolves. The pending map here is bounded and swept for exactly that
// reason: it is keyed by message ID, and an unbounded map keyed by message is a leak this
// repository has already shipped twice (the conversation memory before M7b and the
// word-game activity map in M11a).
package tuning

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/generate"
	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/storage"
	"github.com/6586x57890143/peregrine/internal/tuning"
)

// Options are the dials.
type Options struct {
	// Dir is where the export goes. Empty disables the whole feature, matching
	// PEREGRINE_BACKUP_DIR: there is no safe guess for a path, and writing somewhere the
	// operator did not choose is worse than not writing.
	Dir string

	// Rotate and Keep are the file lifecycle. See internal/tuning.
	Rotate time.Duration
	Keep   int

	// Sample is the probability that a generation attempt is recorded at all, for a server
	// busy enough that one record per reply is too many. 1.0 records everything, which is
	// the default because replies are rare compared to messages.
	Sample float64

	// EngagementWindow is how long a sent reply is watched for reactions and answers.
	EngagementWindow time.Duration

	// TrackMax bounds the pending map. Required, not optional: see the package comment.
	TrackMax int

	// FlushTick is how often buffered records reach the file. This is the bound on what a
	// crash loses, and it is deliberately not an fsync per record: the caller is a bot
	// answering a human.
	FlushTick time.Duration

	// QueueSize bounds the recorder's channel.
	QueueSize int

	// Version is PEREGRINE_VERSION, stamped on every record. In production it is the image
	// tag, which is the commit SHA CI built.
	Version string

	// Dials is the generation configuration in force, copied into every snapshot.
	//
	// This is the field that makes a version-to-version comparison mean anything. Reading
	// two archives and finding the output improved is worth nothing if the numbers that
	// produced them are not in the same file, and the logit weights are constants in the
	// binary rather than environment variables, so an operator cannot reconstruct them from
	// a .env afterwards.
	Dials generate.Options
}

func (o Options) withDefaults() Options {
	if o.Sample <= 0 || o.Sample > 1 {
		o.Sample = 1
	}
	if o.EngagementWindow <= 0 {
		o.EngagementWindow = 10 * time.Minute
	}
	if o.TrackMax <= 0 {
		o.TrackMax = 500
	}
	if o.FlushTick <= 0 {
		o.FlushTick = 30 * time.Second
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 256
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	return o
}

// Generation is one generation attempt as its caller saw it.
//
// Named after what it describes rather than after the record it becomes, because the caller
// does not know or care about the wire format. The flattening happens here, which is what
// lets internal/tuning stay a leaf whose types are stable across versions.
type Generation struct {
	// ID is the message the bot sent, or empty when nothing was sent. An empty ID means no
	// engagement record can ever follow, which is correct: there is nothing to react to.
	ID      string
	Trigger string
	Channel string

	Prompt     string
	HasContext bool
	Names      []string
	Roast      bool

	// Reply is dropped when Sent is false. See internal/tuning's package comment: a refused
	// emission leaves no text anywhere and telemetry is not the exception.
	Reply   string
	Outcome string
	Sent    bool

	Took  time.Duration
	Trace *generate.Trace
}

// Service is the feature.
type Service struct {
	opts Options

	logger *slog.Logger
	writer *tuning.Writer

	queue   chan tuning.Record
	dropped atomic.Uint64

	mu sync.Mutex
	// pending is keyed by the bot's message ID, and byChannel indexes the same entries so
	// that noting traffic in a channel does not have to scan every pending reply.
	pending   map[string]*observation
	byChannel map[string]map[string]struct{}
	usage     map[string]uint64

	startedAt   time.Time
	dispatcher  *core.Dispatcher
	drain       sync.WaitGroup
	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
	session     *discordgo.Session
}

// observation is one sent reply being watched.
type observation struct {
	channel  string
	sentAt   time.Time
	due      time.Time
	reactors map[string]struct{}

	reactions int
	replied   bool
	followups int
}

// New builds the service. A zero Dir means the feature is off and every entry point becomes
// a cheap no-op rather than a nil dereference.
func New(session *discordgo.Session, opts Options) *Service {
	opts = opts.withDefaults()
	return &Service{
		session:   session,
		opts:      opts,
		pending:   map[string]*observation{},
		byChannel: map[string]map[string]struct{}{},
		usage:     map[string]uint64{},
	}
}

func (s *Service) Name() string { return "tuning" }

// Enabled reports whether anything is being written.
func (s *Service) Enabled() bool { return s.opts.Dir != "" }

// Init opens the file and arms the reaction handler.
//
// The handler is registered HERE rather than in Start, and that placement is the same rule
// the reactor follows: discordgo begins dispatching inside session.Open, which happens
// between Init and Start, so a handler armed in Start silently drops everything that
// arrived in that window.
//
// A directory that cannot be opened is a WARNING and the feature turns itself off. One
// optional behaviour failing must never take the process down, and the corpus matters more
// than the telemetry describing it.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger
	s.dispatcher = deps.Dispatcher

	if !s.Enabled() {
		s.logger.Info("the tuning export is off; set PEREGRINE_TUNING_DIR to enable it")
		return nil
	}

	w, err := tuning.NewWriter(tuning.Options{
		Dir:    s.opts.Dir,
		Rotate: s.opts.Rotate,
		Keep:   s.opts.Keep,
	})
	if err != nil {
		s.logger.Warn("the tuning export could not open its directory, so it will not run",
			"dir", s.opts.Dir, "err", err)
		s.opts.Dir = ""
		return nil
	}
	s.writer = w
	s.queue = make(chan tuning.Record, s.opts.QueueSize)

	if s.session != nil {
		s.session.AddHandler(s.onReaction)
	}

	s.logger.Info("the tuning export is on",
		"dir", s.opts.Dir, "file", w.Name(), "rotate", s.opts.Rotate, "keep", s.opts.Keep,
		"sample", s.opts.Sample, "window", s.opts.EngagementWindow, "version", s.opts.Version)
	return nil
}

// Start launches the drain, the sweep and the flush.
func (s *Service) Start(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	s.startedAt = time.Now()

	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	// The drain is a plain goroutine rather than a core.Loop, because it is not periodic:
	// it blocks on the channel and ends when the channel closes. Shutdown closes it after
	// the loops have stopped, which is what makes the last records reach the file.
	s.drain.Add(1)
	go s.drainQueue()

	loops := []core.Loop{
		{
			Name:  "tuning-sweep",
			Every: sweepTick,
			Fn:    func(context.Context) { s.sweep(time.Now()) },
		},
		{
			Name:  "tuning-flush",
			Every: s.opts.FlushTick,
			Fn:    func(context.Context) { s.flush() },
		},
	}
	for _, l := range loops {
		core.RunLoop(loopCtx, &s.loops, s.logger, l)
	}
	return nil
}

// sweepTick is how often due observations are collected. Fixed rather than configurable:
// the resolution of an engagement window is this, and an operator has no reason to tune the
// granularity of a ten-minute measurement.
const sweepTick = 30 * time.Second

// Shutdown resolves what is still pending, drains the queue and closes the file.
//
// Everything still being watched is written out with the window it ACTUALLY got rather than
// being discarded. A partial window is honest because WindowS is a field: an analysis can
// exclude short windows, and it cannot recover records that were never written.
func (s *Service) Shutdown(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}

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
		s.logger.Warn("tuning loops still running at the shutdown deadline")
	}

	// Everything, due or not. After the loops have stopped, so nothing can add more.
	s.resolveAll()

	close(s.queue)
	s.drain.Wait()

	written := s.writer.Written()
	if err := s.writer.Close(); err != nil {
		return err
	}
	s.logger.Info("tuning export closed", "records", written, "dropped", s.dropped.Load())
	return nil
}

// Record is the recorder. It never blocks and never returns an error.
//
// A caller on the reply path has nothing useful to do about a telemetry failure, and giving
// it an error to ignore would be an error somebody eventually logs at the wrong level.
func (s *Service) Record(g Generation) {
	if !s.Enabled() {
		return
	}

	// Sampled before anything else, so a sampled-out generation costs one comparison and is
	// not watched for engagement either: an Engagement record whose Sample was never written
	// is an orphan the report would have to discard.
	if s.opts.Sample < 1 && rand.Float64() >= s.opts.Sample {
		return
	}

	now := time.Now()
	rec := tuning.Sample{
		Kind:        tuning.KindSample,
		At:          now,
		ID:          g.ID,
		Version:     s.opts.Version,
		Generation:  learn.Generation,
		Trigger:     g.Trigger,
		Channel:     g.Channel,
		Prompt:      g.Prompt,
		PromptWords: countWords(g.Prompt),
		HasContext:  g.HasContext,
		Names:       g.Names,
		Roast:       g.Roast,
		Outcome:     g.Outcome,
		Sent:        g.Sent,
		TookMS:      g.Took.Milliseconds(),
		Trace:       flatten(g.Trace),
	}
	if g.Sent {
		rec.Reply = g.Reply
		rec.Words = countWords(g.Reply)
	}

	s.submit(rec)

	if g.Sent && g.ID != "" {
		s.watch(g.ID, g.Channel, now)
	}
}

// NoteReply records that a human replied to one of the bot's messages.
//
// The strongest engagement signal available without asking Discord for anything: the
// reactor already computes REPLY_TO_BOT to decide whether to answer, so this costs nothing
// new at the call site.
func (s *Service) NoteReply(botMessageID string) {
	if !s.Enabled() || botMessageID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.pending[botMessageID]; ok {
		o.replied = true
	}
}

// NoteActivity records a message in a channel, as the denominator for the signals above.
//
// Indexed by channel rather than scanned, because this is called once per message the bot
// sees and the pending map holds hundreds of entries. A scan here would be a per-message
// cost proportional to how chatty the bot has been.
func (s *Service) NoteActivity(channelID string) {
	if !s.Enabled() || channelID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.byChannel[channelID] {
		if o, ok := s.pending[id]; ok {
			o.followups++
		}
	}
}

// Count tallies a named event for the snapshot.
//
// Lifetime totals rather than deltas, deliberately, and the opposite choice from the health
// report. Health prints to a log an operator skims, where a lifetime count of 40,000 says
// nothing about now; this writes to a file where every snapshot is timestamped and adjacent,
// so a total is strictly more information and the delta is a subtraction at read time.
func (s *Service) Count(event string) {
	if !s.Enabled() || event == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage[event]++
}

// Snapshot writes the periodic state record. Called by internal/plugins/health, which
// already pays for the corpus page walk on its own ticker: a second one here would be the
// same expensive question asked twice.
func (s *Service) Snapshot(st storage.Status, queueDropped, learnRejected, emitRejected uint64, paused bool) {
	if !s.Enabled() {
		return
	}

	s.mu.Lock()
	usage := make(map[string]uint64, len(s.usage))
	for k, v := range s.usage {
		usage[k] = v
	}
	s.mu.Unlock()

	s.submit(tuning.Snapshot{
		Kind:       tuning.KindSnapshot,
		At:         time.Now(),
		Version:    s.opts.Version,
		Generation: learn.Generation,
		UptimeS:    int64(time.Since(s.startedAt).Seconds()),
		Corpus: tuning.Corpus{
			Ngrams:        st.Ngrams,
			AuthorEntries: st.AuthorEntries,
			Topics:        st.Topics,
			TopicWords:    st.TopicWords,
			NameTopics:    st.NameTopics,
			Names:         st.Names,
			HistoryWindow: st.HistoryWindow,
			ImageCache:    st.ImageCache,
			Learned:       st.Learned,
		},
		Counters: tuning.Counters{
			QueueDropped:  queueDropped,
			LearnRejected: learnRejected,
			EmitRejected:  emitRejected,
			ExportDropped: s.dropped.Load(),
			Paused:        paused,
		},
		Params:  s.params(),
		Weights: s.weights(),
		Usage:   usage,
	})
}

// onReaction counts a reaction on a message being watched.
//
// Through the dispatcher like every other gateway event. A moderator reacting to a raid, or
// a popular message collecting a burst, arrives as many events on many discordgo goroutines,
// and taking this package's mutex on each of them directly would put an unbounded number of
// them in line for it.
//
// A reaction on a message nobody is watching is a map miss and costs nothing, which is what
// makes it safe to observe every reaction in the guild.
func (s *Service) onReaction(_ *discordgo.Session, m *discordgo.MessageReactionAdd) {
	if !s.Enabled() || m == nil || m.MessageID == "" {
		return
	}
	messageID, userID := m.MessageID, m.UserID
	if !s.dispatcher.Submit(func(context.Context) { s.noteReaction(messageID, userID) }) {
		// Not worth a log line per dropped reaction: the dispatcher already counts drops
		// and health reports them, and a reaction lost from a telemetry sample is the least
		// costly thing in the queue.
		return
	}
}

func (s *Service) noteReaction(messageID, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	o, ok := s.pending[messageID]
	if !ok {
		return
	}
	o.reactions++
	if userID != "" {
		o.reactors[userID] = struct{}{}
	}
}

// watch starts observing a sent reply.
func (s *Service) watch(messageID, channelID string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The bound is enforced by resolving the OLDEST entry early rather than by refusing the
	// new one. An entry that is dropped produces no record at all; one resolved early
	// produces a record with a short WindowS, which an analysis can see and exclude. Losing
	// the newest observation to protect a bound would also bias the file toward quiet
	// periods, which are exactly the ones nobody needs data about.
	for len(s.pending) >= s.opts.TrackMax {
		oldest, at := "", time.Time{}
		for id, o := range s.pending {
			if oldest == "" || o.sentAt.Before(at) {
				oldest, at = id, o.sentAt
			}
		}
		if oldest == "" {
			break
		}
		s.emit(s.take(oldest), oldest, now)
	}

	s.pending[messageID] = &observation{
		channel:  channelID,
		sentAt:   now,
		due:      now.Add(s.opts.EngagementWindow),
		reactors: map[string]struct{}{},
	}
	if s.byChannel[channelID] == nil {
		s.byChannel[channelID] = map[string]struct{}{}
	}
	s.byChannel[channelID][messageID] = struct{}{}
}

// sweep resolves every observation whose window has closed.
func (s *Service) sweep(now time.Time) {
	s.mu.Lock()
	var due []string
	for id, o := range s.pending {
		if now.After(o.due) {
			due = append(due, id)
		}
	}
	records := make([]tuning.Record, 0, len(due))
	for _, id := range due {
		if o := s.take(id); o != nil {
			records = append(records, engagementOf(o, id, now))
		}
	}
	s.mu.Unlock()

	// Outside the lock, which is a habit rather than a requirement here: submit is a
	// non-blocking channel send and cannot wait on anything, which is also why watch below
	// can call it while holding the lock without risking a deadlock. Keeping the batch path
	// outside anyway means the critical section stays a map walk, so a burst of due
	// observations does not hold the lock against the reply path recording a new one.
	for _, rec := range records {
		s.submit(rec)
	}
}

// resolveAll writes out everything still pending, at shutdown.
func (s *Service) resolveAll() {
	now := time.Now()

	s.mu.Lock()
	records := make([]tuning.Record, 0, len(s.pending))
	for id := range s.pending {
		if o := s.take(id); o != nil {
			records = append(records, engagementOf(o, id, now))
		}
	}
	s.mu.Unlock()

	for _, rec := range records {
		s.submit(rec)
	}
}

// take removes an observation and its channel index entry. The caller holds the lock.
func (s *Service) take(messageID string) *observation {
	o, ok := s.pending[messageID]
	if !ok {
		return nil
	}
	delete(s.pending, messageID)
	if set := s.byChannel[o.channel]; set != nil {
		delete(set, messageID)
		if len(set) == 0 {
			// The channel index is keyed by channel and so grows with every guild the bot
			// joins. Emptying it is not enough; the key has to go.
			delete(s.byChannel, o.channel)
		}
	}
	return o
}

// emit writes one observation immediately. The caller holds the lock, which is why it goes
// straight to submit rather than through the batching the sweep does: this path runs at most
// once per new observation over the bound.
func (s *Service) emit(o *observation, messageID string, now time.Time) {
	if o == nil {
		return
	}
	s.submit(engagementOf(o, messageID, now))
}

func engagementOf(o *observation, messageID string, now time.Time) tuning.Engagement {
	return tuning.Engagement{
		Kind:             tuning.KindEngagement,
		At:               now,
		ID:               messageID,
		Channel:          o.channel,
		Reactions:        o.reactions,
		DistinctReactors: len(o.reactors),
		Replied:          o.replied,
		Followups:        o.followups,
		WindowS:          int(now.Sub(o.sentAt).Seconds()),
	}
}

// submit hands a record to the drain, dropping when the queue is full.
func (s *Service) submit(rec tuning.Record) {
	select {
	case s.queue <- rec:
	default:
		s.dropped.Add(1)
	}
}

// drainQueue is the single writer goroutine.
func (s *Service) drainQueue() {
	defer s.drain.Done()
	for rec := range s.queue {
		if err := s.writer.Write(rec); err != nil {
			// Logged once per failure rather than counted silently. A write that fails is
			// also what suppresses pruning inside the writer, so the operator's next
			// question after seeing this is answered: nothing has been deleted.
			s.logger.Warn("tuning export write failed", "err", err)
		}
	}
}

func (s *Service) flush() {
	if err := s.writer.Flush(); err != nil {
		s.logger.Warn("tuning export flush failed", "err", err)
	}
}

// Dropped reports how many records the recorder threw away.
func (s *Service) Dropped() uint64 { return s.dropped.Load() }
