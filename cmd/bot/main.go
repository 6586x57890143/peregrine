// Command bot is the peregrine entrypoint: parse flags, load env, wire logging,
// then hand off to the bot or to a maintenance mode.
//
// It is deliberately thin, and mirrors merlin's cmd/bot/main.go so that an
// operator who has debugged one startup has debugged both. Everything it knows
// how to do is: decide which mode to run, make failures visible, and translate
// a signal into a cancelled context. All behavior lives behind internal/.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/legacy"
	"github.com/6586x57890143/peregrine/internal/maintenance"
	"github.com/6586x57890143/peregrine/internal/safety"
	"github.com/6586x57890143/peregrine/internal/storage"
)

func main() {
	// A LevelVar rather than a fixed level so LOG_LEVEL can move the level after
	// the logger has already been handed out. Until config loads, everything logs
	// at Info, which means a configuration failure is always visible regardless
	// of what LOG_LEVEL was going to say.
	level := new(slog.LevelVar)
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// Must precede SetDefault. internal/legacy logs through the stdlib log
	// package at roughly 200 call sites, and SetDefault routes those into this
	// handler; without a level set for that bridge they would all arrive as Info
	// regardless of what the handler is filtering to. Converting those call sites
	// is each milestone's job as it moves its subsystem out of legacy, not a
	// rename's.
	slog.SetLogLoggerLevel(slog.LevelInfo)
	slog.SetDefault(log)

	// Dev convenience only. In the container the environment comes from
	// env_file/Docker, .env does not exist, and a missing file here is not an
	// error worth reporting.
	_ = godotenv.Load()

	if err := runGuarded(log, level); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// runGuarded turns a panic into a logged fatal rather than a bare runtime trace
// on stderr. That matters for this bot specifically: it processes every message
// in its own goroutine and a panic in one of those is unrecoverable from here,
// but a panic on the startup or shutdown path is exactly the kind that otherwise
// leaves nothing in the log an operator actually reads. The trace goes into the
// same structured stream as everything else.
func runGuarded(log *slog.Logger, level *slog.LevelVar) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic", "value", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return run(log, level)
}

