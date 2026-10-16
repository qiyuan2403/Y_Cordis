package mount

import (
	"os"
	"testing"
)

// requireNode decides whether a missing Node runtime is fatal or a skip.
//
// Mirrors the helper in the jsbridge package: default skip so a developer
// without Node can run the rest, but Y_CORDIS_REQUIRE_NODE=1 (set by CI) makes
// it fatal, because a suite that silently skips its load-bearing cases reports
// success it did not earn.
func requireNode(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if os.Getenv("Y_CORDIS_REQUIRE_NODE") == "1" {
		t.Fatalf("node is required in this environment but is unavailable: %v", err)
	}
	t.Skipf("node unavailable: %v", err)
}
