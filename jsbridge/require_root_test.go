package jsbridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiyuan2403/Y_Cordis/loader"
)

// TestRequireRootResolvesNodeModules proves the seam pluginmgr depends on:
// with RequireRoot set, a worker's `import()` resolves packages installed
// under RequireRoot/node_modules — the directory an npm profile installs
// into. Without this the installer's output would be unreachable from the
// evaluator.
func TestRequireRootResolvesNodeModules(t *testing.T) {
	if testing.Short() {
		t.Skip("requires node")
	}
	root := t.TempDir()
	pkg := filepath.Join(root, "node_modules", "@qrpdsh-test", "marker")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"@qrpdsh-test/marker","version":"1.0.0","type":"module","main":"index.mjs"}`
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := "export const marker = 'resolved-from-require-root'\n"
	if err := os.WriteFile(filepath.Join(pkg, "index.mjs"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}

	pool, err := NewPool(Config{
		Size:        1,
		CallTimeout: 30 * time.Second,
		RequireRoot: root,
	})
	if err != nil {
		requireNode(t, err)
		t.FailNow()
	}
	defer pool.Close()

	ev := &Evaluator{Pool: pool}
	out, err := ev.Eval(context.Background(),
		`(async () => (await import('@qrpdsh-test/marker')).marker)()`,
		&loader.EvalScope{Platform: "linux"})
	if err != nil {
		requireNode(t, err)
		t.Fatalf("eval: %v", err)
	}
	if s, _ := out.(string); s != "resolved-from-require-root" {
		t.Fatalf("marker resolved to %v, want the package export", out)
	}
}
