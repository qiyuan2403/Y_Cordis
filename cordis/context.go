package cordis

import (
	"fmt"
	"sync"
)

// Context is a dependency container and lifecycle scope.
//
// The official Context is a Proxy whose property reads run through a resolver.
// Go has no Proxy, so this is an explicit structure with an ancestor pointer:
// a child context delegates reads it cannot satisfy to its parent, which is the
// semantic the proxy implements.
//
// TWO maps carry the scoping semantics (vendor/cordis/src/context.ts):
//
//   - isolate maps a service NAME to an isolation LABEL. Resolution keys the
//     store by that label, so two scopes isolating the same name see different
//     implementations.
//   - intercept maps a service NAME to config merged for plugins below this
//     context (ancestor entries first).
//
// Both are inherited by children and never mutate the parent.
type Context struct {
	parent *Context
	// st is the tree-wide service store, shared by every context in the tree.
	st *store

	// isolate and intercept are the scoped maps described above. They are
	// copied on write, so a child never mutates its parent's view.
	isolate   map[string]string
	intercept map[string]any

	// labels assigns a stable label to each service name provided in this
	// tree, so an unisolated name resolves to the same key everywhere.
	labels   map[string]string
	labelsMu sync.Mutex

	// registry and uidSeq live on the root context only.
	registry map[*Fiber]struct{}
	uidSeq   uint64
	// labelSeq mints unique isolation labels.
	labelSeq uint64
	// runtimes holds one Runtime per plugin name, on the root only.
	runtimes map[string]*Runtime

	fiber  *Fiber
	events *Events
	// filter, when set, restricts which listener owners an event reaches.
	filter func(owner *Context) bool

	mu       sync.RWMutex
	children []*Context
	disposed bool

	// regMu guards the fiber registry and uid counter. It is SEPARATE from mu
	// because Child() holds mu while creating the child's fiber, and fiber
	// creation registers on the root: a shared lock would self-deadlock.
	regMu sync.Mutex
}

// NewRoot creates the root context of a tree.
func NewRoot() *Context {
	st := newStore()
	root := &Context{
		st:        st,
		isolate:   map[string]string{},
		intercept: map[string]any{},
		labels:    map[string]string{},
	}
	root.events = newEvents(root)
	root.fiber = newRootFiber(root)
	st.subscribe(root.onServiceChange)
	return root
}

// Child creates a nested scope whose resolution includes this context's
// services. Disposing the child never disposes the parent.
func (c *Context) Child() *Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	child := &Context{
		parent:    c,
		st:        c.st,
		isolate:   c.isolate,
		intercept: c.intercept,
		labels:    c.labels,
	}
	child.events = newEvents(child)
	child.fiber = newFiber(child, nil)
	c.children = append(c.children, child)
	return child
}

// Isolate returns a child context whose service `name` resolves under `label`.
//
// Two contexts given the same label join scopes (vendor/cordis/src/context.ts
// `isolate()`). An empty label mints a fresh one, giving a private scope.
func (c *Context) Isolate(name, label string) *Context {
	if label == "" {
		label = c.mintLabel(name)
	}
	next := copyMap(c.isolate)
	next[name] = label
	child := c.Child()
	child.isolate = next
	return child
}

// Intercept returns a child context carrying extra config for service `name`.
//
// Plugins loaded below it see that config merged into the service's resolved
// config, ancestor entries first; the parent is unaffected.
func (c *Context) Intercept(name string, config any) *Context {
	next := copyMap(c.intercept)
	next[name] = config
	child := c.Child()
	child.intercept = next
	return child
}

// Fiber returns the fiber that owns this context.
func (c *Context) Fiber() *Fiber { return c.fiber }

// Events returns this context's event service.
func (c *Context) Events() *Events { return c.events }

// label resolves the store key for a service name in this scope.
//
// The lookup walks the ancestor chain for an isolate entry, then falls back to
// the tree-wide label for the name. This is the Go form of reading
// `ctx[symbols.isolate][name]` through the prototype chain.
func (c *Context) label(name string) string {
	for cur := c; cur != nil; cur = cur.parent {
		if label, ok := cur.isolate[name]; ok {
			return label
		}
	}
	return c.labelFor(name)
}

