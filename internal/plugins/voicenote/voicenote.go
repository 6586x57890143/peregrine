// Package voicenote is the transcription feature: somebody posts a Discord voice message and
// the bot says what was in it.
//
// It is a complete plugin over an Engine seam, and the only Engine that ships is a stub. That
// split is the point of the milestone: the parts that belong to this repository are here and
// tested, and the part that needs a 465 MiB model and platform binaries is one interface away.
// See engine.go for what a real one must not reproduce.
//
// # Why the feature is structured this way at all
//
// A Whisper transcript is the least controlled text peregrine has ever handled. It comes from
// arbitrary audio somebody uploaded, it is not typed by anyone the bot can hold responsible,
// and until M10a it reached Discord without passing CheckEmit, because the emit gate sat at the
// generation exit and this path did not generate. Someone could have made the bot say anything
// by saying it out loud (SPEC.md section 4, A2 and A3).
//
// So the two things this plugin must get right are both about chokepoints rather than about
// audio: the transcript goes to Discord through internal/discordguard, and it enters the corpus
// through internal/learn, whose CheckLearn is inside its one entry point. Neither is optional
// and neither is this package's to decide.
package voicenote

import (
	"context"
	"log"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/core"
)

// Guard is the send chokepoint. A transcript is text the bot posts, so it goes through mention
// suppression, the emit gate and the pause switch like anything else.
type Guard interface {
	SendReply(channelID, content string, ref *discordgo.MessageReference) (*discordgo.Message, bool)
	Edit(channelID, messageID, content string) bool
}

// Options are the dials.
type Options struct {
	// Enabled is PEREGRINE_ENABLE_TRANSCRIPTION. It defaults to false, and that default
	// deliberately differs from the compile-time constant it replaced: the feature needs
	// assets that exist in no deployed environment.
	Enabled bool

	// QueueSize bounds work in flight. Transcription is slow and voice notes arrive in
	// bursts, so the queue is what stops a burst becoming unbounded memory. A full queue
	// drops and says so, which is the same honest semantics the message dispatcher uses.
	QueueSize int
}

// job is one voice note waiting to be transcribed.
type job struct {
	url           string
	channelID     string
	messageID     string
	placeholderID string
}

// Service is the feature.
type Service struct {
	engine Engine
	guard  Guard
	opts   Options
	logger *slog.Logger

	queue chan job

	// wg tracks the worker so Shutdown can wait for an in-flight transcription. Only Start
	// ever Adds to it, which is the invariant that makes the finding-4 panic impossible: an
	// Add racing a Wait at zero panics.
	wg          sync.WaitGroup
	cancelWork  context.CancelFunc
	dropped     int
	droppedLock sync.Mutex
}

// New builds the service. A nil Engine means the stub, which reports itself unavailable.
func New(engine Engine, guard Guard, opts Options) *Service {
	if engine == nil {
		engine = StubEngine()
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 32
	}
	return &Service{engine: engine, guard: guard, opts: opts}
}

func (s *Service) Name() string { return "voicenote" }

// Init reports what the feature can actually do, at startup, once.
//
// The flag being on with no engine behind it is a WARNING rather than silence, because a
// feature that is enabled and does nothing is the exact shape of findings 30 and G3: the
// operator sets the variable, watches nothing happen, and concludes the bot ignores its
// configuration. It is not a startup error, because one optional behaviour being unavailable
// must never take the process down.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger
	s.queue = make(chan job, s.opts.QueueSize)

	switch {
	case !s.opts.Enabled:
		// Nothing to say: the operator turned it off.
	case !s.engine.Available():
		s.logger.Warn("transcription is enabled but no engine is available, so voice notes will " +
			"be ignored. The engine is a seam in internal/plugins/voicenote and no implementation " +
			"ships in this repository (SPEC.md section 9, M12b)")
	default:
		s.logger.Info("transcription is enabled and an engine is available")
	}
	return nil
}

// Start launches the worker, if there is anything for it to do.
func (s *Service) Start(ctx context.Context) error {
	if !s.Available() {
		return nil
	}
	workCtx, cancel := context.WithCancel(ctx)
	s.cancelWork = cancel

	// Not a core.RunLoop: this blocks on a queue rather than a ticker. It is registered with
	// the same WaitGroup so Shutdown waits for an in-flight transcription the way it waits for
	// a tick.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.work(workCtx)
	}()
	return nil
}

