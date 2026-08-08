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
	"syscall"

	"github.com/joho/godotenv"

	"github.com/6586x57890143/peregrine/internal/legacy"
)

func main() {
	// A LevelVar rather than a fixed level so M2 can move the level from config
	// without invalidating a logger that has already been handed out.
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

	if err := runGuarded(log); err != nil {
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
func runGuarded(log *slog.Logger) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic", "value", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return run(log)
}

func run(log *slog.Logger) error {
	cleanDB := flag.Bool("clean-db", false, "Remove spammy and slur-bearing keys from the corpus, then exit. Never touches Discord.")
	flag.Parse()

	// Maintenance modes run against the corpus and exit. They deliberately do
	// not open a Discord session, need no token, and must not be reachable by
	// accident: bbolt holds an exclusive flock, so this fails within five
	// seconds against a live bot rather than hanging.
	if *cleanDB {
		log.Info("running maintenance mode", "mode", "clean-db")
		return legacy.CleanDatabase()
	}

	// SIGTERM is the one that matters in production: it is what `docker stop`
	// sends, and the container gets ten seconds before SIGKILL. Under the old
	// signal.Notify the same handling existed but lived 300 lines into main();
	// here the cancellation is owned by the entrypoint and Run just waits on it,
	// which is what lets a test drive a shutdown without a signal.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := legacy.Run(ctx); err != nil {
		return err
	}

	// Distinguish a clean shutdown from a Run that returned nil for some other
	// reason. Only the former is expected.
	if !errors.Is(ctx.Err(), context.Canceled) {
		log.Warn("bot stopped without a shutdown signal")
	}
	return nil
}