// labelFor returns (creating if needed) the tree-wide label for a name.
func (c *Context) labelFor(name string) string {
	root := c.root()
	root.labelsMu.Lock()
	defer root.labelsMu.Unlock()
	if label, ok := root.labels[name]; ok {
		return label
	}
	label := "svc:" + name
	root.labels[name] = label
	return label
}

func (c *Context) mintLabel(name string) string {
	root := c.root()
	root.labelsMu.Lock()
	defer root.labelsMu.Unlock()
	root.labelSeq++
	return fmt.Sprintf("iso:%s:%d", name, root.labelSeq)
}

func (c *Context) root() *Context {
	cur := c
	for cur.parent != nil {
		cur = cur.parent
	}
	return cur
}

// Get resolves a service by name, walking the ancestor chain through the
// isolation labels.
//
// `strict` mirrors the official `get(name, strict)`: when true, only an
// implementation whose providing fiber is ACTIVE is returned. A nil result
// means "absent", and callers degrade exactly like `ctx.get(name)` consumers.
func (c *Context) Get(name string) any {
	return c.get(name, true)
}

func (c *Context) get(name string, strict bool) any {
	impl := c.impl(name, strict)
	if impl == nil {
		return nil
	}
	return impl.Value
}

func (c *Context) impl(name string, strict bool) *Impl {
	label := c.label(name)
	impl := c.st.get(label)
	if impl == nil {
		return nil
	}
	if strict && impl.Fiber != nil && impl.Fiber.State() != Active {
		return nil
	}
	if impl.Check != nil && !impl.Check() {
		return nil
	}
	return impl
}

// Provide registers a service owned by the current fiber and returns a
// disposer that withdraws it.
//
// The service is unregistered automatically when the owning fiber unloads, and
// dependent fibers are notified so they can recompute their epochs.
func (c *Context) Provide(name string, value any, check func() bool) error {
	label := c.label(name)
	if existing := c.st.get(label); existing != nil {
		return fmt.Errorf("cordis: service %q has been registered at <%s>", name, existing.Fiber.Name())
	}
	impl := &Impl{Name: name, Value: value, Fiber: c.fiber, Check: check}
	c.st.put(label, impl)
	c.fiber.addProvided(label, name)

	if c.fiber.State() == Active {
		c.st.notify([]string{name})
	}
	return nil
}

// Set overwrites a provided service's value. Only the providing fiber may do
// so (vendor/cordis/src/reflect.ts `set()`).
func (c *Context) Set(name string, value any) error {
	label := c.label(name)
	impl := c.st.get(label)
	if impl == nil {
		return fmt.Errorf("cordis: cannot set property %q without provide", name)
	}
	if impl.Fiber != c.fiber {
		return fmt.Errorf("cordis: cannot set property %q in multiple fibers", name)
	}
	impl.Value = value
	return nil
}

// onServiceChange recomputes every fiber whose declared injections include a
// changed service, then refreshes it. This is the reactive core: providing a
// service wakes the plugins that were waiting for it.
func (c *Context) onServiceChange(names []string) {
	for _, f := range c.allFibers() {
		changed := false
		for _, name := range names {
			if !f.requires(name) {
				continue
			}
			// Isolation scope must match: a service provided in another scope
			// does not satisfy this fiber.
			if f.ctx.label(name) != c.label(name) {
				continue
			}
			changed = true
			break
		}
		if changed {
			f.refresh()
		}
	}
}

// allFibers returns every live fiber in this tree.
func (c *Context) allFibers() []*Fiber {
	root := c.root()
	root.regMu.Lock()
	defer root.regMu.Unlock()
	out := make([]*Fiber, 0, len(root.registry))
	for f := range root.registry {
		out = append(out, f)
	}
	return out
}

func copyMap[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
