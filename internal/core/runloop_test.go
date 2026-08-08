package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunLoopTicks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup

	ticks := make(chan struct{}, 8)
	RunLoop(ctx, &wg, quietLogger(), Loop{
		Name:  "test",
		Every: time.Millisecond,
		Fn:    func(context.Context) { ticks <- struct{}{} },
	})

	for range 3 {
		select {
		case <-ticks:
		case <-time.After(2 * time.Second):
			t.Fatal("loop did not tick")
		}
	}
	cancel()
	wg.Wait()
}

// TestRunLoopImmediate covers the distinction the nine hand-rolled loops made by
// hand and inconsistently: the status line and the clustering pass are wanted at
// startup, a leaderboard reset check on a fresh process is not.
func TestRunLoopImmediate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup

	ran := make(chan struct{}, 1)
	RunLoop(ctx, &wg, quietLogger(), Loop{
		Name:      "test",
		Every:     time.Hour, // far beyond the test, so only Immediate can fire
		Immediate: true,
		Fn:        func(context.Context) { ran <- struct{}{} },
	})

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("Immediate did not run before the first tick")
	}
	cancel()
	wg.Wait()
}

func TestRunLoopWithoutImmediateDoesNotRunAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup

	var ran atomic.Int64
	RunLoop(ctx, &wg, quietLogger(), Loop{
		Name:  "test",
		Every: time.Hour,
		Fn:    func(context.Context) { ran.Add(1) },
	})

	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()

	if got := ran.Load(); got != 0 {
		t.Errorf("loop ran %d times without Immediate, want 0", got)
	}
}

// TestRunLoopSurvivesAPanic pins the rule that one optional behavior failing
// disables that behavior and nothing else. Every one of these loops is optional;
// under the old hand-rolled shape a panic in any of them killed the whole bot.
func TestRunLoopSurvivesAPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup

	var calls atomic.Int64
	recovered := make(chan struct{}, 1)
	RunLoop(ctx, &wg, quietLogger(), Loop{
		Name:  "panicky",
		Every: time.Millisecond,
		Fn: func(context.Context) {
			if calls.Add(1) == 1 {
				panic("boom")
			}
			select {
			case recovered <- struct{}{}:
			default:
			}
		},
	})

	select {
	case <-recovered:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not continue after a panicking iteration")
	}
	cancel()
	wg.Wait()
}

// TestRunLoopStopsOnContextCancel is what makes a bounded shutdown possible: a
// service waits on this WaitGroup, so a loop mid-write finishes before the corpus
// is closed under it.
func TestRunLoopStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup

	RunLoop(ctx, &wg, quietLogger(), Loop{
		Name:  "test",
		Every: time.Millisecond,
		Fn:    func(context.Context) {},
	})

	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not exit on context cancellation")
	}
}

// TestRunLoopRefusesNonPositiveInterval pins that this reports rather than
// panics. time.NewTicker panics on a non-positive duration, which would take the
// process down at startup with a stack trace instead of a message naming the loop.
func TestRunLoopRefusesNonPositiveInterval(t *testing.T) {
	var wg sync.WaitGroup
	var ran atomic.Int64

	for _, every := range []time.Duration{0, -time.Second} {
		RunLoop(t.Context(), &wg, quietLogger(), Loop{
			Name:  "bad",
			Every: every,
			Fn:    func(context.Context) { ran.Add(1) },
		})
	}

	// Nothing was started, so this must not block.
	wg.Wait()
	if got := ran.Load(); got != 0 {
		t.Errorf("a loop with a non-positive interval ran %d times, want 0", got)
	}
}
