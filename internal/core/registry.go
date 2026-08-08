// Package core owns process lifecycle: the Discord session, the readiness check,
// the bounded work queue that every incoming message goes through, the ticker
// helper that replaced nine hand-rolled goroutines, and the registry that starts
// and stops everything in a defined order.
//
// It knows nothing about what peregrine does. Nothing here imports a feature
// package, and nothing here may: services import core, and registration happens
// only in cmd/bot (SPEC.md section 2).
package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.etcd.io/bbolt"

	"github.com/6586x57890143/peregrine/internal/config"
)

// Service is the interface every feature module implements. Services are
// compiled in and registered at startup, never loaded dynamically: the
// modularity is for blast-radius containment, not runtime extensibility.
//
// Peregrine has exactly one service today, internal/legacy, and that is the
// point. Each later milestone lifts one subsystem out of legacy into its own
// service registered alongside it, so the registry is load-bearing from the
// first commit rather than a seam waiting for a user.
type Service interface {
	// Name is a stable identifier used in logs and panic attribution.
	Name() string

	// Init runs once, in registration order, before any service starts. The
	// session is not connected yet: no gateway or REST calls in Init.
	Init(deps Deps) error

	// Start runs once, after every Init has succeeded and the gateway is READY.
	// Long-running work is launched here and must respect ctx cancellation.
	Start(ctx context.Context) error

	// Shutdown runs in reverse start order. Must be idempotent and must respect
	// ctx's deadline: the process is on its way out and something is waiting.
	Shutdown(ctx context.Context) error
}

// Deps is the fixed set of shared services injected into every service at Init,
// so nothing reaches for a global.
type Deps struct {
	Session *discordgo.Session
	Config  *config.Config
	Logger  *slog.Logger

	// Store is the raw bbolt handle only until M6, which replaces it with
	// storage.Reader and storage.Writer bound to a transaction. That change is
	// what makes the nested-transaction deadlock in the generation path
	// unwritable rather than merely fixed (SPEC.md section 8, finding 1): with no
	// service able to reach a *bbolt.DB, no service can open a transaction inside
	// another one even by accident.
	Store *bbolt.DB

	// Dispatcher is the only way a gateway handler should do work. See its own
	// documentation for why a handler must not spawn its own goroutine.
	Dispatcher *Dispatcher
}

// Registry owns service lifecycle: Init, Start, and reverse-order Shutdown.
type Registry struct {
	mu       sync.Mutex
	services []Service
	started  []Service
	deps     Deps
	log      *slog.Logger
}

func NewRegistry(deps Deps, log *slog.Logger) *Registry {
	return &Registry{deps: deps, log: log}
}

func (r *Registry) Register(s Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services = append(r.services, s)
}

// InitAll calls Init on every service in registration order, under panic
// recovery. A failure or a panic aborts startup rather than limping on: for this
// bot a half-initialized process presents as a bot that connects, looks healthy,
// and quietly does nothing, which is the hardest failure to notice.
func (r *Registry) InitAll() error {
	for _, s := range r.services {
		if err := safeCall(func() error { return s.Init(r.deps) }); err != nil {
			return fmt.Errorf("init service %q: %w", s.Name(), err)
		}
	}
	return nil
}

// startupFailureShutdownTimeout bounds the ShutdownAll that StartAll calls on its
// own failure path, so a service with a hung Shutdown cannot stop a startup
// failure from ever returning.
const startupFailureShutdownTimeout = 10 * time.Second

// StartAll starts services in registration order. If any Start fails, everything
// already started is shut down in reverse order before returning.
func (r *Registry) StartAll(ctx context.Context) error {
	for _, s := range r.services {
		if err := safeCall(func() error { return s.Start(ctx) }); err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), startupFailureShutdownTimeout)
			r.ShutdownAll(shutdownCtx)
			cancel()
			return fmt.Errorf("start service %q: %w", s.Name(), err)
		}
		r.started = append(r.started, s)
	}
	return nil
}

// ShutdownAll shuts down every started service in reverse start order. Each call
// is panic-isolated, so one broken Shutdown cannot stop the others from running,
// which for peregrine means a panic in some engagement feature cannot prevent the
// corpus from being closed cleanly.
func (r *Registry) ShutdownAll(ctx context.Context) {
	for i := len(r.started) - 1; i >= 0; i-- {
		s := r.started[i]
		if err := safeCall(func() error { return s.Shutdown(ctx) }); err != nil {
			r.log.Error("service shutdown error", "service", s.Name(), "err", err)
		}
	}
	r.started = nil
}

func safeCall(fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	return fn()
}
