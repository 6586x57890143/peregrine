package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recorder is a Service that appends to a shared trace, so ordering is asserted
// directly rather than inferred.
type recorder struct {
	name     string
	trace    *[]string
	initErr  error
	startErr error
	panicIn  string
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Init(Deps) error {
	*r.trace = append(*r.trace, "init:"+r.name)
	if r.panicIn == "init" {
		panic("boom in init")
	}
	return r.initErr
}

func (r *recorder) Start(context.Context) error {
	*r.trace = append(*r.trace, "start:"+r.name)
	if r.panicIn == "start" {
		panic("boom in start")
	}
	return r.startErr
}

func (r *recorder) Shutdown(context.Context) error {
	*r.trace = append(*r.trace, "shutdown:"+r.name)
	if r.panicIn == "shutdown" {
		panic("boom in shutdown")
	}
	return nil
}

func traceString(trace []string) string { return strings.Join(trace, " ") }

// TestRegistryShutsDownInReverseStartOrder is the property the whole type exists
// for. Later services are built on earlier ones, so tearing down in start order
// would pull the ground out from under something still running.
func TestRegistryShutsDownInReverseStartOrder(t *testing.T) {
	var trace []string
	r := NewRegistry(Deps{}, quietLogger())
	r.Register(&recorder{name: "a", trace: &trace})
	r.Register(&recorder{name: "b", trace: &trace})
	r.Register(&recorder{name: "c", trace: &trace})

	if err := r.InitAll(); err != nil {
		t.Fatalf("InitAll: %v", err)
	}
	if err := r.StartAll(t.Context()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	r.ShutdownAll(t.Context())

	want := "init:a init:b init:c start:a start:b start:c shutdown:c shutdown:b shutdown:a"
	if got := traceString(trace); got != want {
		t.Errorf("lifecycle order wrong\ngot:  %s\nwant: %s", got, want)
	}
}

// TestRegistryInitFailureAbortsStartup pins fail-safe over fail-silent. For this
// bot a half-initialized process presents as one that connects, looks healthy, and
// quietly does nothing, which is the hardest failure to notice.
func TestRegistryInitFailureAbortsStartup(t *testing.T) {
	var trace []string
	r := NewRegistry(Deps{}, quietLogger())
	r.Register(&recorder{name: "a", trace: &trace})
	r.Register(&recorder{name: "b", trace: &trace, initErr: errors.New("nope")})
	r.Register(&recorder{name: "c", trace: &trace})

	err := r.InitAll()
	if err == nil {
		t.Fatal("InitAll must fail when a service's Init fails")
	}
	if !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("error must name the failing service; got %v", err)
	}
	// c must not have been initialized after b failed.
	if got := traceString(trace); got != "init:a init:b" {
		t.Errorf("initialization continued past a failure: %s", got)
	}
}

// TestRegistryStartFailureRollsBack asserts the process never limps along half
// started: everything already up comes back down, in reverse order.
func TestRegistryStartFailureRollsBack(t *testing.T) {
	var trace []string
	r := NewRegistry(Deps{}, quietLogger())
	r.Register(&recorder{name: "a", trace: &trace})
	r.Register(&recorder{name: "b", trace: &trace})
	r.Register(&recorder{name: "c", trace: &trace, startErr: errors.New("nope")})

	if err := r.InitAll(); err != nil {
		t.Fatalf("InitAll: %v", err)
	}
	err := r.StartAll(t.Context())
	if err == nil {
		t.Fatal("StartAll must fail when a service's Start fails")
	}

	// c failed to start so it is not in the started list and must not be shut
	// down; b and a must be, newest first.
	want := "init:a init:b init:c start:a start:b start:c shutdown:b shutdown:a"
	if got := traceString(trace); got != want {
		t.Errorf("rollback order wrong\ngot:  %s\nwant: %s", got, want)
	}
}

// TestRegistryIsolatesPanics covers all three lifecycle phases. A panic in one
// service must be reported as that service's failure, not as a bare runtime trace
// that takes the process down with no attribution.
func TestRegistryIsolatesPanics(t *testing.T) {
	t.Run("init", func(t *testing.T) {
		var trace []string
		r := NewRegistry(Deps{}, quietLogger())
		r.Register(&recorder{name: "boom", trace: &trace, panicIn: "init"})

		err := r.InitAll()
		if err == nil {
			t.Fatal("a panic in Init must surface as an error")
		}
		if !strings.Contains(err.Error(), "panic") || !strings.Contains(err.Error(), `"boom"`) {
			t.Errorf("error must say it was a panic and name the service; got %v", err)
		}
	})

	t.Run("shutdown does not stop the others", func(t *testing.T) {
		var trace []string
		r := NewRegistry(Deps{}, quietLogger())
		r.Register(&recorder{name: "first", trace: &trace})
		r.Register(&recorder{name: "panicky", trace: &trace, panicIn: "shutdown"})

		if err := r.InitAll(); err != nil {
			t.Fatalf("InitAll: %v", err)
		}
		if err := r.StartAll(t.Context()); err != nil {
			t.Fatalf("StartAll: %v", err)
		}
		r.ShutdownAll(t.Context())

		// This is the one that matters operationally: a panic in some engagement
		// feature's Shutdown must not prevent the service that owns the corpus
		// from shutting down cleanly.
		if got := traceString(trace); !strings.HasSuffix(got, "shutdown:panicky shutdown:first") {
			t.Errorf("a panicking Shutdown blocked the remaining services: %s", got)
		}
	})
}

// TestRegistryShutdownIsNotRepeated guards against a double teardown when
// StartAll's rollback path and the normal exit path both run.
func TestRegistryShutdownIsNotRepeated(t *testing.T) {
	var trace []string
	r := NewRegistry(Deps{}, quietLogger())
	r.Register(&recorder{name: "a", trace: &trace})

	if err := r.InitAll(); err != nil {
		t.Fatalf("InitAll: %v", err)
	}
	if err := r.StartAll(t.Context()); err != nil {
		t.Fatalf("StartAll: %v", err)
	}
	r.ShutdownAll(t.Context())
	r.ShutdownAll(t.Context())

	if got := strings.Count(traceString(trace), "shutdown:a"); got != 1 {
		t.Errorf("service was shut down %d times, want 1", got)
	}
}
