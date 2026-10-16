package jsbridge

import (
	"os"
	"testing"
)

// requireNode decides whether a missing Node runtime is fatal or a skip.
//
// Default: skip, so a developer without Node can still run the non-JS parts of
// the suite. With Y_CORDIS_REQUIRE_NODE=1 (set by CI): fail, because a suite
// that silently skips its load-bearing cases reports success it did not earn.
//
// Every test that needs Node must go through this helper rather than calling
// t.Skipf directly, or the CI guarantee has a hole.
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
