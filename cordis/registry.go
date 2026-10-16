package cordis

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// Plugin is one composition unit.
//
// The official kernel accepts a function, a class, or an object with `apply`.
// Go has one shape — a struct with an optional Apply — and the loader supplies
// the body. `Inject` and `Provide` are metadata: Inject is what the fiber waits
// on, Provide is what the loader validates against the catalog.
type Plugin struct {
	// Name is the display name used in diagnostics and logger names.
	Name string
	// Inject lists the services this plugin requires; it loads only while all
	// of them are available.
	Inject []string
	// Provide lists the service names this plugin contributes. It is metadata
	// for loader validation; Apply is expected to call Provide for each.
	Provide []string
	// Apply runs the plugin body with its context and resolved config.
	Apply func(*Context, any) error
	// ResolveConfig, when set, rewrites the raw config immediately before Apply
	// runs — and ONLY then. This is the Go form of the official
	// `internal/config` waterfall (vendor/loader/src/index.ts:112): the hook
	// fires after the fiber's injections become active, so a config expression
	// may read `ctx.<service>`.
	//
	// It must not be called at parse time: before activation the service does
	// not exist, so the expression would read undefined.
	ResolveConfig func(ctx *Context, raw any) (any, error)
}

// Runtime is the mutable record shared by every fiber of one plugin callback.
//
// The official kernel keys this by the plugin's callback identity; Go uses the
// plugin's name, which is what the loader has.
type Runtime struct {
	Name string
	// fibers is every live fiber of this plugin (one per Plugin() call).
	fibers sync.Map
	// counter assigns fiber ordinals for diagnostics.
	counter atomic.Uint64
}

// Mount loads a plugin on this context and returns its fiber.
//
// A missing injection fails loudly: deferral is the loader's job, and Mount
// hiding a misconfiguration would violate fail-loud.
func (c *Context) Mount(p *Plugin) (*Fiber, error) {
	for _, name := range p.Inject {
		if c.Get(name) == nil {
			return nil, fmt.Errorf("cordis: plugin %q requires missing service %q", p.Name, name)
		}
	}
	return c.load(p, nil)
}

// Plugin loads a plugin on this context, resolving its injections.
//
// Unlike Mount, a missing injection leaves the fiber PENDING rather than
// failing: the fiber activates later, when a Provide satisfies it. This is the
// reactive path the loader depends on.
func (c *Context) Plugin(p *Plugin, config any) (*Fiber, error) {
	return c.load(p, config)
}

func (c *Context) load(p *Plugin, config any) (*Fiber, error) {
	child := c.Child()
	child.filter = func(owner *Context) bool {
		return owner == child || owner.fiber == child.fiber
	}
	f := child.fiber
	f.name = p.Name
	f.apply = p.Apply
	f.config = config
	f.resolveConfig = p.ResolveConfig
	for _, name := range p.Inject {
		f.inject[name] = nil
	}
	if rt := c.runtimeFor(p); rt != nil {
		f.runtime = rt
		rt.fibers.Store(f, struct{}{})
		rt.counter.Add(1)
	}
	// Recompute the epoch: if every injection already resolves, the plugin
	// loads immediately; otherwise it stays PENDING until a Provide wakes it.
	f.refresh()
	return f, nil
}

// runtimeFor returns the shared runtime for a plugin, creating it on first use.
func (c *Context) runtimeFor(p *Plugin) *Runtime {
	root := c.root()
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.runtimes == nil {
		root.runtimes = map[string]*Runtime{}
	}
	rt, ok := root.runtimes[p.Name]
	if !ok {
		rt = &Runtime{Name: p.Name}
		root.runtimes[p.Name] = rt
	}
	return rt
}

// Inject declares dependencies and runs a callback once they resolve. It is the
// shorthand for loading a plugin whose only purpose is to wait.
func (c *Context) Inject(names []string, apply func(*Context, any) error) (*Fiber, error) {
	return c.Plugin(&Plugin{Name: "inject", Inject: names, Apply: apply}, nil)
}