// Shutdown stops the worker and waits for it, bounded by ctx.
//
// The queue is deliberately NOT closed. Closing it would let an in-flight Offer panic on send,
// which is the same reasoning the message dispatcher's queue is built on: cancelling the
// worker's context and letting the send drop is the shape that cannot race.
func (s *Service) Shutdown(ctx context.Context) error {
	if s.cancelWork != nil {
		s.cancelWork()
	}
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.logger.Warn("shutdown deadline reached with a transcription still running")
	}
	if n := s.Dropped(); n > 0 {
		s.logger.Info("voice notes dropped because the transcription queue was full", "count", n)
	}
	return nil
}

// Available reports whether the feature will do anything.
func (s *Service) Available() bool { return s.opts.Enabled && s.engine.Available() }

// Dropped reports how many voice notes were refused for want of queue space.
func (s *Service) Dropped() int {
	s.droppedLock.Lock()
	defer s.droppedLock.Unlock()
	return s.dropped
}

// Offer queues a message's voice attachments and reports whether it took any.
//
// It posts a placeholder first and queues the job only if the placeholder was SENT. That
// ordering matters: without a placeholder there is nothing to edit later, so queueing anyway
// would burn a transcription run and then log an edit failure against a message ID that never
// existed.
//
// Returning false is the normal case for almost every message. The caller does not consume the
// message either way: a voice note is still a message, and one that happens to carry audio is
// not addressed to the bot.
func (s *Service) Offer(channelID, messageID, _ string, attachments []discordgo.MessageAttachment) bool {
	if !s.Available() {
		return false
	}

	took := false
	for _, att := range attachments {
		if !isAudio(att.Filename) {
			continue
		}

		placeholder, ok := s.guard.SendReply(channelID, "\U0001F50A transcription in progress...",
			&discordgo.MessageReference{MessageID: messageID, ChannelID: channelID})
		if !ok {
			log.Printf("[VOICE] placeholder not sent, skipping transcription")
			continue
		}

		select {
		case s.queue <- job{
			url:           att.URL,
			channelID:     channelID,
			messageID:     messageID,
			placeholderID: placeholder.ID,
		}:
			took = true
		default:
			// Non-blocking, because Offer runs on a dispatcher worker and blocking here would
			// stall message handling behind a slow transcription. The placeholder is already
			// posted, so it is edited rather than left saying "in progress" forever.
			s.droppedLock.Lock()
			s.dropped++
			s.droppedLock.Unlock()
			s.guard.Edit(channelID, placeholder.ID, "transcription queue is full, skipping this one")
		}
	}
	return took
}

// work drains the queue until the context is cancelled.
func (s *Service) work(ctx context.Context) {
	log.Println("[INFO] Transcription worker started.")
	for {
		select {
		case j := <-s.queue:
			s.transcribe(ctx, j)
		case <-ctx.Done():
			log.Println("[INFO] Transcription worker stopped by shutdown signal.")
			return
		}
	}
}

// transcribe runs one job and edits its placeholder with the result.
//
// The result is NOT learned here, and that is deliberate. The old implementation fed
// transcripts into the corpus from this path, which was one of A1's four unfiltered callers,
// and a transcript is the least controlled text the bot handles: it comes from arbitrary audio
// and is not typed by anybody the bot can hold responsible. If a future engine wants transcripts
// learned, it goes through learn.Learner.Message like every other path, and the decision to do
// it belongs in a milestone rather than in a hidden side effect here.
func (s *Service) transcribe(ctx context.Context, j job) {
	text, err := s.engine.Transcribe(ctx, j.url)
	if err != nil {
		s.logger.Error("transcription failed", "channel", j.channelID, "err", err)
		s.guard.Edit(j.channelID, j.placeholderID, "could not transcribe that one")
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		s.guard.Edit(j.channelID, j.placeholderID, "could not make out any speech in that")
		return
	}
	s.guard.Edit(j.channelID, j.placeholderID, "\U0001F50A "+text)
}

// isAudio reports whether a filename looks like something an engine could read.
//
// Discord voice messages are always .ogg and named voice-message.ogg, but people also post
// ordinary audio attachments, and the two are the same job once the file is in hand.
func isAudio(filename string) bool {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".ogg", ".mp3", ".wav", ".m4a", ".flac":
		return true
	}
	return false
}
