package mount

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiyuan2403/Y_Cordis/cordis"
	"github.com/qiyuan2403/Y_Cordis/jsbridge"
	"github.com/qiyuan2403/Y_Cordis/loader"
)

// newEvaluator builds a real sidecar evaluator, skipping when node is absent.
func newEvaluator(t *testing.T) *jsbridge.Evaluator {
	t.Helper()
	pool, err := jsbridge.NewPool(jsbridge.Config{Size: 2, CallTimeout: 20 * time.Second})
	if err != nil {
		requireNode(t, err)
	}
	t.Cleanup(pool.Close)
	return &jsbridge.Evaluator{Pool: pool}
}

func loadBaseBundle(t *testing.T) []*loader.Entry {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "testdata", "official", "base.cordis.patch.yml"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	parsed, err := loader.Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	patches, err := loader.ParsePatches(parsed)
	if err != nil {
		t.Fatalf("ParsePatches: %v", err)
	}
	// The base bundle is ONE insert patch carrying all rows.
	var rows []loader.Value
	for _, p := range patches {
		rows = append(rows, p.Insert...)
	}
	entries, err := loader.ParseEntries(rows)
	if err != nil {
		t.Fatalf("ParseEntries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no entries parsed from the base bundle")
	}
	return entries
}

// TestDisabledIsEvaluatedAgainstLoaderScope pins the first half of the two-scope
// contract: a `!!js` disabled flag resolves against the LOADER context, which
// exists before any row mounts.
//
// The base bundle gates several rows on `!ctx.get('profileContext')`, so the
// answer must track whether that service is present — and must change when it
// changes, because disabled is re-decided at every mount decision.
func TestDisabledIsEvaluatedAgainstLoaderScope(t *testing.T) {
	ev := newEvaluator(t)
	root := cordis.NewRoot()
	// The loader's own context is ACTIVE by construction.
	root.Fiber().Inject()

	scopes := &Scopes{
		LoaderCtx: root,
		Env:       map[string]string{},
		Cwd:       "/w",
		Platform:  "linux",
		Home:      "/h",
		Helpers:   []string{"dshHomePath"},
	}
	entries := loadBaseBundle(t)
	ctx := context.Background()

	// Find a row whose disabled flag is a !!js expression.
	var gated *loader.Entry
	for _, e := range entries {
		if _, ok := e.Disabled.(loader.JsExpr); ok {
			gated = e
			break
		}
	}
	if gated == nil {
		t.Fatal("the base bundle should contain a !!js-gated row")
	}

	// Without profileContext, `!ctx.get('profileContext')` is true -> disabled.
	got, err := gated.DecideDisabled(ctx, ev, scopes)
	if err != nil {
		t.Fatalf("DecideDisabled: %v", err)
	}
	if !got {
		t.Fatalf("row %q: disabled = false, want true while profileContext is absent", gated.ID)
	}

	// Provide the service: the SAME decision must now flip.
	provider := root.Child()
	provider.Fiber().Inject()
	provider.Fiber().SetStateForTest(cordis.Active)
	if err := provider.Provide("profileContext", map[string]any{"name": "web"}, nil); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	got, err = gated.DecideDisabled(ctx, ev, scopes)
	if err != nil {
		t.Fatalf("DecideDisabled after provide: %v", err)
	}
	if got {
		t.Fatalf("row %q: disabled = true, want false once profileContext resolves", gated.ID)
	}
}

// TestConfigResolvesAgainstRowScopeNotLoaderScope pins the second half of the
// contract: a row's config expression sees services, and the distinction from
// the loader scope is WHEN the scope is built, not which context it is.
//
// This is worth stating precisely, because the naive reading is wrong. The
// official store is a FLAT map keyed by isolation label, assigned tree-wide
// (vendor/cordis/src/reflect.ts:209 `store`, `provide()` does
// `root[isolate][name] ??= Symbol(name)`). So a parent context does resolve a
// service provided by a descendant. What changes over time is which services
// have been PROVIDED SO FAR: at parse time none, and by the time a later row
// activates, the earlier rows' services exist.
//
// The test therefore provides the service and shows the row scope resolves it,
// while a scope captured BEFORE the provide does not.
func TestConfigResolvesAgainstRowScopeNotLoaderScope(t *testing.T) {
	ev := newEvaluator(t)
	root := cordis.NewRoot()
	root.Fiber().Inject()

	// A dedicated row context carrying the service the expression needs.
	rowCtx := root.Child()
	rowCtx.Fiber().Inject()
	rowCtx.Fiber().SetStateForTest(cordis.Active)
	if err := rowCtx.Provide("webStartup", map[string]any{"host": "0.0.0.0", "port": float64(9)}, nil); err != nil {
		t.Fatalf("Provide: %v", err)
	}

	scopes := &Scopes{
		LoaderCtx: root,
		RowCtx:    func(*loader.Entry) (*cordis.Context, error) { return rowCtx, nil },
		Env:       map[string]string{},
		Cwd:       "/w",
		Platform:  "linux",
		Home:      "/h",
		Helpers:   []string{"dshHomePath"},
	}

	// The row scope resolves the service.
	rowScope, err := scopes.RowScope(nil)
	if err != nil {
		t.Fatalf("RowScope: %v", err)
	}
	if _, ok := rowScope.Services["webStartup"]; !ok {
		t.Fatal("the row scope is missing webStartup")
	}

	// A scope built BEFORE the provide does not have it. This is the real
	// distinction the two-scope design protects: evaluation happens after the
	// row's injections activate, so the scope must be built then.
	early := cordis.NewRoot()
	early.Fiber().Inject()
	earlyScopes := &Scopes{LoaderCtx: early, Platform: "linux", Home: "/h"}
	if _, ok := earlyScopes.LoaderScope().Services["webStartup"]; ok {
		t.Fatal("a scope built before the provide unexpectedly sees webStartup")
	}

	// Now resolve a real expression through the row scope.
	e := &loader.Entry{ID: "web-startup", Config: mustParse(t, "host: !!js ctx.webStartup.host ?? '127.0.0.1'\nport: !!js ctx.webStartup.port ?? 3080\n")}
	cfg, err := e.ResolveConfig(context.Background(), ev, scopes)
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	obj, ok := cfg.(*loader.Object)
	if !ok {
		t.Fatalf("resolved config is %T, want *Object", cfg)
	}
	host, _ := obj.Get("host")
	if host != "0.0.0.0" {
		t.Fatalf("host = %v, want 0.0.0.0 (the row scope's value)", host)
	}
	port, _ := obj.Get("port")
	if port != float64(9) {
		t.Fatalf("port = %v, want 9", port)
	}
}

