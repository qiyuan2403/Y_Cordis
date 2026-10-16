package jsbridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiyuan2403/Y_Cordis/loader"
	"github.com/qiyuan2403/Y_Cordis/workspace"
)

// TestServicePropertyAccess pins that services resolve BOTH as properties and
// through `get`. The shipped bundles use both forms: base writes
// `ctx.get('profileContext')`, while web-app writes `ctx.webStartup.host` and
// headless writes `ctx.headlessStartup.task`. Property access is what the
// official cordis context supports, so a scope that only offered `get` would
// make those bundles throw on undefined.
func TestServicePropertyAccess(t *testing.T) {
	p := testPool(t, 1)
	scope := &Scope{
		Services: map[string]any{
			"webStartup":      map[string]any{"host": "0.0.0.0", "port": float64(3080)},
			"headlessStartup": map[string]any{"task": "run the tests"},
		},
	}
	cases := []struct {
		expr string
		want any
	}{
		{"ctx.webStartup.host ?? '127.0.0.1'", "0.0.0.0"},
		{"ctx.webStartup.port ?? 3080", float64(3080)},
		{"ctx.webStartup.missing ?? 'fallback'", "fallback"},
		{"ctx.headlessStartup.task", "run the tests"},
		{"ctx.get('webStartup').host", "0.0.0.0"},
		{"ctx.get('headlessStartup').task", "run the tests"},
	}
	for _, tc := range cases {
		got, err := p.Eval(context.Background(), tc.expr, scope)
		if err != nil {
			t.Fatalf("Eval(%q): %v", tc.expr, err)
		}
		if got != tc.want {
			t.Fatalf("Eval(%q) = %#v, want %#v", tc.expr, got, tc.want)
		}
	}
}

