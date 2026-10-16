package loader

import (
	"context"
	"fmt"
)

// Entry is one parsed plugin row.
//
// Raw values stay raw: `!!js` nodes are NOT evaluated at parse time. The
// official loader evaluates them at two different moments against two different
// scopes, so baking them here would be wrong in a way that only shows up once
// rows actually mount.
type Entry struct {
	// ID is the stable row identifier, unique within its tree.
	ID string
	// Name is the module specifier the row loads.
	Name string
	// Config is the raw plugin config, with `!!js` nodes intact.
	Config Value
	// Group marks a nested group row, whose Config is a child entry list.
	Group bool
	// Disabled is the raw disabled flag: bool, JsExpr, or nil.
	Disabled Value
	// Inject lists required services (array form) or carries intercept config
	// (object form). Both shapes are preserved.
	Inject Value
	// Extra carries keys this build does not model, so a round trip through the
	// loader never silently drops configuration.
	Extra *Object
}

// ParseEntries decodes a parsed entry list into rows.
//
// Structural violations fail loudly: a config the loader cannot represent is a
// bug in the input, not something to paper over.
func ParseEntries(v Value) ([]*Entry, error) {
	seq, ok := v.([]Value)
	if !ok {
		if v == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("loader: entry list must be a sequence, got %T", v)
	}
	out := make([]*Entry, 0, len(seq))
	for i, item := range seq {
		obj, ok := item.(*Object)
		if !ok {
			return nil, fmt.Errorf("loader: entry %d must be a mapping, got %T", i, item)
		}
		e, err := parseEntry(obj, i)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func parseEntry(obj *Object, index int) (*Entry, error) {
	e := &Entry{Extra: NewObject()}
	for _, k := range obj.Keys() {
		v, _ := obj.Get(k)
		switch k {
		case "id":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("loader: entry %d id must be a string", index)
			}
			e.ID = s
		case "name":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("loader: entry %d name must be a string", index)
			}
			e.Name = s
		case "config":
			e.Config = v
		case "group":
			b, _ := v.(bool)
			e.Group = b
		case "disabled":
			e.Disabled = v
		case "inject":
			e.Inject = v
		default:
			e.Extra.Set(k, v)
		}
	}
	return e, nil
}

// InjectNames returns the service names this row requires.
//
// The official `Inject` type accepts an array of names or an object mapping
// name to intercept config; both forms yield the same name set
// (vendor/cordis/src/registry.ts `Inject.resolve`).
func (e *Entry) InjectNames() []string {
	switch t := e.Inject.(type) {
	case []Value:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case *Object:
		out := make([]string, 0, t.Len())
		for _, k := range t.Keys() {
			out = append(out, k)
		}
		return out
	default:
		return nil
	}
}

// InjectConfig returns the intercept config declared for one service, if the
// row used the object form.
func (e *Entry) InjectConfig(name string) any {
	obj, ok := e.Inject.(*Object)
	if !ok {
		return nil
	}
	v, _ := obj.Get(name)
	return v
}

// ScopeProvider supplies the evaluation scope for one row.
//
// The two scopes differ, and that difference is the reason this is an interface
// rather than one value passed to Interpolate:
//
//   - DisabledScope is the LOADER's scope. `disabled` is evaluated against the
//     loader context at every mount decision, and re-evaluated on each one
//     (vendor/loader/src/loader/src/config/entry.ts `disabledOf`).
//   - ConfigScope is the ROW'S FIBER scope, available only after that row's
//     declared injections have activated, and it is what makes `ctx.webStartup`
//     resolve (vendor/loader/src/index.ts:112 interpolates with `this.ctx`).
type ScopeProvider interface {
	// LoaderScope is the scope `disabled` evaluates against. It is available
	// before any row mounts.
	LoaderScope() *EvalScope
	// RowScope is the scope `config` evaluates against. It is called only after
	// the row's injections are active; returning an error defers or fails the
	// activation.
	RowScope(e *Entry) (*EvalScope, error)
}

// DecideDisabled evaluates a row's `disabled` flag against the loader scope.
//
// It is called at EVERY mount decision, never cached: a `!!js` disabled flag is
// a live predicate, and the official loader re-evaluates it each time
// (vendor/loader/src/config/entry.ts `disabledOf` -> `this.evaluate(...)`).
//
// A nil flag means enabled. A non-bool result is an error: the official code
// coerces with `Boolean()`, but a config expression that yields, say, a string
// is far more likely to be a mistake than an intent, and this host fails loud
// on ambiguity.
func (e *Entry) DecideDisabled(ctx context.Context, ev Evaluator, scopes ScopeProvider) (bool, error) {
	switch v := e.Disabled.(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	case JsExpr:
		if ev == nil {
			return false, ErrNoEvaluator
		}
		if scopes == nil {
			return false, fmt.Errorf("loader: entry %q has a !!js disabled flag but no scope provider", e.ID)
		}
		raw, err := ev.Eval(ctx, v.Expr, scopes.LoaderScope())
		if err != nil {
			return false, fmt.Errorf("loader: entry %q disabled expression: %w", e.ID, err)
		}
		b, ok := raw.(bool)
		if !ok {
			return false, fmt.Errorf("loader: entry %q disabled expression must yield a bool, got %T (%v)", e.ID, raw, raw)
		}
		return b, nil
	default:
		return false, fmt.Errorf("loader: entry %q disabled must be a bool or a !!js expression, got %T", e.ID, v)
	}
}

// ResolveConfig evaluates a row's config against the row's own scope.
//
// Called at ACTIVATION, not at parse time. The row's injections must already be
// active, so `ctx.<service>` resolves; passing a scope built before that point
// is the failure this seam exists to prevent.
func (e *Entry) ResolveConfig(ctx context.Context, ev Evaluator, scopes ScopeProvider) (Value, error) {
	if !HasJsExpr(e.Config) {
		return e.Config, nil
	}
	if ev == nil {
		return nil, ErrNoEvaluator
	}
	if scopes == nil {
		return nil, fmt.Errorf("loader: entry %q has a !!js config but no scope provider", e.ID)
	}
	scope, err := scopes.RowScope(e)
	if err != nil {
		return nil, fmt.Errorf("loader: entry %q config scope: %w", e.ID, err)
	}
	in := &Interpolator{Eval: ev, Ctx: ctx}
	out, err := in.Interpolate(e.Config, scope)
	if err != nil {
		return nil, fmt.Errorf("loader: entry %q config expression: %w", e.ID, err)
	}
	return out, nil
}
