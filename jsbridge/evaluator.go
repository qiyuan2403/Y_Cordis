package jsbridge

import (
	"context"

	"github.com/qiyuan2403/Y_Cordis/loader"
)

// Evaluator adapts a Pool to loader.Evaluator, so `!!js` config expressions
// evaluate in the same JavaScript semantics the official loader uses.
type Evaluator struct {
	Pool *Pool
	// Base carries the scope fields shared by every expression (env, cwd,
	// platform, home). Per-call scope values from the loader override them.
	Base Scope
}

// compile-time assertion that the adapter satisfies the loader seam.
var _ loader.Evaluator = (*Evaluator)(nil)

// Eval evaluates one expression. A nil scope uses Base unchanged.
func (e *Evaluator) Eval(ctx context.Context, expr string, scope *loader.EvalScope) (any, error) {
	merged := e.Base
	if scope != nil {
		if scope.Env != nil {
			merged.Env = scope.Env
		}
		if scope.Cwd != "" {
			merged.Cwd = scope.Cwd
		}
		if scope.Platform != "" {
			merged.Platform = scope.Platform
		}
		if scope.Home != "" {
			merged.Home = scope.Home
		}
		if len(scope.Helpers) > 0 {
			merged.Helpers = scope.Helpers
		}
		if len(scope.Services) > 0 {
			merged.Services = scope.Services
		}
	}
	if merged.Platform == "" {
		merged.Platform = hostPlatform()
	}
	return e.Pool.Eval(ctx, expr, &merged)
}
