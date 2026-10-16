// Package mount wires the loader's evaluation seam to the cordis kernel: it
// builds the two scopes the dialect needs, and drives the mount loop.
//
// WHY TWO SCOPES. The official loader evaluates a row's `!!js` in two places
// against two different contexts (verified in vendor/loader/src):
//
//   - `disabled` is evaluated against the LOADER context at EVERY mount
//     decision (config/entry.ts `disabledOf`), because it is a live predicate
//     that may flip as services come and go.
//   - `config` is evaluated against the ROW'S FIBER context, inside the
//     `internal/config` waterfall, which fires only AFTER that row's declared
//     injections become active (index.ts:112). That is what makes
//     `ctx.webStartup.host` resolve — before activation the service does not
//     exist.
//
// Feeding one pre-built scope to both is the mistake this package exists to
// avoid: it appears to work when a test seeds every service by hand, and fails
// the moment a real composition provides them in dependency order.
package mount

import (
	"context"
	"fmt"

	"github.com/qiyuan2403/Y_Cordis/cordis"
	"github.com/qiyuan2403/Y_Cordis/loader"
)

// Scopes implements loader.ScopeProvider over a cordis tree.
type Scopes struct {
	// LoaderCtx is the context `disabled` evaluates against: the loader's own,
	// which is stable across mount decisions.
	LoaderCtx *cordis.Context
	// RowCtx returns the fiber context for a row, i.e. the context whose
	// services that row's config may read. It is consulted only after the row's
	// injections are active.
	RowCtx func(e *loader.Entry) (*cordis.Context, error)
	// Env is `process.env` as seen by expressions.
	Env map[string]string
	// Cwd is `process.cwd()`.
	Cwd string
	// Platform is `process.platform`.
	Platform string
	// Home backs the `dshHomePath` helper.
	Home string
	// Helpers names the helpers to expose.
	Helpers []string
}

var _ loader.ScopeProvider = (*Scopes)(nil)

// LoaderScope builds the scope for `disabled` evaluation.
func (s *Scopes) LoaderScope() *loader.EvalScope {
	return s.scopeFor(s.LoaderCtx)
}

// RowScope builds the scope for one row's `config` evaluation.
func (s *Scopes) RowScope(e *loader.Entry) (*loader.EvalScope, error) {
	if s.RowCtx == nil {
		return nil, fmt.Errorf("mount: no row context resolver configured")
	}
	ctx, err := s.RowCtx(e)
	if err != nil {
		return nil, err
	}
	return s.scopeFor(ctx), nil
}

func (s *Scopes) scopeFor(ctx *cordis.Context) *loader.EvalScope {
	out := &loader.EvalScope{
		Env:      s.Env,
		Cwd:      s.Cwd,
		Platform: s.Platform,
		Home:     s.Home,
		Helpers:  s.Helpers,
	}
	if ctx != nil {
		// The official context is a Proxy, so ANY provided service is reachable
		// by name. Enumerating the visible set is how a struct reproduces that.
		out.Services = ctx.VisibleServices()
	}
	return out
}

// Mounted records the outcome of one row's mount attempt.
type Mounted struct {
	Entry *loader.Entry
	Fiber *cordis.Fiber
	// Skipped is true when the row was disabled or its injections never
	// resolved; Reason explains which.
	Skipped bool
	Reason  string
}

// Mounter drives rows through the two-phase evaluation.
type Mounter struct {
	// Eval evaluates `!!js` expressions.
	Eval loader.Evaluator
	// Scopes supplies the per-row scopes.
	Scopes *Scopes
	// Load loads a row. The raw config is passed through unchanged; resolution
	// happens inside the kernel's activation path, so it can read the row's
	// services. Returning a nil fiber means the row was loaded in-process
	// without a fiber (a group carrier).
	Load func(e *loader.Entry) (*cordis.Fiber, error)
	// Ctx bounds the whole mount.
	Ctx context.Context
}

// MountAll mounts every row in order.
//
// For each row:
//
//  1. `disabled` is decided against the LOADER scope, re-evaluated at THIS
//     mount decision (never cached).
//  2. The row is loaded, which activates its injections.
//  3. Its config is resolved against the ROW scope — inside the kernel's
//     activation path, so it runs only when the injections are active.
//
// A row whose injections never resolve stays PENDING: its config is never
// evaluated and its plugin body never runs, which is exactly the official
// behavior.
func (m *Mounter) MountAll(entries []*loader.Entry) ([]Mounted, error) {
	ctx := m.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	out := make([]Mounted, 0, len(entries))
	for _, e := range entries {
		disabled, err := e.DecideDisabled(ctx, m.Eval, m.Scopes)
		if err != nil {
			return out, err
		}
		if disabled {
			out = append(out, Mounted{Entry: e, Skipped: true, Reason: "disabled"})
			continue
		}

		fiber, err := m.Load(e)
		if err != nil {
			return out, fmt.Errorf("mount: entry %q: %w", e.ID, err)
		}
		if fiber != nil && fiber.State() == cordis.Pending {
			out = append(out, Mounted{Entry: e, Fiber: fiber, Skipped: true, Reason: "injections pending"})
			continue
		}
		if fiber != nil && fiber.State() == cordis.Failed {
			out = append(out, Mounted{Entry: e, Fiber: fiber, Skipped: true, Reason: "activation failed"})
			continue
		}
		out = append(out, Mounted{Entry: e, Fiber: fiber})
	}
	return out, nil
}

// Resolver builds the kernel-side config resolver for one row.
//
// It is handed to cordis.Plugin as ResolveConfig, so the kernel calls it at
// activation time — after the injections are active and before the body runs.
// Building it per row is what keeps the ROW scope (not a global one) in play.
func (m *Mounter) Resolver(e *loader.Entry) func(*cordis.Context, any) (any, error) {
	return func(ctx *cordis.Context, raw any) (any, error) {
		_ = ctx // the row's services are read through m.Scopes.RowScope
		return e.ResolveConfig(m.ctxOrBackground(), m.Eval, m.Scopes)
	}
}

func (m *Mounter) ctxOrBackground() context.Context {
	if m.Ctx != nil {
		return m.Ctx
	}
	return context.Background()
}
