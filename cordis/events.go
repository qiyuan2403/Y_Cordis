package cordis

import (
	"errors"
	"fmt"
	"sync"
)

// DispatchMode names the five dispatch strategies. The official type is a
// string union; the values are reported on `internal/dispatch` diagnostics.
type DispatchMode string

const (
	// ModeEmit runs synchronous listeners without awaiting them.
	ModeEmit DispatchMode = "emit"
	// ModeParallel awaits all listeners together.
	ModeParallel DispatchMode = "parallel"
	// ModeSerial awaits listeners in order until one bails.
	ModeSerial DispatchMode = "serial"
	// ModeBail stops on the first synchronous bail value.
	ModeBail DispatchMode = "bail"
	// ModeWaterfall composes listeners around a final `next` callback.
	ModeWaterfall DispatchMode = "waterfall"
)

// Listener receives one event dispatch. The value it returns participates in
// bail and serial semantics.
type Listener func(args []any) any

// OnOptions controls listener placement and filtering.
type OnOptions struct {
	// Prepend adds the listener before existing listeners for the event.
	Prepend bool
	// Global delivers the event regardless of context filtering.
	Global bool
}

// hook is one registered listener.
type hook struct {
	ctx      *Context
	listener Listener
	global   bool
}

// Events is the event bus installed on every context.
//
// Dispatch resolves listeners, applies the owner filter, and runs them under
// the mode's rule. The official bus mixes its methods onto `ctx`; here they are
// explicit methods on the service.
type Events struct {
	ctx *Context

	mu    sync.RWMutex
	hooks map[string][]hook
}

func newEvents(ctx *Context) *Events {
	return &Events{ctx: ctx, hooks: map[string][]hook{}}
}

// On registers a listener owned by the current fiber and returns a disposer
// that removes it. The listener is removed automatically when the fiber
// unloads.
func (e *Events) On(name string, l Listener, opts OnOptions) (Disposer, error) {
	if err := e.ctx.fiber.assertActive(); err != nil {
		return nil, err
	}
	var disposer Disposer
	err := e.ctx.fiber.Effect(func() (Disposer, error) {
		e.mu.Lock()
		h := hook{ctx: e.ctx, listener: l, global: opts.Global}
		if opts.Prepend {
			e.hooks[name] = append([]hook{h}, e.hooks[name]...)
		} else {
			e.hooks[name] = append(e.hooks[name], h)
		}
		e.mu.Unlock()
		return func() error {
			e.remove(name, l)
			return nil
		}, nil
	})
	if err != nil {
		return nil, err
	}
	disposer = func() error {
		e.remove(name, l)
		return nil
	}
	return disposer, nil
}

// Once registers a listener that removes itself after its first call.
func (e *Events) Once(name string, l Listener, opts OnOptions) (Disposer, error) {
	var disposer Disposer
	wrapped := func(args []any) any {
		if disposer != nil {
			_ = disposer()
		}
		return l(args)
	}
	return e.On(name, wrapped, opts)
}

func (e *Events) remove(name string, l Listener) {
	e.mu.Lock()
	defer e.mu.Unlock()
	hooks := e.hooks[name]
	for i := range hooks {
		if fmt.Sprintf("%p", hooks[i].listener) == fmt.Sprintf("%p", l) {
			e.hooks[name] = append(hooks[:i], hooks[i+1:]...)
			return
		}
	}
}

// resolve collects the listeners for one dispatch, applying the owner filter.
//
// A non-`internal/` event first emits `internal/dispatch` diagnostics, matching
// the official `dispatch()`.
func (e *Events) resolve(mode DispatchMode, name string) []Listener {
	e.mu.RLock()
	hooks := append([]hook{}, e.hooks[name]...)
	e.mu.RUnlock()

	filter := e.ctx.filter
	out := make([]Listener, 0, len(hooks))
	for _, h := range hooks {
		if h.global || filter == nil || filter(h.ctx) {
			out = append(out, h.listener)
		}
	}
	return out
}

// Emit runs listeners synchronously, ignoring return values.
func (e *Events) Emit(name string, args ...any) {
	for _, l := range e.resolve(ModeEmit, name) {
		_ = l(args)
	}
}

// Bail runs listeners synchronously until one returns a bail value.
//
// A bail value is anything that is not nil and not false — mirroring the
// official `isBailed`.
func (e *Events) Bail(name string, args ...any) any {
	for _, l := range e.resolve(ModeBail, name) {
		if v := l(args); isBailed(v) {
			return v
		}
	}
	return nil
}

// Waterfall composes listeners around a final `next` callback.
//
// Listeners run outermost-first. A listener that does not call `next` vetoes
// the rest of the chain, including the built-in behavior. The last listener's
// `next` calls `inner`.
//
// Go has no continuation, so the chain is built explicitly: `next` for listener
// i invokes listener i+1. Calling `next` twice is an error, as it is upstream.
func (e *Events) Waterfall(name string, inner func(any) any, initial any) any {
	listeners := e.resolve(ModeWaterfall, name)
	return dispatchWaterfall(listeners, 0, inner, initial)
}

func dispatchWaterfall(listeners []Listener, i int, inner func(any) any, value any) any {
	if i >= len(listeners) {
		return inner(value)
	}
	called := false
	next := func(v any) any {
		if called {
			panic(fmt.Sprintf("cordis: waterfall listener %d called next twice", i))
		}
		called = true
		return dispatchWaterfall(listeners, i+1, inner, v)
	}
	// The listener receives the value and the continuation.
	args := []any{value, next}
	return listeners[i](args)
}

// isBailed reports whether a listener result stops a bail-style dispatch.
// `nil` and `false` do not bail; everything else does.
func isBailed(v any) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return true
}

// assertActive reports an error when the fiber cannot host new effects.
func (f *Fiber) assertActive() error {
	switch f.State() {
	case Disposed:
		return errors.New("cordis: cannot create effect on inactive context")
	case Unloading:
		return errors.New("cordis: cannot create effect on inactive context")
	default:
		return nil
	}
}
