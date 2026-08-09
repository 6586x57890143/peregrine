package ingest

import "sync"

// stats accumulates counts across concurrent workers.
//
// A mutex rather than four atomics, because the four numbers are read together for one
// log line and a torn read across them would report a pass that never happened. The lock
// is held for four additions per channel, which is nothing next to the REST call that
// produced them.
//
// It exists at all because the fan-out is bounded but still concurrent: without it the
// counters would be a data race, which is the class of bug M3 spent a milestone removing
// and which the race detector in CI would catch on the first pass.
type stats struct {
	mu sync.Mutex
	s  Stats
}

func (a *stats) add(s Stats) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.s.Channels += s.Channels
	a.s.Learned += s.Learned
	a.s.Skipped += s.Skipped
	a.s.Errors += s.Errors
}

func (a *stats) snapshot() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.s
}