func run(log *slog.Logger, level *slog.LevelVar) error {
	// The value is deliberately unused: mode selection is by whether the flag was
	// passed, checked below, so `-clean-db=false` is still a request to clean rather
	// than a request to start the bot. A boolean flag whose false value silently means
	// "do something else entirely" is not a thing an operator can reason about.
	_ = flag.Bool("clean-db", false,
		"Remove spammy and blocklisted n-grams from the corpus, then exit. Never touches Discord.")
	compactTo := flag.String("compact", "",
		"Copy the corpus to this path, reclaiming free pages, then exit. bbolt's file never "+
			"shrinks, so this is the only way to get space back after -clean-db.")
	purgeAuthor := flag.String("purge-author", "",
		"Remove one Discord user ID's contribution to author-diversity counts, then exit. "+
			"The surgical alternative to discarding a corpus one bad actor has poisoned.")
	flag.Parse()

	// Which maintenance flags were PASSED, rather than which have a non-empty value.
	//
	// The difference is not pedantic. `-purge-author ""`, or `-purge-author "$UID"` with
	// UID unset, is a command an operator issues during an incident, and treating the
	// empty value as "no mode requested" made it silently start the bot instead: the
	// most dangerous possible interpretation of a typo, since the whole point of the
	// mode is that it does not touch Discord. Passing the flag now selects the mode, and
	// the empty ID is refused by the mode itself with an explanation.
	passed := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { passed[f.Name] = true })

	cfg, err := config.Load()
	if err != nil {
		// Load reports every problem it found rather than the first, so a
		// multi-variable mistake takes one restart to see instead of one per
		// variable. Unpacked into one log record each: errors.Join separates with
		// newlines, and slog's TextHandler quotes a multi-line value, so logging
		// the joined error directly produces a single line with literal \n
		// sequences in it, which is exactly the thing an operator has to read
		// carefully at the worst moment.
		problems := unwrapJoined(err)
		for _, p := range problems {
			log.Error("invalid configuration", "err", p)
		}
		return fmt.Errorf("configuration is invalid: %d problem(s) reported above", len(problems))
	}
	level.Set(cfg.Level())

	// A variable that is documented but not yet read is indistinguishable from
	// one the bot is ignoring, so say so rather than leaving the operator to
	// conclude configuration does not work.
	if deferred := config.DeferredSet(); len(deferred) > 0 {
		log.Warn("these variables are set but not read yet; the milestone that reads each one is in brackets",
			"vars", strings.Join(deferred, ", "))
	}

	// Maintenance modes run against the corpus and exit. They deliberately do
	// not open a Discord session and need no token, which is why config.Load does
	// not require one: an operator cleaning a poisoned corpus should not need a
	// live credential to do it. bbolt holds an exclusive flock, so these fail
	// within five seconds against a live bot rather than hanging.
	if passed["clean-db"] || passed["compact"] || passed["purge-author"] {
		return runMaintenance(cfg, log, passed, *compactTo, *purgeAuthor)
	}

	// SIGTERM is the one that matters in production: it is what `docker stop`
	// sends, and the container gets ten seconds before SIGKILL, which is where the
	// shutdown budget below comes from.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cfg.RequireToken(); err != nil {
		return err
	}

	// The corpus is opened here, and closed here, LAST. It used to be a defer
	// inside the same function that spawned the message goroutines, so shutdown
	// raced them: a handler still in flight could reach a closed database. Owning
	// the open and the close at the outermost level is what makes the ordering
	// below statable at all.
	//
	// storage.Open also refuses a corpus whose schema_version it does not
	// recognize, which is what turns "the layout changed in M6" from silently
	// reading garbage into a startup error that says to start the corpus over.
	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Error("closing corpus", "err", err)
		}
	}()

	// The safety gate, built before the session so a bad ruleset stops the process
	// before it can connect to anything.
	gate, err := buildGate(cfg, log)
	if err != nil {
		return err
	}
	if cfg.PauseAllWrites {
		log.Warn("PEREGRINE_PAUSE_ALL_WRITES is set: every outbound message is refused process-wide. " +
			"Reading and learning continue.")
	}

	session, err := core.NewSession(cfg.Token)
	if err != nil {
		return err
	}

	dispatcher := core.NewDispatcher(cfg.MessageWorkers, cfg.MessageQueue, log)
	registry := core.NewRegistry(core.Deps{
		Session:    session,
		Config:     cfg,
		Logger:     log,
		Store:      store,
		Dispatcher: dispatcher,
		Gate:       gate,
	}, log)
	registry.Register(legacy.New())

	if err := registry.InitAll(); err != nil {
		return err
	}

	// Armed before Open, which is where discordgo starts dispatching. Open only
	// means the identify was sent, never that Discord accepted it: a rejected
	// identify leaves discordgo reconnecting in a loop while startup carries on
	// and logs success. See core.WatchReady.
	awaitReady := core.WatchReady(session)

	if err := session.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}
	if err := awaitReady(); err != nil {
		// Close the half-open session before returning, or the process exits with
		// discordgo's reconnect loop still running.
		if closeErr := session.Close(); closeErr != nil {
			log.Error("closing session after failed readiness check", "err", closeErr)
		}
		return err
	}

	// Workers start before services, so a message that arrives the instant a
	// service registers its handler has somewhere to go.
	dispatcher.Start(ctx)

	if err := registry.StartAll(ctx); err != nil {
		return err
	}

	<-ctx.Done()
	log.Info("shutting down")

	// The order matters and each step is here because the old code got it wrong.
	//
	//  1. Close the session FIRST, to stop the inflow. Draining while Discord is
	//     still delivering messages is a race against a live gateway.
	//  2. Drain the dispatcher, so work already accepted finishes rather than
	//     being dropped on the floor.
	//  3. Shut services down in reverse start order, which is where the loops get
	//     stopped and waited for.
	//  4. Close the store, via the defer above, strictly after every writer has
	//     stopped.
	//
	// One budget covers steps 2 and 3 together, because the container's ten
	// seconds is the real constraint and giving each step its own full timeout
	// would let them add up past it.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer cancel()

	if err := session.Close(); err != nil {
		log.Error("closing discord session", "err", err)
	}
	dispatcher.Shutdown(shutdownCtx)
	registry.ShutdownAll(shutdownCtx)

	log.Info("shutdown complete", "dropped_events", dispatcher.Dropped())
	return nil
}

