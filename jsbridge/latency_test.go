package jsbridge

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSidecarLatencyBudget measures where the bridge's time actually goes.
//
// This exists because the harness's per-turn budget is small: a model round
// trip is on the order of 200ms, so anything the host spends on the device side
// is a visible fraction of a turn. Node's cold start is the suspect — it is
// tens of milliseconds per process — so the numbers here decide how the pool
// must be sized and kept warm.
//
// Run with -v to see the table.
func TestSidecarLatencyBudget(t *testing.T) {
	// 1. Cold: pool creation plus the first call, i.e. a process spawn.
	coldStart := time.Now()
	p, err := NewPool(Config{Size: 4, CallTimeout: 20 * time.Second})
	if err != nil {
		requireNode(t, err)
	}
	defer p.Close()
	poolCreate := time.Since(coldStart)
	if _, err := p.Eval(context.Background(), "1", &Scope{}); err != nil {
		t.Fatalf("first Eval: %v", err)
	}
	coldTotal := time.Since(coldStart)

	// 2. Warm: repeated calls on an already-spawned worker.
	const warmIters = 200
	warmStart := time.Now()
	for range warmIters {
		if _, err := p.Eval(context.Background(), "1+1", &Scope{}); err != nil {
			t.Fatalf("warm Eval: %v", err)
		}
	}
	warmPer := time.Since(warmStart) / warmIters

	// 3. A realistic expression: the shape the bundles actually use.
	expr := "process.env.DSH_PERMISSION_MODE ?? 'workspace-write'"
	scope := &Scope{Env: map[string]string{"DSH_PERMISSION_MODE": "workspace-write"}}
	const realIters = 200
	realStart := time.Now()
	for range realIters {
		if _, err := p.Eval(context.Background(), expr, scope); err != nil {
			t.Fatalf("realistic Eval: %v", err)
		}
	}
	realPer := time.Since(realStart) / realIters

	// 4. Spawn cost, isolated: the number that decides whether the pool must
	// stay warm.
	spawnStart := time.Now()
	if _, err := startWorker(p.cfg); err != nil {
		t.Fatalf("startWorker: %v", err)
	}
	spawnCost := time.Since(spawnStart)

	t.Logf("pool create (no spawn): %s", poolCreate)
	t.Logf("cold (create + first call): %s", coldTotal)
	t.Logf("raw node spawn: %s", spawnCost)
	t.Logf("warm call: %s", warmPer)
	t.Logf("realistic call: %s", realPer)
	t.Logf("warm call as fraction of a 200ms turn: %.4f%%", float64(warmPer.Microseconds())/2000)
	t.Logf("raw spawn as fraction of a 200ms turn: %.2f%%", float64(spawnCost.Microseconds())/2000)
	_ = fmt.Sprint
}
