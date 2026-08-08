package core

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Dispatcher is a bounded worker pool for gateway events.
//
// It exists because of a specific crash. The old messageCreate handler called
// wg.Add(1) on the same sync.WaitGroup the shutdown path called wg.Wait() on, and
// then spawned a goroutine per message. Two bugs followed from that shape:
//
//   - An Add that lands after the counter has reached zero panics. Shutdown
//     closed the stop channel and waited, and any message arriving in that window
//     could take the process down on its way out (SPEC.md section 8, finding 4).
//   - Even without the panic, wg.Wait returning did not mean handlers were done:
//     the ones still in flight kept using the database and the session after
//     shutdown had moved on to closing both.
//
// And one cost: goroutines per message is unbounded, so a busy channel or a
// coordinated flood grows the process without limit while every one of those
// goroutines contends for bbolt's single write lock.
//
// The fix has three parts. Work goes on a buffered channel instead of a new
// goroutine. A fixed pool of workers reads it, so concurrency is capped at
// something bbolt can actually absorb. And the WaitGroup is only ever Added to
// during Start, never from a handler, so the panic cannot recur by construction.
type Dispatcher struct {
	queue   chan func(context.Context)
	wg      sync.WaitGroup
	log     *slog.Logger
	workers int

	// dropped counts events refused because the queue was full. Surfaced rather
	// than swallowed: a persistently full queue is a real operational signal and
	// the alternative is inferring it from the bot feeling unresponsive.
	dropped atomic.Uint64

	// closing is set before the drain begins, so an enqueue racing shutdown is
	// refused rather than accepted into a queue nobody will read. The queue itself
	// is never closed, because closing it would let an in-flight send panic.
	closing atomic.Bool

	// cancelWorkers stops the pool. The Dispatcher owns this rather than relying
	// on the context passed to Start, because Shutdown has to be able to stop the
	// workers itself: if it could only wait for someone else to cancel, then
	// calling Shutdown with a live start context would block for the entire
	// deadline every time and the caller would have to know to cancel in exactly
	// the right order. Owning it makes Shutdown correct in isolation.
	mu            sync.Mutex
	cancelWorkers context.CancelFunc
}

func NewDispatcher(workers, queueSize int, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		queue:   make(chan func(context.Context), queueSize),
		log:     log,
		workers: workers,
	}
}

// Start launches the workers. Every wg.Add in this type happens here, on one
// goroutine, before any event can arrive. That is the property that makes the
// panic in finding 4 impossible rather than unlikely.
//
// Workers run under a context derived from ctx, so they stop either when the
// caller cancels or when Shutdown does.
func (d *Dispatcher) Start(ctx context.Context) {
	workerCtx, cancel := context.WithCancel(ctx)

	d.mu.Lock()
	d.cancelWorkers = cancel
	d.mu.Unlock()

	for range d.workers {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			for {
				select {
				case fn := <-d.queue:
					d.run(workerCtx, fn)
				case <-workerCtx.Done():
					return
				}
			}
		}()
	}
}

// run isolates one unit of work. A panic while handling a message must not take
// down a worker, because a worker dying silently reduces throughput permanently
// and looks like the bot getting slower rather than like a bug.
func (d *Dispatcher) run(ctx context.Context, fn func(context.Context)) {
	defer func() {
		if rec := recover(); rec != nil {
			d.log.Error("panic handling event", "value", rec)
		}
	}()
	fn(ctx)
}

// Submit enqueues work without blocking, reporting whether it was accepted.
//
// Non-blocking is deliberate, not a shortcut. discordgo dispatches each gateway
// event on its own goroutine, so blocking here would pile those up without bound
// and turn a slow database into unbounded memory growth. Dropping with a counter
// is the honest semantics for best-effort chat: a message the bot fails to learn
// from is a rounding error, and a message it fails to reply to is invisible next
// to a process that has run out of memory.
func (d *Dispatcher) Submit(fn func(context.Context)) bool {
	if d.closing.Load() {
		d.dropped.Add(1)
		return false
	}
	select {
	case d.queue <- fn:
		return true
	default:
		d.dropped.Add(1)
		return false
	}
}

// Dropped reports how many events were refused. Read by the status line so a
// full queue is visible rather than inferred.
func (d *Dispatcher) Dropped() uint64 { return d.dropped.Load() }

// Queued reports the current queue depth, for the same reason.
func (d *Dispatcher) Queued() int { return len(d.queue) }

// Shutdown stops accepting work, drains what is already queued, stops the
// workers, and waits for the item each may still be running. Everything after
// "stops accepting" is bounded by ctx.
//
// The queue is deliberately never closed. Closing it would make an enqueue that
// is already in flight panic on send, and the whole point of this type is that
// shutdown cannot be crashed by a message that arrived at an inconvenient moment.
//
// Draining is best-effort by design: dropping the tail of the queue is strictly
// better than missing the container's SIGKILL and losing an orderly close of the
// corpus. Idempotent, so a Shutdown from the registry's startup-failure path and
// one from the normal exit path cannot conflict.
func (d *Dispatcher) Shutdown(ctx context.Context) {
	d.closing.Store(true)
	d.drain(ctx)

	// Stop the workers before waiting on them. Waiting first would block for the
	// whole deadline whenever the start context is still live, which is the normal
	// case when Shutdown is called from a service's own Shutdown rather than from
	// a signal handler.
	d.mu.Lock()
	cancel := d.cancelWorkers
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		d.log.Warn("shutdown deadline reached with workers still running")
	}
}

// drain runs queued work in the calling goroutine until the queue is empty or ctx
// expires. Done here rather than left to the workers because the workers are
// about to be cancelled, and this way the drain is bounded by the caller's
// deadline and nothing else.
func (d *Dispatcher) drain(ctx context.Context) {
	for {
		select {
		case fn := <-d.queue:
			d.run(ctx, fn)
		case <-ctx.Done():
			if remaining := len(d.queue); remaining > 0 {
				d.log.Warn("shutdown deadline reached with work still queued",
					"remaining", remaining, "dropped_total", d.dropped.Load())
			}
			return
		default:
			return
		}
	}
}