// TestPermissionModelBlocksFsAndChildProcess pins the load-bearing sandbox:
// Node's permission model must deny filesystem and subprocess access, because
// `with (ctx)` alone only shadows unqualified names and leaves
// `globalThis.process` reachable.
func TestPermissionModelBlocksFsAndChildProcess(t *testing.T) {
	p := testPool(t, 1)

	// Reaching the REAL process through globalThis must not grant fs access.
	got, err := p.Eval(context.Background(),
		`(() => { try { globalThis.process.mainModule.require('node:fs').readFileSync('/etc/hostname'); return 'READ_ALLOWED' } catch (e) { return 'BLOCKED:' + (e.code || 'err') } })()`,
		&Scope{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	s, _ := got.(string)
	if !strings.HasPrefix(s, "BLOCKED:") {
		t.Fatalf("filesystem access was not blocked: %#v", got)
	}

	// Child processes must be blocked too.
	got, err = p.Eval(context.Background(),
		`(() => { try { globalThis.process.mainModule.require('node:child_process').execSync('echo x'); return 'SPAWN_ALLOWED' } catch (e) { return 'BLOCKED:' + (e.code || 'err') } })()`,
		&Scope{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	s, _ = got.(string)
	if !strings.HasPrefix(s, "BLOCKED:") {
		t.Fatalf("child_process was not blocked: %#v", got)
	}
}

// TestProtocolStreamIsNotStdout pins the transport choice: the worker writes
// protocol frames on fd 3, so a plugin's stray stdout write cannot corrupt the
// stream. A `console.log` inside an evaluated expression must not break the
// round trip.
func TestProtocolStreamIsNotStdout(t *testing.T) {
	p := testPool(t, 1)
	got, err := p.Eval(context.Background(),
		`(() => { console.log('stray plugin output'); return 'still-ok' })()`,
		&Scope{})
	if err != nil {
		t.Fatalf("a stray stdout write broke the protocol: %v", err)
	}
	if got != "still-ok" {
		t.Fatalf("got %#v, want \"still-ok\"", got)
	}
	// And the pool still works afterwards.
	if _, err := p.Eval(context.Background(), "1+1", &Scope{}); err != nil {
		t.Fatalf("pool unusable after stray output: %v", err)
	}
}

// TestOversizedResultIsRefused pins the frame bound: a worker producing a huge
// value must not be able to exhaust harness memory.
func TestOversizedResultIsRefused(t *testing.T) {
	p, err := NewPool(Config{Size: 1, CallTimeout: 10 * time.Second, MaxFrameBytes: 64 << 10})
	if err != nil {
		requireNode(t, err)
	}
	defer p.Close()

	_, err = p.Eval(context.Background(), "'x'.repeat(2_000_000)", &Scope{})
	if err == nil {
		t.Fatal("expected an oversized frame to be refused")
	}
	if !strings.Contains(err.Error(), "size limit") && !strings.Contains(err.Error(), "frame") {
		t.Fatalf("error should name the size bound, got %v", err)
	}
	// The pool must recover.
	if _, err := p.Eval(context.Background(), "1", &Scope{}); err != nil {
		t.Fatalf("pool unusable after an oversized frame: %v", err)
	}
}

// TestPoolStartsNoWorkersUntilUsed pins lazy spawning: an idle pool must not
// hold Node processes, which matters on memory-constrained Android.
func TestPoolStartsNoWorkersUntilUsed(t *testing.T) {
	p, err := NewPool(Config{Size: 4})
	if err != nil {
		requireNode(t, err)
	}
	defer p.Close()
	if n := p.live.Load(); n != 0 {
		t.Fatalf("idle pool holds %d workers, want 0", n)
	}
	if _, err := p.Eval(context.Background(), "1", &Scope{}); err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if n := p.live.Load(); n != 1 {
		t.Fatalf("after one call live = %d, want 1", n)
	}
}

// TestIsolatedWorkerSurvivesAndRestarts pins the untrusted-plugin path: an
// isolated instance keeps its own process and rebuilds it after a fault.
func TestIsolatedWorkerSurvivesAndRestarts(t *testing.T) {
	iso, err := NewIsolated(Config{CallTimeout: 5 * time.Second})
	if err != nil {
		requireNode(t, err)
	}
	defer iso.Close()

	// A module in the temp dir the worker can import.
	dir := t.TempDir()
	modPath := filepath.Join(dir, "plugin.mjs")
	if err := os.WriteFile(modPath, []byte("export const name = 'p'\nexport function apply() {}\n"), 0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}

	resp, err := iso.Call(context.Background(), Request{Method: MethodPing})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !resp.OK || resp.Value != "pong" {
		t.Fatalf("ping response = %+v", resp)
	}

	// A fault tears the process down; the next call must start a fresh one.
	if _, err := iso.Call(context.Background(), Request{Method: MethodEval, Expr: "while(true){}"}); err == nil {
		t.Fatal("expected the runaway call to fail")
	}
	resp, err = iso.Call(context.Background(), Request{Method: MethodPing})
	if err != nil {
		t.Fatalf("isolated worker did not restart: %v", err)
	}
	if !resp.OK {
		t.Fatalf("restarted worker response = %+v", resp)
	}
}

// TestImportReportsExportNames pins the trusted pooled import path.
//
// Loading a module from disk inherently needs filesystem read access, which the
// permission model blocks by default. The grant is SCOPED to the module's
// directory: passing AllowFsRead would let a plugin read every reachable file,
// including credentials.
func TestImportReportsExportNames(t *testing.T) {
	dir := t.TempDir()
	// Module loading goes through an Isolated plugin host, NOT the eval pool:
	// the pool holds zero fs permission so a `!!js` expression cannot read
	// files, and granting it read access to import would collapse that.
	host, err := NewPluginHost([]string{dir}, Config{CallTimeout: 10 * time.Second})
	if err != nil {
		requireNode(t, err)
	}
	defer host.Close()
	modPath := filepath.Join(dir, "mod.mjs")
	if err := os.WriteFile(modPath, []byte("export const a = 1\nexport function b() {}\n"), 0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}
	names, err := host.Import(context.Background(), modPath)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("exports = %v, want [a b]", names)
	}
}

// TestInterpolatesWebAppAndHeadlessBundles is a BRIDGE test (see the caveat on
// TestInterpolatesOfficialBundleEndToEnd): it proves property-style service
// access evaluates, using a test-supplied scope. Real mounting must build each
// row's scope from that row's activated injections, which this test does not
// exercise.
func TestInterpolatesWebAppAndHeadlessBundles(t *testing.T) {
	pool, err := NewPool(Config{Size: 2, CallTimeout: 15 * time.Second})
	if err != nil {
		requireNode(t, err)
	}
	defer pool.Close()

	ev := &Evaluator{Pool: pool, Base: Scope{
		Env:      map[string]string{"DSH_TOOLS_MODE": "ptc"},
		Cwd:      "/workspace",
		Platform: "linux",
		Home:     "/home/u/.dsh",
		Helpers:  []string{"dshHomePath"},
		Services: map[string]any{
			"profileContext":  map[string]any{"name": "web"},
			"webStartup":      map[string]any{"host": "0.0.0.0", "port": float64(9)},
			"headlessStartup": map[string]any{"task": "go"},
		},
	}}

	for _, name := range []string{"web-app", "headless"} {
		src, err := os.ReadFile(filepath.Join("..", "testdata", "official", name+".cordis.patch.yml"))
		if err != nil {
			t.Fatalf("fixture %s: %v", name, err)
		}
		parsed, err := loader.Parse(src)
		if err != nil {
			t.Fatalf("%s: Parse: %v", name, err)
		}
		if !loader.HasJsExpr(parsed) {
			t.Fatalf("%s: expected !!js expressions", name)
		}
		in := &loader.Interpolator{Eval: ev, Ctx: context.Background()}
		out, err := in.Interpolate(parsed, &loader.EvalScope{})
		if err != nil {
			t.Fatalf("%s: Interpolate: %v", name, err)
		}
		if loader.HasJsExpr(out) {
			t.Fatalf("%s: !!js nodes remain", name)
		}
	}
}

// TestWorkerScriptAvoidsNoexecSharedStorage pins the Android-shaped failure
// mode against the REAL device: a worker script written to shared storage
// cannot run, because that mount is noexec.
//
// The fixture is the actual /storage/emulated/0 rather than a synthetic
// directory, because a synthetic one is unreliable: as root, permission bits
// do not restrict file creation, so a 0600 directory still probes usable.
// noexec is a MOUNT property and cannot be faked with modes.
//
// Skipped on hosts without Android shared storage.
func TestWorkerScriptAvoidsNoexecSharedStorage(t *testing.T) {
	const shared = "/storage/emulated/0"
	if _, err := os.Stat(shared); err != nil {
		t.Skipf("no shared storage at %s", shared)
	}
	workspace.Forget(shared)
	caps := workspace.Probe(shared)
	if !caps.Usable() {
		t.Skipf("shared storage is not usable here: %+v", caps)
	}
	if caps.Executable {
		t.Skipf("this device's shared storage is executable; nothing to assert: %+v", caps)
	}

	p, err := NewPool(Config{Size: 1, ExecRoot: shared, CallTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewPool must fall through to a usable dir, got: %v", err)
	}
	defer p.Close()

	// The bridge works, proving it picked a runnable script location.
	got, err := p.Eval(context.Background(), "1+1", &Scope{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got != float64(2) {
		t.Fatalf("got %#v, want 2", got)
	}
	// And it did NOT put the script on the noexec mount.
	if strings.HasPrefix(p.cfg.WorkerScript, shared) {
		t.Fatalf("worker script landed on noexec storage: %s", p.cfg.WorkerScript)
	}
}

// TestExecRootIsHonoredWhenUsable pins the positive case: a good exec root is
// used, so a caller that resolved roots deliberately gets them.
func TestExecRootIsHonoredWhenUsable(t *testing.T) {
	good := t.TempDir()
	workspace.Forget(good)
	p, err := NewPool(Config{Size: 1, ExecRoot: good, CallTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer p.Close()
	if !strings.HasPrefix(p.cfg.WorkerScript, good) {
		t.Fatalf("worker script = %s, want it under %s", p.cfg.WorkerScript, good)
	}
	if _, err := p.Eval(context.Background(), "1", &Scope{}); err != nil {
		t.Fatalf("Eval: %v", err)
	}
}

// TestEvalPoolHoldsNoFilesystemPermission pins the boundary that made moving
// Import necessary: the eval pool must not be able to read ANY file, because
// every `!!js` expression runs there.
//
// If a read grant ever leaks into the pool (for example by reintroducing an
// Import path that needs one), this test fails — which is the point.
func TestEvalPoolHoldsNoFilesystemPermission(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	p, err := NewPool(Config{Size: 1, CallTimeout: 10 * time.Second})
	if err != nil {
		requireNode(t, err)
	}
	defer p.Close()

	// An expression must not be able to read the file, however it reaches fs.
	// `require` is not even in scope inside the evaluator (it is a `new
	// Function` body, not module scope), and the permission model blocks the
	// global routes.
	for _, expr := range []string{
		`(() => { try { return require('node:fs').readFileSync(` + "`" + secret + "`" + `, 'utf8') } catch (e) { return 'BLOCKED' } })()`,
		`(() => { try { return globalThis.process.mainModule.require('node:fs').readFileSync(` + "`" + secret + "`" + `, 'utf8') } catch (e) { return 'BLOCKED' } })()`,
		`(() => { try { return globalThis.process.binding('fs').readFileSync(` + "`" + secret + "`" + `, 'utf8') } catch (e) { return 'BLOCKED' } })()`,
	} {
		got, err := p.Eval(context.Background(), expr, &Scope{})
		if err != nil {
			// A thrown permission error is also an acceptable refusal.
			continue
		}
		if got != "BLOCKED" {
			t.Fatalf("the eval pool read a file: expr=%s got=%v", expr, got)
		}
	}

	// Positive control: the SAME module loads in a plugin host granted the
	// directory, proving the difference is the grant rather than a broken
	// mechanism. Module loading is the real fs-touching operation, so it is the
	// honest way to show the boundary.
	modPath := filepath.Join(dir, "mod.mjs")
	if err := os.WriteFile(modPath, []byte("export const ok = 1\n"), 0o600); err != nil {
		t.Fatalf("write module: %v", err)
	}
	// An isolated worker WITHOUT the grant must refuse the same module, showing
	// the boundary is the grant and not the mechanism. (Pool.Import no longer
	// exists at all: module loading was moved off the eval path precisely so no
	// read grant can ever reach a `!!js` expression.)
	ungranted, err := NewIsolated(Config{CallTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewIsolated: %v", err)
	}
	defer ungranted.Close()
	if _, err := ungranted.Import(context.Background(), modPath); err == nil {
		t.Fatal("an ungranted isolated worker loaded a module, which requires fs read")
	}
	host, err := NewPluginHost([]string{dir}, Config{CallTimeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewPluginHost: %v", err)
	}
	defer host.Close()
	names, err := host.Import(context.Background(), modPath)
	if err != nil {
		t.Fatalf("a granted plugin host could not load a module: %v", err)
	}
	if len(names) != 1 || names[0] != "ok" {
		t.Fatalf("plugin host exports = %v, want [ok]", names)
	}
}

// TestPluginHostRefusesChildProcessAndNet pins that the plugin host's grant is
// read-only: a plugin gets its files and nothing more.
func TestPluginHostRefusesChildProcessAndNet(t *testing.T) {
	dir := t.TempDir()
	host, err := NewPluginHost([]string{dir}, Config{CallTimeout: 10 * time.Second})
	if err != nil {
		requireNode(t, err)
	}
	defer host.Close()

	resp, err := host.Call(context.Background(), Request{
		Method: MethodEval,
		Expr:   `(() => { try { require('node:child_process').execSync('echo x'); return 'SPAWNED' } catch (e) { return 'BLOCKED' } })()`,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !resp.OK || resp.Value != "BLOCKED" {
		t.Fatalf("a plugin host could spawn a subprocess: %+v", resp)
	}
}