// runMaintenance owns the modes that operate on the corpus and never touch Discord.
//
// It opens and closes the store itself, so the bot path's ordering is not entangled
// with a mode that exits immediately. All three take the already-open store rather
// than a path, because two code paths independently resolving PEREGRINE_DB_PATH is
// how you clean a database that is not the one the bot uses, which succeeds, reports
// a tidy summary, and changes nothing the operator cares about.
//
// More than one mode at a time is refused rather than ordered. Compacting a corpus
// that a clean pass in the same invocation has just emptied is a plausible thing to
// want and an ambiguous thing to write, so the operator states the sequence.
func runMaintenance(cfg *config.Config, log *slog.Logger, passed map[string]bool, compactTo, purgeAuthor string) error {
	modes := 0
	for _, name := range []string{"clean-db", "compact", "purge-author"} {
		if passed[name] {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("pass one maintenance mode at a time: -clean-db, -compact and " +
			"-purge-author each take the whole corpus, and the order they should run in is " +
			"the operator's decision, not a default")
	}

	// Checked before the corpus is opened, so an obviously incomplete command fails
	// without taking bbolt's exclusive flock on the way.
	if passed["compact"] && compactTo == "" {
		return errors.New("-compact needs a destination path. It writes a new file rather than " +
			"replacing the corpus in place, so that a compaction that goes wrong costs nothing")
	}

	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Error("closing corpus", "err", err)
		}
	}()

	switch {
	case passed["clean-db"]:
		// The clean pass builds the gate, so it removes what the OPERATOR's blocklist
		// covers and not merely the built-in baseline. That is most of the point:
		// adding a pattern to the blocklist does not retroact, and this is the only
		// thing that applies it to what is already in the corpus.
		gate, err := buildGate(cfg, log)
		if err != nil {
			return err
		}
		log.Info("running maintenance mode", "mode", "clean-db", "corpus", cfg.DBPath)
		res, err := maintenance.Clean(store, gate, log)
		if err != nil {
			return err
		}
		if res.Removed > 0 {
			log.Info("the corpus file has not shrunk; bbolt frees pages for reuse but never "+
				"returns them to the filesystem", "next", "-compact <path>")
		}
		return nil

	case compactTo != "":
		log.Info("running maintenance mode", "mode", "compact", "corpus", cfg.DBPath, "destination", compactTo)
		return maintenance.Compact(store, compactTo, log)

	default:
		log.Info("running maintenance mode", "mode", "purge-author", "corpus", cfg.DBPath, "author", purgeAuthor)
		_, err := maintenance.PurgeAuthor(store, purgeAuthor, log)
		return err
	}
}

// shutdownBudget is deliberately under Docker's default ten seconds between
// SIGTERM and SIGKILL. Exceeding it means the corpus is closed by the process
// being killed rather than by us, which is the one outcome worth engineering
// against: bbolt survives it, but the final leaderboard write does not.
const shutdownBudget = 8 * time.Second

// buildGate loads the blocklist and constructs the safety gate.
//
// A configured path that fails to load is FATAL, and that is the whole design.
// Continuing with an empty ruleset would mean running with fewer rules than the
// operator believes, and an incomplete blocklist is indistinguishable from a
// working one right up until the bot posts something that has to be answered for.
// So a missing file, an unreadable file, a malformed line, an uncompilable pattern
// and an empty file all stop the process here.
//
// An UNSET path is allowed, and that is a deliberate asymmetry rather than a hole.
// A developer running against a scratch corpus should not have to invent a slur
// list before the bot will start, and the built-in baseline in internal/filter
// still applies either way. It is loud about it: running with no operator list in a
// hostile channel is a choice, and this makes it a visible one.
func buildGate(cfg *config.Config, log *slog.Logger) (*safety.Gate, error) {
	if cfg.BlocklistPath == "" {
		log.Warn("PEREGRINE_BLOCKLIST_PATH is not set: running with the built-in baseline only. " +
			"The operator blocklist is where threat and illegal-content patterns live, and it is " +
			"the only part of the ruleset that can be edited without a deploy. Do not run in a " +
			"hostile channel like this.")
		return safety.NewGate(nil, log, cfg.PauseAllWrites), nil
	}

	blocklist, err := safety.LoadBlocklist(cfg.BlocklistPath)
	if err != nil {
		// One record per problem, same reason as the configuration errors above:
		// slog quotes a multi-line value, and an operator fixing a list mid-incident
		// should see every bad line at once.
		problems := unwrapJoined(err)
		for _, p := range problems {
			log.Error("blocklist", "err", p)
		}
		return nil, fmt.Errorf("blocklist at %s is unusable (%d problem(s) reported above): refusing to "+
			"start with an incomplete ruleset, because that is indistinguishable from a working one "+
			"until it is too late", cfg.BlocklistPath, len(problems))
	}

	counts := blocklist.CountByCategory()
	log.Info("blocklist loaded",
		"path", cfg.BlocklistPath,
		"rules", blocklist.Len(),
		"slur", counts[safety.CategorySlur],
		"illegal", counts[safety.CategoryIllegal],
		"spam", counts[safety.CategorySpam])
	if counts[safety.CategoryIllegal] == 0 {
		log.Warn("the blocklist has no illegal-category rules, so nothing pages the operator. " +
			"That category is for content where the exposure is legal rather than reputational.")
	}

	return safety.NewGate(blocklist, log, cfg.PauseAllWrites), nil
}

// unwrapJoined flattens an errors.Join into its parts so each can be logged
// separately. Returns a one-element slice for anything else, so the caller never
// has to branch.
func unwrapJoined(err error) []error {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		if parts := joined.Unwrap(); len(parts) > 0 {
			return parts
		}
	}
	return []error{err}
}
