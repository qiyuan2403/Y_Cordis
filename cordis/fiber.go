package cordis

import (
	"errors"
	"fmt"
	"sync"
)

// Disposer releases one registration. Disposers run in LIFO order when the
// owning fiber unloads.
type Disposer func() error

// Fiber is one plugin application's runtime instance: its dependency state, its
// lifecycle effects, and its cleanup.
//
// STATE MACHINE (vendor/cordis/src/fiber.ts). A fiber is PENDING until every
// declared injection resolves, then LOADING while the callback runs, then
// ACTIVE. Withdrawing a required service moves it back to UNLOADING and
// disposes its effects; providing it again reloads. `_setEpoch` is the hinge:
// the epoch is the concatenation of the providing fibers' uids, so any change
// of provider invalidates the load.
type Fiber struct {
	ctx     *Context
	parent  *Context
	runtime *Runtime

	mu    sync.Mutex
	state State
	// inject maps each required service name to its intercept config.
	inject map[string]any
	// epoch fingerprints the current providers. It is INACTIVE when any
	// required service is missing.
	epoch string
	// config is the raw config; it is resolved (with `!!js` interpolated)
	// only after injections become active.
	config any
	// disposers run in LIFO order on unload.
	disposers []Disposer
	// provided remembers the store labels this fiber owns, so unload can
	// withdraw them.
	provided []providedService
	// uid identifies this fiber; 0 for the root fiber.
	uid uint64
	// name is the plugin name for diagnostics.
	name string
	// apply runs the plugin body.
	apply func(*Context, any) error
	// onStateChange fires after every transition, for `internal/status`.
	onStateChange func(*Fiber, State)
	// resolveConfig rewrites the raw config at activation time. It is the Go
	// form of the official `internal/config` waterfall.
	resolveConfig func(ctx *Context, raw any) (any, error)
	// err records the last load failure.
	err error
	// root marks the tree's root fiber.
	root bool
}

type providedService struct {
	label string
	name  string
}

// inactiveEpoch is the epoch of a fiber with unsatisfied injections. It
// mirrors the official `INACTIVE` sentinel.
const inactiveEpoch = "__INACTIVE__"

func newRootFiber(ctx *Context) *Fiber {
	return &Fiber{
		ctx:    ctx,
		state:  Active,
		inject: map[string]any{},
		root:   true,
	}
}

func newFiber(ctx *Context, runtime *Runtime) *Fiber {
	f := &Fiber{
		ctx:     ctx,
		parent:  ctx.parent,
		runtime: runtime,
		state:   Pending,
		inject:  map[string]any{},
		epoch:   inactiveEpoch,
	}
	ctx.fiber = f
	ctx.root().register(f)
	return f
}

// State reports the fiber's lifecycle state.
func (f *Fiber) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// Name reports the plugin name used in diagnostics.
func (f *Fiber) Name() string { return f.name }

// UID reports the fiber's unique id (0 for the root fiber).
func (f *Fiber) UID() uint64 { return f.uid }

// Context returns the fiber's context.
func (f *Fiber) Context() *Context { return f.ctx }

// Inject declares required services and their intercept config.
func (f *Fiber) Inject(names ...string) {
	f.mu.Lock()
	for _, n := range names {
		f.inject[n] = nil
	}
	f.mu.Unlock()
}

func (f *Fiber) requires(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.inject[name]
	return ok
}

// Effect runs setup and registers its disposer for LIFO execution at unload.
//
// A setup error leaves nothing registered. Registering on a disposed fiber runs
// the disposer immediately and reports loudly, so resources never leak silently
// past teardown (vendor/cordis/src/fiber.ts `effect()`).
func (f *Fiber) Effect(setup func() (Disposer, error)) error {
	if f.State() == Disposed {
		d, err := setup()
		if err == nil && d != nil {
			_ = d()
		}
		return errors.New("cordis: cannot create effect on a disposed fiber; disposer ran immediately")
	}
	if f.State() == Unloading {
		return errors.New("cordis: cannot create effect while the fiber is unloading")
	}
	d, err := setup()
	if err != nil {
		return err
	}
	if d == nil {
		d = func() error { return nil }
	}
	f.mu.Lock()
	f.disposers = append(f.disposers, d)
	f.mu.Unlock()
	return nil
}

