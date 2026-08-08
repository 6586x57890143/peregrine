package core

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Loop describes one recurring background job.
type Loop struct {
	// Name appears in logs and in panic attribution. Required.
	Name string

	// Every is the interval between runs. Required and must be positive.
	Every time.Duration

	// Immediate runs Fn once before the first tick. Two of the loops this
	// replaced did that by hand and the rest did not, which was a real
	// distinction worth keeping: the status line and the clustering pass are
	// wanted at startup, whereas a leaderboard reset check on a fresh process is
	// pure noise.
	Immediate bool

	// Fn is the work. It is called with the loop's context and should return when
	// that context is cancelled.
	Fn func(context.Context)
}

// RunLoop starts l on its own goroutine and returns immediately. The goroutine
// exits when ctx is cancelled.
//
// This replaces nine near-identical hand-rolled ticker goroutines in the old
// main(), each of which built its own time.Ticker, selected on a shared
// stopSignal channel, and got the same three lines subtly differently. Beyond the
// duplication, that shape had two problems this fixes:
//
//   - A panic inside any of them killed the process. Every one of those loops is
//     an optional behavior (a status line, a clustering pass, a leaderboard reset
//     check), and this bot's rule is that one feature failing disables that
//     feature and nothing else. Each iteration is now panic-isolated.
//   - Each Add happened at startup on the main goroutine, which was fine, but the
//     same WaitGroup was also being Added to from the per-message handler, which
//     was not. The Dispatcher now owns that half; wg here is only ever touched
//     before any event can arrive.
//
// wg is what lets a service wait for its own loops during Shutdown, so a loop
// that is mid-write when the process exits finishes before the corpus closes.
func RunLoop(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger, l Loop) {
	if l.Every <= 0 {
		// Refusing rather than defaulting: time.NewTicker panics on a
		// non-positive duration, and a loop that silently never runs is worse
		// than one that says it was misconfigured. config validates every
		// interval, so reaching this means a caller passed a computed value.
		log.Error("refusing to start loop with a non-positive interval", "loop", l.Name, "every", l.Every)
		return
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		if l.Immediate {
			runIteration(ctx, log, l)
		}

		ticker := time.NewTicker(l.Every)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				runIteration(ctx, log, l)
			case <-ctx.Done():
				log.Debug("loop stopped", "loop", l.Name)
				return
			}
		}
	}()
}

func runIteration(ctx context.Context, log *slog.Logger, l Loop) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Error("panic in background loop, loop continues", "loop", l.Name, "value", rec)
		}
	}()
	l.Fn(ctx)
}
