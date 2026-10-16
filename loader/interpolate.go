package loader

import (
	"context"
	"errors"
)

// Evaluator resolves one `!!js` expression against a scope.
//
// The official implementation is
// `new Function('ctx','expr','with (ctx) { return eval(expr) }')` — arbitrary
// JavaScript. This host delegates to a JS engine (the jsbridge sidecar) rather
// than reimplementing a subset, because the shipped bundles already exceed any
// tractable subset (`ctx.get('profileContext')?.name === 'desktop' &&
// ['darwin','win32'].includes(process.platform) ? process.platform : null`,
// and sdk-app reaches `process.getBuiltinModule('node:path')`).
//
// An implementation that cannot evaluate an expression must return an error;
// silently substituting a literal would write unevaluated config into a
// running entry.
type Evaluator interface {
	Eval(ctx context.Context, expr string, scope *EvalScope) (any, error)
}

// EvalScope is the environment an expression is evaluated in.
type EvalScope struct {
	// Env is `process.env`.
	Env map[string]string
	// Cwd is `process.cwd()`.
	Cwd string
	// Platform is `process.platform`.
	Platform string
	// Home backs the `dshHomePath` helper.
	Home string
	// Helpers names the helpers to expose (currently only "dshHomePath").
	Helpers []string
	// Services backs `ctx.get(name)`.
	Services map[string]any
}

// ErrNoEvaluator reports an expression that cannot be evaluated because no
// evaluator is installed. This mirrors the official loader's fail-loud
// behavior when the `!!js` evaluator is absent.
var ErrNoEvaluator = errors.New("loader: !!js expression cannot be evaluated: no JavaScript evaluator registered")

// Interpolator resolves `!!js` nodes in a parsed value tree.
type Interpolator struct {
	Eval Evaluator
	Ctx  context.Context
}

// Interpolate replaces every `!!js` node in v with its evaluated value.
//
// Ported from vendor/loader/src/config/utils.ts `interpolate`: the walk is
// RECURSIVE over arrays and nested objects, not just top-level scalars, so a
// `!!js` node buried in a config object still resolves.
//
// A nil or erroring evaluator fails loudly rather than leaving the raw node in
// place.
func (in *Interpolator) Interpolate(v Value, scope *EvalScope) (Value, error) {
	switch t := v.(type) {
	case JsExpr:
		if in.Eval == nil {
			return nil, ErrNoEvaluator
		}
		ctx := in.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		out, err := in.Eval.Eval(ctx, t.Expr, scope)
		if err != nil {
			return nil, err
		}
		return out, nil
	case []Value:
		out := make([]Value, len(t))
		for i := range t {
			item, err := in.Interpolate(t[i], scope)
			if err != nil {
				return nil, err
			}
			out[i] = item
		}
		return out, nil
	case *Object:
		out := NewObject()
		for _, k := range t.keys {
			raw, _ := t.Get(k)
			item, err := in.Interpolate(raw, scope)
			if err != nil {
				return nil, err
			}
			out.Set(k, item)
		}
		return out, nil
	default:
		return v, nil
	}
}

// HasJsExpr reports whether v contains any `!!js` node. Callers use it to skip
// the interpolating walk (and any evaluator round trip) for the common case.
func HasJsExpr(v Value) bool {
	switch t := v.(type) {
	case JsExpr:
		return true
	case []Value:
		for i := range t {
			if HasJsExpr(t[i]) {
				return true
			}
		}
	case *Object:
		for _, k := range t.keys {
			raw, _ := t.Get(k)
			if HasJsExpr(raw) {
				return true
			}
		}
	}
	return false
}
