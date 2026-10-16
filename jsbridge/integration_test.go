package jsbridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiyuan2403/Y_Cordis/loader"
)

// TestInterpolatesOfficialBundleEndToEnd is a BRIDGE test, not a mounting test.
//
// It proves the sidecar evaluates the shipped bundle's expressions correctly
// when handed the scope those expressions expect. It does NOT prove mounting
// works: the loader evaluates `disabled` against the LOADER context at every
// mount decision, and `config` against the ENTRY's fiber context only after
// that row's injections activate (vendor/loader/src/index.ts:112 evaluates
// `config` with `this.ctx` = the fiber's ctx; entry.ts:91 evaluates `disabled`
// against the entry's ctx). The scope here is supplied by the test, so it
// cannot stand in for that scheduling.
func TestInterpolatesOfficialBundleEndToEnd(t *testing.T) {
	pool, err := NewPool(Config{Size: 2, CallTimeout: 15e9})
	if err != nil {
		requireNode(t, err)
	}
	defer pool.Close()

	src, err := os.ReadFile(filepath.Join("..", "testdata", "official", "base.cordis.patch.yml"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	parsed, err := loader.Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !loader.HasJsExpr(parsed) {
		t.Fatal("the base bundle must contain !!js expressions")
	}

	ev := &Evaluator{Pool: pool, Base: Scope{
		Env: map[string]string{
			"DSH_PERMISSION_MODE": "danger-full-access",
			"DSH_TELEMETRY_MODE":  "DISABLED",
		},
		Cwd:      "/workspace",
		Platform: "linux",
		Home:     "/home/u/.dsh",
		Helpers:  []string{"dshHomePath"},
		Services: map[string]any{"profileContext": map[string]any{"name": "desktop"}},
	}}
	in := &loader.Interpolator{Eval: ev, Ctx: context.Background()}
	out, err := in.Interpolate(parsed, &loader.EvalScope{})
	if err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if loader.HasJsExpr(out) {
		t.Fatal("!!js nodes remain after interpolation")
	}

	// Walk the composed tree and assert the specific rows whose values come
	// from !!js in the shipped bundle.
	rows := indexRows(t, out)
	assertRow(t, rows, "session-persistence-jsonl", "config", func(cfg any) bool {
		obj, ok := cfg.(*loader.Object)
		if !ok {
			return false
		}
		v, _ := obj.Get("root")
		return v == "/home/u/.dsh/sessions"
	}, "root == dshHomePath('sessions')")

	assertRow(t, rows, "sandbox-policy", "config", func(cfg any) bool {
		obj, ok := cfg.(*loader.Object)
		if !ok {
			return false
		}
		mode, _ := obj.Get("mode")
		root, _ := obj.Get("workspaceRoot")
		return mode == "danger-full-access" && root == "/workspace"
	}, "mode from process.env, workspaceRoot from process.cwd()")

	assertRow(t, rows, "approval", "config", func(cfg any) bool {
		obj, ok := cfg.(*loader.Object)
		if !ok {
			return false
		}
		policy, _ := obj.Get("policy")
		return policy == "never"
	}, "policy == 'never' for danger-full-access")

	// bash-sandbox is disabled on win32; on linux the !!js yields false.
	assertRow(t, rows, "bash-sandbox", "disabled", func(v any) bool {
		return v == false
	}, "process.platform === 'win32' -> false on linux")
}

// indexRows flattens the entry list into id -> row, descending into both
// group `config` children and patch `insert` arrays (the base bundle is a
// patch list whose single row inserts all 95 entries).
func indexRows(t *testing.T, v any) map[string]*loader.Object {
	t.Helper()
	out := map[string]*loader.Object{}
	var walk func(any)
	walk = func(node any) {
		switch n := node.(type) {
		case *loader.Object:
			if id, ok := n.Get("id"); ok {
				if s, ok := id.(string); ok {
					out[s] = n
				}
			}
			for _, k := range n.Keys() {
				child, _ := n.Get(k)
				walk(child)
			}
		case []loader.Value:
			for _, item := range n {
				walk(item)
			}
		}
	}
	walk(v)
	return out
}

func assertRow(t *testing.T, rows map[string]*loader.Object, id, field string, ok func(any) bool, why string) {
	t.Helper()
	row, found := rows[id]
	if !found {
		t.Fatalf("row %q not found in the composed tree", id)
	}
	got, _ := row.Get(field)
	if !ok(got) {
		t.Fatalf("row %q field %q = %#v (%s)", id, field, got, why)
	}
}
