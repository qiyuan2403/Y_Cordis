package cordis

// VisibleServices returns every service resolvable from this context, keyed by
// name.
//
// This exists because the official context is a Proxy: reading `ctx.anything`
// runs through the resolver, so a plugin (and a config expression evaluated in
// the plugin's scope) can reach ANY provided service by name. A Go struct has
// no such catch-all, so the set must be enumerated when a scope is projected
// for an evaluator.
//
// A name is included when it resolves from this context under its isolation
// label and its provider is ACTIVE — the same gate `Get` applies.
func (c *Context) VisibleServices() map[string]any {
	names := c.visibleNames()
	out := make(map[string]any, len(names))
	for _, name := range names {
		if v := c.Get(name); v != nil {
			out[name] = v
		}
	}
	return out
}

// visibleNames collects candidate service names: every name provided anywhere in
// the tree, plus every name mentioned in an isolation map along the ancestor
// chain (a scope may isolate a name that is provided only below it).
func (c *Context) visibleNames() []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}

	// Names provided in the tree.
	root := c.root()
	root.labelsMu.Lock()
	for name := range root.labels {
		add(name)
	}
	root.labelsMu.Unlock()

	// Names isolated along this context's chain.
	for cur := c; cur != nil; cur = cur.parent {
		for name := range cur.isolate {
			add(name)
		}
	}

	// Names provided by any live implementation.
	c.st.mu.RLock()
	for _, impl := range c.st.impls {
		add(impl.Name)
	}
	c.st.mu.RUnlock()

	return out
}
