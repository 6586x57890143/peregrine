// Package repair re-walks Discord history to rebuild what the corpus cannot rebuild itself.
//
// # The failure class this exists for
//
// Finding 33 found that the historical backfill wrote NO co-occurrence associations: it passed
// no author, learn.associate returns early on an empty name set, and M14 fixed the writer.
//
// Fixing the writer did not fix the data, and that is the general lesson rather than the
// specific bug: A FIX TO A WRITER IS NOT A FIX TO THE DATA, AND WHETHER THE DATA IS
// RE-DERIVABLE IS A PROPERTY OF THE LAYOUT that has to be checked before the fix is called
// done. The corpus stores n-grams and counts and never message text, so anything built from
// message STRUCTURE has this property. The association indexes were one. topic, kn_succ and
// kn_pre are derivable and already have Writer.RebuildKNIndexes; a future index keyed on word
// position or message shape would not be.
//
// So this is a table of jobs (jobs.go) rather than one hard-coded pass, and the next repair is
// a row rather than a package.
//
// # Additive and time-bounded, not drop-and-rebuild
//
// The obvious design is to empty the index and rebuild it, mirroring RebuildKNIndexes. It is
// wrong here, and the two look alike enough that the reasons are worth keeping:
//
//   - RebuildKNIndexes is derivable, deterministic, and completes or rolls back inside ONE
//     transaction. This walk spans hours and REST calls and cannot be atomic. Copying the
//     shape of an atomic rebuild onto a non-atomic one is the mistake.
//   - An interruption after a drop leaves every unwalked channel with LESS than it started
//     with, because it loses the correct post-fix data live traffic has been writing since.
//     The status quo is thin but never wrong.
//   - A drop destroys data from messages Discord can no longer return: deleted messages,
//     deleted channels, channels the bot has lost read access to. Those are not re-derivable
//     at all.
//   - It makes the bot visibly worse for the whole duration, in exactly the direction the
//     repair exists to improve, during the hours somebody is watching.
//
// Only messages older than the fix need repairing, so walking exactly those double-counts
// nothing, needs no drop, and makes an interruption monotone progress.
//
// # What it reuses
//
// internal/ingest is already a seam: Session, Cursors and Learner are interfaces. So this is
// that walk with different adapters and a stop bound, not a second paging implementation.
package repair

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/ingest"
	"github.com/6586x57890143/peregrine/internal/learn"
	"github.com/6586x57890143/peregrine/internal/names"
	"github.com/6586x57890143/peregrine/internal/storage"
)

// memberCacheTTL and memberCacheSize bound the guild-member cache a pass uses.
//
// Generous on both, because a walk applies today's nicknames to old messages regardless, so
// freshness is already approximate here in a way it is not on the live path. Constants rather
// than configuration for the same reason as the concurrency dials: no incident changes them.
const (
	memberCacheTTL  = 6 * time.Hour
	memberCacheSize = 4096
)

// Options are the dials.
type Options struct {
	// Enabled names the jobs to run. Empty runs none, which is the default: a pass that
	// re-reads all of history is a decision an operator makes rather than one a deploy makes
	// for them.
	Enabled []string

	// Override is the boundary for jobs whose generation predates generation stamping.
	//
	// It exists for exactly one case and should not grow a second: generation 2 shipped in
	// M14 without a stamp, so its real start instant is unrecoverable. Every generation from
	// 3 onwards reads its boundary out of the corpus and ignores this.
	Override time.Time

	// GuildConcurrency, ChannelConcurrency and BatchDelay are deliberately gentler than the
	// live ingest pass. This walk has no deadline and the bot does, so it yields REST budget
	// rather than competing for it. bbolt serializes writers, but both passes write one small
	// transaction per message, so the contention that matters is Discord's rate limiter.
	GuildConcurrency   int
	ChannelConcurrency int
	BatchDelay         time.Duration

	// Retry is how often to look again after an interrupted pass. A RESUME timer, not a
	// schedule: a job already marked done returns immediately on every tick thereafter.
	Retry time.Duration
}