// TestConfigExpressionFallsBackForMissingField pins what `??` actually guards:
// a MISSING FIELD on a present service, not a missing service.
//
// `ctx.webStartup.host ?? 'x'` throws when webStartup itself is absent, because
// `??` guards the final value and property access on undefined throws first.
// That is correct JavaScript, and the shipped bundles rely on it: web-app's
// `ctx.webStartup.host` is only evaluated on a profile where the web startup
// plugin provides that service. Asserting a fallback for an ABSENT service
// would encode behavior the runtime does not have.
func TestConfigExpressionFallsBackForMissingField(t *testing.T) {
	ev := newEvaluator(t)
	root := cordis.NewRoot()
	root.Fiber().Inject()

	scopes := &Scopes{
		LoaderCtx: root,
		RowCtx:    func(*loader.Entry) (*cordis.Context, error) { return root, nil },
		Env:       map[string]string{},
		Cwd:       "/w",
		Platform:  "linux",
		Home:      "/h",
		Helpers:   []string{"dshHomePath"},
	}
	rowCtx := root.Child()
	rowCtx.Fiber().Inject()
	rowCtx.Fiber().SetStateForTest(cordis.Active)
	if err := rowCtx.Provide("webStartup", map[string]any{}, nil); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	scopes.RowCtx = func(*loader.Entry) (*cordis.Context, error) { return rowCtx, nil }
	e := &loader.Entry{ID: "fallback", Config: mustParse(t, "host: !!js ctx.webStartup.host ?? '127.0.0.1'\n")}
	cfg, err := e.ResolveConfig(context.Background(), ev, scopes)
	if err != nil {
		t.Fatalf("ResolveConfig: %v", err)
	}
	obj := cfg.(*loader.Object)
	host, _ := obj.Get("host")
	if host != "127.0.0.1" {
		t.Fatalf("host = %v, want the fallback 127.0.0.1", host)
	}
}

// TestDisabledRejectsNonBool pins fail-loud on an ambiguous disabled value.
func TestDisabledRejectsNonBool(t *testing.T) {
	ev := newEvaluator(t)
	root := cordis.NewRoot()
	root.Fiber().Inject()
	scopes := &Scopes{LoaderCtx: root, Platform: "linux", Home: "/h"}
	e := &loader.Entry{ID: "bad", Disabled: loader.JsExpr{Expr: "'a string'"}}
	if _, err := e.DecideDisabled(context.Background(), ev, scopes); err == nil {
		t.Fatal("a non-bool disabled expression was accepted")
	}
}

// TestMountAllSkipsDisabledRowsAndKeepsPendingRows pins the loop's two skip
// reasons: a disabled row never loads, and a row whose injections never resolve
// stays PENDING with its config never evaluated.
func TestMountAllSkipsDisabledRowsAndKeepsPendingRows(t *testing.T) {
	ev := newEvaluator(t)
	root := cordis.NewRoot()
	root.Fiber().Inject()

	scopes := &Scopes{
		LoaderCtx: root,
		RowCtx:    func(*loader.Entry) (*cordis.Context, error) { return root, nil },
		Env:       map[string]string{},
		Cwd:       "/w",
		Platform:  "linux",
		Home:      "/h",
		Helpers:   []string{"dshHomePath"},
	}

	loaded := 0
	m := &Mounter{
		Eval:   ev,
		Scopes: scopes,
		Load: func(e *loader.Entry) (*cordis.Fiber, error) {
			loaded++
			// A row that declares a service nobody provides stays PENDING.
			return root.Plugin(&cordis.Plugin{
				Name:   e.ID,
				Inject: e.InjectNames(),
				Apply:  func(*cordis.Context, any) error { return nil },
			}, nil)
		},
		Ctx: context.Background(),
	}

	entries := []*loader.Entry{
		{ID: "off", Disabled: true},
		{ID: "pending", Inject: []loader.Value{"nobody-provides-this"}},
		{ID: "live"},
	}
	got, err := m.MountAll(entries)
	if err != nil {
		t.Fatalf("MountAll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	if !got[0].Skipped || got[0].Reason != "disabled" {
		t.Fatalf("row 0 = %+v, want skipped/disabled", got[0])
	}
	if !got[1].Skipped || got[1].Reason != "injections pending" {
		t.Fatalf("row 1 = %+v, want skipped/injections pending", got[1])
	}
	if got[2].Skipped {
		t.Fatalf("row 2 = %+v, want mounted", got[2])
	}
	if loaded != 2 {
		t.Fatalf("loaded %d rows, want 2 (the disabled row must not load)", loaded)
	}
}

func mustParse(t *testing.T, src string) loader.Value {
	t.Helper()
	v, err := loader.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return v
}
