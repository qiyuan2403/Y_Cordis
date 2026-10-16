// Package cordis re-implements, in Go, the plugin-kernel semantics the DSH
// harness depends on from its vendored Cordis framework: a service registry
// with ancestor-scoped, isolation-labelled resolution, declared injection with
// reactive epochs, reversible effects with LIFO teardown, and five event
// dispatch modes including waterfall delegation.
//
// Behavior source: the official tree at vendor/cordis/src (tag
// dsh-v0.2.1-alpha.4). Where this package differs, the official behavior wins.
//
// PORT CONTRACT (docs/cordis-port-contract.md): the JavaScript kernel's
// reflection machinery — Proxy, `Reflect.get(receiver)`, shadow/receiver
// rebinding, WeakMap-keyed bookkeeping, prototype-chain walking, and
// async-stack splicing — has no Go counterpart and is deliberately NOT ported.
// What IS ported is the semantic contract those mechanisms implement.
package cordis

import "sync"

// State is a plugin fiber's lifecycle position.
//
// Mirrors the official `FiberState` enum exactly, including the numeric order
// (PENDING, LOADING, ACTIVE, FAILED, DISPOSED, UNLOADING), because diagnostics
// and tests compare against it.
type State int

const (
	// Pending: waiting for required services to be provided.
	Pending State = iota
	// Loading: the plugin callback is running.
	Loading
	// Active: loaded and providing services.
	Active
	// Failed: the callback or its config threw.
	Failed
	// Disposed: the fiber was removed and cannot restart.
	Disposed
	// Unloading: disposers are running.
	Unloading
)

func (s State) String() string {
	switch s {
	case Pending:
		return "PENDING"
	case Loading:
		return "LOADING"
	case Active:
		return "ACTIVE"
	case Failed:
		return "FAILED"
	case Disposed:
		return "DISPOSED"
	case Unloading:
		return "UNLOADING"
	default:
		return "UNKNOWN"
	}
}

// Impl is one provided service implementation.
//
// `Fiber` is the owner: the service lives exactly as long as that fiber, and
// only the owner may overwrite the value. `Check` is the optional availability
// predicate a provider supplies to gate its dependents.
type Impl struct {
	Name  string
	Value any
	Fiber *Fiber
	Check func() bool
}

// store is the service table shared by every context in one tree.
//
// The official kernel keeps a single ReflectService whose `store` is keyed by
// an ISOLATION SYMBOL, not by service name: two contexts that isolate the same
// name under different labels see different implementations. Reproducing that
// keying is what makes isolate() work.
type store struct {
	mu    sync.RWMutex
	impls map[string]*Impl
	// listeners fire when a service is provided or withdrawn, so dependent
	// fibers can recompute their epochs.
	onChange []func(names []string)
}

func newStore() *store {
	return &store{impls: map[string]*Impl{}}
}

func (s *store) get(label string) *Impl {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.impls[label]
}

func (s *store) put(label string, impl *Impl) {
	s.mu.Lock()
	s.impls[label] = impl
	s.mu.Unlock()
}

func (s *store) remove(label string) {
	s.mu.Lock()
	delete(s.impls, label)
	s.mu.Unlock()
}

func (s *store) subscribe(fn func(names []string)) {
	s.mu.Lock()
	s.onChange = append(s.onChange, fn)
	s.mu.Unlock()
}

func (s *store) notify(names []string) {
	s.mu.RLock()
	fns := append([]func([]string){}, s.onChange...)
	s.mu.RUnlock()
	for _, fn := range fns {
		fn(names)
	}
}