// Service runs the enabled jobs.
type Service struct {
	session *discordgo.Session
	store   *storage.Store
	learner *learn.Learner
	opts    Options
	logger  *slog.Logger

	loops       sync.WaitGroup
	cancelLoops context.CancelFunc
}

// New builds the service, taking the shared Learner for the same reason the ingest plugin
// does: the corpus writer is one per process, and a second one would carry its own bot ID that
// nothing ever sets.
func New(session *discordgo.Session, store *storage.Store, learner *learn.Learner, opts Options) *Service {
	return &Service{session: session, store: store, learner: learner, opts: opts}
}

func (s *Service) Name() string { return "repair" }

// Init records the logger. State lives in the corpus and is read per pass.
func (s *Service) Init(deps core.Deps) error {
	s.logger = deps.Logger
	return nil
}

// Start launches the pass.
//
// core.RunLoop rather than a bare goroutine even though these are one-shots: it buys panic
// isolation for a walk that runs for hours, context binding, and the shutdown wait, all of
// which a hand-rolled goroutine would reimplement. RunLoop refuses a non-positive interval, so
// Retry is a real duration.
func (s *Service) Start(ctx context.Context) error {
	if len(s.opts.Enabled) == 0 {
		return nil
	}

	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelLoops = cancel

	core.RunLoop(loopCtx, &s.loops, s.logger, core.Loop{
		Name:      "repair",
		Every:     s.opts.Retry,
		Immediate: true,
		Fn:        func(ctx context.Context) { s.Once(ctx) },
	})
	return nil
}

// Shutdown stops the loop and waits for a pass in flight.
func (s *Service) Shutdown(ctx context.Context) error {
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
		// Expected rather than alarming: the walk is long by design and the shutdown budget
		// is eight seconds. Its cursors are what make an interrupted pass resumable.
		s.logger.Info("shutdown deadline reached with a repair still running; " +
			"it resumes from its own cursors on the next start")
	}
	return nil
}

// Once runs every enabled job that is not already done.
func (s *Service) Once(ctx context.Context) {
	for _, job := range s.pending() {
		if ctx.Err() != nil {
			return
		}
		s.run(ctx, job)
	}
}

// pending returns the enabled jobs the corpus has not recorded as finished.
func (s *Service) pending() []Job {
	enabled := map[string]bool{}
	all := false
	for _, name := range s.opts.Enabled {
		if name == AllJobs {
			all = true
		}
		enabled[name] = true
	}

	var out []Job
	if err := s.store.View(func(r *storage.Reader) error {
		for _, job := range jobs(s.learner) {
			if !all && !enabled[job.Name] {
				continue
			}
			if r.RepairStateOf(job.Name) == storage.RepairDone {
				continue
			}
			out = append(out, job)
		}
		return nil
	}); err != nil {
		s.logger.Error("reading repair state failed", "err", err)
		return nil
	}
	return out
}

