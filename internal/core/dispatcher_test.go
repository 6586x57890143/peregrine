package core

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDispatcherRunsSubmittedWork(t *testing.T) {
	ctx := t.Context()

	d := NewDispatcher(2, 8, quietLogger())
	d.Start(ctx)

	var ran atomic.Int64
	done := make(chan struct{}, 5)
	for range 5 {
		if !d.Submit(func(context.Context) {
			ran.Add(1)
			done <- struct{}{}
		}) {
			t.Fatal("Submit refused work on an empty queue")
		}
	}
	for range 5 {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 5 items ran", ran.Load())
		}
	}
}

// TestDispatcherDropsRatherThanBlocks is the pin for the reason Submit is
// non-blocking. discordgo dispatches every gateway event on its own goroutine, so
// blocking here would pile those up without bound and turn a slow database into
// unbounded memory growth.
func TestDispatcherDropsRatherThanBlocks(t *testing.T) {
	ctx := t.Context()

	// One worker, held busy, and a queue of one. The third Submit has nowhere to
	// go and must return false immediately rather than wait.
	d := NewDispatcher(1, 1, quietLogger())

	release := make(chan struct{})
	blocking := make(chan struct{})
	d.Start(ctx)

	if !d.Submit(func(context.Context) {
		close(blocking)
		<-release
	}) {
		t.Fatal("first Submit was refused")
	}
	<-blocking // the only worker is now occupied

	if !d.Submit(func(context.Context) {}) {
		t.Fatal("second Submit should have taken the one queue slot")
	}

	// A blocking implementation would hang here rather than return.
	returned := make(chan bool, 1)
	go func() { returned <- d.Submit(func(context.Context) {}) }()
	select {
	case accepted := <-returned:
		if accepted {
			t.Error("Submit accepted work with a full queue and a busy worker")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit blocked instead of dropping")
	}

	if got := d.Dropped(); got != 1 {
		t.Errorf("Dropped() = %d, want 1: a drop must be counted, not swallowed", got)
	}
	close(release)
}

// TestDispatcherSurvivesAPanickingHandler pins that one bad message cannot
// permanently reduce throughput. A worker dying silently looks like the bot
// getting slower rather than like a bug.
func TestDispatcherSurvivesAPanickingHandler(t *testing.T) {
	ctx := t.Context()

	d := NewDispatcher(1, 4, quietLogger())
	d.Start(ctx)

	d.Submit(func(context.Context) { panic("boom") })

	after := make(chan struct{})
	d.Submit(func(context.Context) { close(after) })

	select {
	case <-after:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker did not survive a panicking handler")
	}
}

// TestDispatcherShutdownDrains asserts queued work still runs on the way out,
// which is the difference between a graceful stop and dropping whatever happened
// to be in flight.
func TestDispatcherShutdownDrains(t *testing.T) {
	// No workers at all, so nothing can run until Shutdown drains the queue
	// itself. That is what makes this test about the drain rather than about
	// timing.
	d := NewDispatcher(0, 8, quietLogger())
	d.Start(t.Context())

	var ran atomic.Int64
	for range 4 {
		d.Submit(func(context.Context) { ran.Add(1) })
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	d.Shutdown(shutdownCtx)

	if got := ran.Load(); got != 4 {
		t.Errorf("%d of 4 queued items ran during shutdown, want all 4", got)
	}
}

// TestDispatcherRefusesWorkAfterShutdown covers the race the old code crashed on:
// a message arriving while shutdown is under way. It must be refused, not
// accepted into a queue nobody will read, and above all it must not panic.
func TestDispatcherRefusesWorkAfterShutdown(t *testing.T) {
	ctx := t.Context()

	d := NewDispatcher(1, 4, quietLogger())
	d.Start(ctx)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	d.Shutdown(shutdownCtx)

	if d.Submit(func(context.Context) { t.Error("work submitted after shutdown must not run") }) {
		t.Error("Submit accepted work after Shutdown")
	}
}

// TestDispatcherConcurrentSubmitDuringShutdownDoesNotPanic is the direct
// regression test for finding 4. The old handler called wg.Add on the same
// WaitGroup the shutdown path waited on, so an Add landing after the counter hit
// zero panicked, taking the process down on its way out. Worth running under
// -race, which CI does.
func TestDispatcherConcurrentSubmitDuringShutdownDoesNotPanic(t *testing.T) {
	ctx := t.Context()

	d := NewDispatcher(4, 16, quietLogger())
	d.Start(ctx)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					d.Submit(func(context.Context) {})
				}
			}
		}()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	d.Shutdown(shutdownCtx)

	close(stop)
	wg.Wait()
}