// refresh recomputes the epoch and transitions the fiber.
//
// This is the reactivity hinge: it runs whenever a service this fiber requires
// is provided or withdrawn.
func (f *Fiber) refresh() {
	f.mu.Lock()
	if f.root {
		f.mu.Unlock()
		return
	}
	// Compute the epoch from the current providers.
	epoch := ""
	ready := true
	for name := range f.inject {
		impl := f.ctx.impl(name, false)
		if impl == nil {
			ready = false
			break
		}
		epoch += fmt.Sprintf(":%d", impl.Fiber.uid)
	}
	if !ready {
		epoch = inactiveEpoch
	}
	old := f.epoch
	if epoch == old {
		f.mu.Unlock()
		return
	}
	f.epoch = epoch
	f.mu.Unlock()

	if epoch != inactiveEpoch && old == inactiveEpoch {
		f.reload()
		return
	}
	f.unload()
}

// reload activates the plugin.
//
// Config is resolved HERE, not at parse time: the official kernel interpolates
// `!!js` in the `internal/config` waterfall, which fires only after the fiber's
// injections are active and against the fiber's own context
// (vendor/loader/src/index.ts:112).
func (f *Fiber) reload() {
	f.setState(Loading)
	// Config is resolved HERE, after the injections became active and before
	// the plugin body runs — never at parse time.
	if f.resolveConfig != nil {
		resolved, err := f.resolveConfig(f.ctx, f.config)
		if err != nil {
			f.mu.Lock()
			f.err = err
			f.mu.Unlock()
			f.setState(Failed)
			return
		}
		f.config = resolved
	}
	if f.apply != nil {
		if err := f.apply(f.ctx, f.config); err != nil {
			f.mu.Lock()
			f.err = err
			f.mu.Unlock()
			f.setState(Failed)
			return
		}
	}
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	f.setState(Active)
}

// unload runs the disposers in LIFO order, then withdraws provided services.
//
// Every disposer runs even when an earlier one fails; the errors are joined, so
// one stuck disposer never strands the rest.
func (f *Fiber) unload() {
	if f.State() == Disposed {
		return
	}
	f.setState(Unloading)

	f.mu.Lock()
	disposers := f.disposers
	f.disposers = nil
	provided := f.provided
	f.provided = nil
	f.mu.Unlock()

	var errs []error
	for i := len(disposers) - 1; i >= 0; i-- {
		if err := runDisposer(disposers[i]); err != nil {
			errs = append(errs, err)
		}
	}
	for _, p := range provided {
		f.ctx.st.remove(p.label)
	}
	if len(provided) > 0 {
		names := make([]string, 0, len(provided))
		for _, p := range provided {
			names = append(names, p.name)
		}
		f.ctx.st.notify(names)
	}
	_ = errors.Join(errs...)

	if f.epoch == inactiveEpoch {
		f.setState(Pending)
	}
}

func runDisposer(d Disposer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("cordis: disposer panicked: %v", r)
		}
	}()
	return d()
}

// Dispose unloads the fiber permanently.
func (f *Fiber) Dispose() error {
	if f.State() == Disposed {
		return nil
	}
	f.unload()
	f.setState(Disposed)
	f.ctx.root().unregister(f)
	return nil
}

func (f *Fiber) addProvided(label, name string) {
	f.mu.Lock()
	f.provided = append(f.provided, providedService{label: label, name: name})
	f.mu.Unlock()
}

func (f *Fiber) setState(s State) {
	f.mu.Lock()
	old := f.state
	f.state = s
	hook := f.onStateChange
	f.mu.Unlock()
	if old != s && hook != nil {
		hook(f, old)
	}
}

// register/unregister track live fibers on the root for change notification.
// They take regMu, NOT mu: Child() holds mu while creating the child's fiber.
func (c *Context) register(f *Fiber) {
	c.regMu.Lock()
	defer c.regMu.Unlock()
	if c.registry == nil {
		c.registry = map[*Fiber]struct{}{}
	}
	c.registry[f] = struct{}{}
	c.uidSeq++
	f.uid = c.uidSeq
}

func (c *Context) unregister(f *Fiber) {
	c.regMu.Lock()
	defer c.regMu.Unlock()
	delete(c.registry, f)
}

// SetStateForTest forces the fiber into a state. It exists for tests that need
// a provider to appear ACTIVE without running a plugin body; production code
// drives state through refresh/reload/unload.
func (f *Fiber) SetStateForTest(s State) { f.setState(s) }
