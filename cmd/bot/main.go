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
	"go.etcd.io/bbolt"

	"github.com/6586x57890143/peregrine/internal/config"
	"github.com/6586x57890143/peregrine/internal/core"
	"github.com/6586x57890143/peregrine/internal/legacy"
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
	cleanDB := flag.Bool("clean-db", false, "Remove spammy and slur-bearing keys from the corpus, then exit. Never touches Discord.")
	flag.Parse()

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
	// live credential to do it. bbolt holds an exclusive flock, so this fails
	// within five seconds against a live bot rather than hanging.
	if *cleanDB {
		log.Info("running maintenance mode", "mode", "clean-db", "corpus", cfg.DBPath)
		return legacy.CleanDatabase(cfg.DBPath)
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
	store, err := bbolt.Open(cfg.DBPath, 0600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return fmt.Errorf("open corpus at %s: %w", cfg.DBPath, err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Error("closing corpus", "err", err)
		}
	}()
	if err := legacy.EnsureBuckets(store); err != nil {
		return err
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

// shutdownBudget is deliberately under Docker's default ten seconds between
// SIGTERM and SIGKILL. Exceeding it means the corpus is closed by the process
// being killed rather than by us, which is the one outcome worth engineering
// against: bbolt survives it, but the final leaderboard write does not.
const shutdownBudget = 8 * time.Second

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