// run performs one job.
func (s *Service) run(ctx context.Context, job Job) {
	boundary, err := s.boundary(job)
	if err != nil {
		// Declining is the safe direction. A job with no boundary would either walk
		// everything, re-counting data the fixed writer already wrote, or walk nothing while
		// reporting success.
		s.logger.Error("skipping repair: no boundary", "job", job.Name, "err", err)
		return
	}

	if err := s.store.Update(func(w *storage.Writer) error {
		return w.SetRepairState(job.Name, storage.RepairRunning)
	}); err != nil {
		s.logger.Error("marking repair running failed", "job", job.Name, "err", err)
		return
	}

	s.logger.Info("starting a repair",
		"job", job.Name, "why", job.Why,
		"before", boundary.Format(time.RFC3339))
	start := time.Now()

	// One cache per pass, shared by every channel worker. See names.NewCachedSession for why
	// this is a prerequisite rather than an optimization on a full-history walk.
	cache := names.NewCachedSession(s.session, memberCacheTTL, memberCacheSize)

	in := ingest.New(
		s.session,
		cursors{store: s.store, job: job.Name},
		learner{session: cache, store: s.store, job: job},
		s.logger,
		ingest.Options{
			// From the beginning of time. SnowflakeAt clamps below the Discord epoch, which
			// is what that clamp was written for, so an absurd lookback says "everything"
			// without inventing a second mechanism for it.
			Lookback:           100 * 365 * 24 * time.Hour,
			Until:              boundary,
			GuildConcurrency:   s.opts.GuildConcurrency,
			ChannelConcurrency: s.opts.ChannelConcurrency,
			BatchDelay:         s.opts.BatchDelay,
		})

	stats, err := in.Run(ctx)
	if err != nil {
		s.logger.Error("repair pass failed, will resume", "job", job.Name, "err", err)
		return
	}

	if ctx.Err() != nil {
		s.logger.Info("repair interrupted, will resume from its cursors",
			"job", job.Name, "repaired", stats.Learned, "took", time.Since(start))
		return
	}

	if err := s.store.Update(func(w *storage.Writer) error {
		return w.SetRepairState(job.Name, storage.RepairDone)
	}); err != nil {
		s.logger.Error("marking repair done failed", "job", job.Name, "err", err)
		return
	}

	s.logger.Info("repair finished",
		"job", job.Name,
		"guilds", stats.Guilds, "channels", stats.Channels,
		"repaired", stats.Learned, "skipped", stats.Skipped, "errors", stats.Errors,
		"requests_saved", names.CacheHits(cache), "took", time.Since(start))
}

// boundary answers "when did the fix land", preferring what the corpus recorded.
//
// The stamp is the right answer and the override is the fallback, not the other way round: an
// operator's memory of a deploy time is what this whole mechanism exists to stop depending on.
func (s *Service) boundary(job Job) (time.Time, error) {
	var (
		at    time.Time
		known bool
	)
	if err := s.store.View(func(r *storage.Reader) error {
		at, known = r.LearnGenerationStart(job.FixedIn)
		return nil
	}); err != nil {
		return time.Time{}, err
	}
	if known {
		return at, nil
	}
	if !s.opts.Override.IsZero() {
		return s.opts.Override, nil
	}
	return time.Time{}, fmt.Errorf(
		"the corpus has no record of when learn generation %d began, and no override is set: "+
			"that generation shipped before generation stamping existed, so set "+
			"PEREGRINE_REPAIR_BEFORE to the instant it deployed", job.FixedIn)
}

// learner adapts one job to ingest.Learner.
//
// The whole difference between a repair and the live pass is which function this calls: the
// live adapter calls Learner.Message, which writes n-grams, history, stats and counters, and
// re-reading history through it would count every n-gram a second time (finding 13).
type learner struct {
	session names.Session
	store   *storage.Store
	job     Job
}

func (l learner) Learn(m *discordgo.Message, guildID string) error {
	mentioned := names.OfMessage(l.session, l.store, &discordgo.MessageCreate{Message: m}, guildID)
	author := names.Primary(m.Author, m.Member)

	return l.store.Update(func(w *storage.Writer) error {
		return l.job.Apply(w, m, author, mentioned)
	})
}

// cursors adapts one job's marks to ingest.Cursors, namespaced by job name.
type cursors struct {
	store *storage.Store
	job   string
}

func (c cursors) Cursor(channelID string) (string, error) {
	var id string
	err := c.store.View(func(r *storage.Reader) error {
		id = r.RepairCursor(c.job, channelID)
		return nil
	})
	return id, err
}

func (c cursors) SetCursor(channelID, messageID string) error {
	return c.store.Update(func(w *storage.Writer) error {
		return w.SetRepairCursor(c.job, channelID, messageID)
	})
}
