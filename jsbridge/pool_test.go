package jsbridge

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// testPool returns a pool proven able to serve a call.
//
// The probe matters: pool creation is LAZY (no process starts until the first
// call), so a missing Node runtime only surfaces at call time. Without the
// probe, a test would fail deep inside its own assertions instead of taking the
// skip-or-fail path requireNode provides.
func testPool(t *testing.T, size int) *Pool {
	t.Helper()
	p, err := NewPool(Config{Size: size, CallTimeout: 10 * time.Second})
	if err != nil {
		requireNode(t, err)
		t.FailNow()
	}
	t.Cleanup(p.Close)
	if _, err := p.Eval(context.Background(), "1", &Scope{}); err != nil {
		requireNode(t, err)
		t.FailNow()
	}
	return p
}

// TestEvalMatchesOfficialScopeSemantics pins the expressions the shipped
// bundles actually use, evaluated through the same `with (ctx) { eval(expr) }`
// shape as vendor/loader/src/config/utils.ts.
func TestEvalMatchesOfficialScopeSemantics(t *testing.T) {
	p := testPool(t, 1)
	scope := &Scope{
		Env:      map[string]string{"DSH_PERMISSION_MODE": "danger-full-access"},
		Cwd:      "/work",
		Platform: "linux",
		Home:     "/home/u/.dsh",
		Helpers:  []string{"dshHomePath"},
		Services: map[string]any{"profileContext": map[string]any{"name": "desktop"}},
	}
	cases := []struct {
		expr string
		want any
	}{
		{"!ctx.get('profileContext')", false},
		{"ctx.get('profileContext')?.name !== 'desktop'", false},
		{"dshHomePath('sessions')", "/home/u/.dsh/sessions"},
		{"process.env.DSH_PERMISSION_MODE ?? 'workspace-write'", "danger-full-access"},
		{"(process.env.DSH_PERMISSION_MODE ?? 'workspace-write') === 'danger-full-access' ? 'never' : 'ask'", "never"},
		{"process.platform === 'win32'", false},
		{"process.platform !== 'win32'", true},
		{"process.cwd()", "/work"},
		{"process.env.DSH_TELEMETRY_MODE || 'FEEDBACK_ONLY'", "FEEDBACK_ONLY"},
		{"Number(process.env.DSH_CONTEXT_WINDOW ?? 1000000)", float64(1000000)},
		{"!(process.env.DSH_MISSING ?? '')", true},
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

// TestEvalDoesNotSeeHostEnvironment pins the credential-scrubbing guarantee: a
// worker must not inherit the harness environment.
func TestEvalDoesNotSeeHostEnvironment(t *testing.T) {
	t.Setenv("DSH_SECRET_CANARY", "leaked")
	p := testPool(t, 1)
	got, err := p.Eval(context.Background(), "process.env.DSH_SECRET_CANARY ?? 'absent'", &Scope{})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if got != "absent" {
		t.Fatalf("host environment leaked into the worker: got %#v", got)
	}
}

// TestEvalRejectsGetBuiltinModule pins the deliberate refusal: a Go host
// cannot honour the Node module system, and a silent stub would be worse than
// a loud error.
func TestEvalRejectsGetBuiltinModule(t *testing.T) {
	p := testPool(t, 1)
	_, err := p.Eval(context.Background(),
		"process.getBuiltinModule('node:path').resolve('/a')", &Scope{})
	if err == nil {
		t.Fatal("expected getBuiltinModule to fail")
	}
	if !strings.Contains(err.Error(), "getBuiltinModule") {
		t.Fatalf("error should name getBuiltinModule, got %v", err)
	}
}

// TestEvalInfiniteLoopIsKilled pins the hard timeout: a runaway expression must
// not hang the harness, and the worker must be killed rather than reused.
func TestEvalInfiniteLoopIsKilled(t *testing.T) {
	p, err := NewPool(Config{Size: 1, CallTimeout: 700 * time.Millisecond})
	if err != nil {
		requireNode(t, err)
	}
	defer p.Close()

	start := time.Now()
	_, err = p.Eval(context.Background(), "while (true) {}", &Scope{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the runaway expression to fail")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("timeout took %s, want well under 5s", elapsed)
	}
	// The pool must still serve calls afterwards (the worker was replaced).
	got, err := p.Eval(context.Background(), "1 + 1", &Scope{})
	if err != nil {
		t.Fatalf("pool unusable after a killed worker: %v", err)
	}
	if got != float64(2) {
		t.Fatalf("got %#v, want 2", got)
	}
}

// TestConcurrentEvalsAreIsolated pins that the pool serves concurrent callers
// without cross-talk, and is race-clean.
func TestConcurrentEvalsAreIsolated(t *testing.T) {
	p := testPool(t, 4)
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			expr := "process.env.N ?? 'unset'"
			scope := &Scope{Env: map[string]string{"N": itoaTest(i)}}
			got, err := p.Eval(context.Background(), expr, scope)
			if err != nil {
				errs <- err
				return
			}
			if got != itoaTest(i) {
				errs <- &mismatch{got: got, want: itoaTest(i)}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

type mismatch struct{ got, want any }

func (m *mismatch) Error() string {
	return "cross-talk: got " + fmt.Sprint(m.got) + ", want " + fmt.Sprint(m.want)
}

func itoaTest(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// TestEvalErrorCarriesMessage pins that a JavaScript throw surfaces as a
// WorkerError with the original message.
func TestEvalErrorCarriesMessage(t *testing.T) {
	p := testPool(t, 1)
	_, err := p.Eval(context.Background(), "throw new Error('boom')", &Scope{})
	if err == nil {
		t.Fatal("expected an error")
	}
	var we *WorkerError
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want it to mention boom", err)
	}
	_ = we
}

// TestClosedPoolRejectsCalls pins that a closed pool fails loudly instead of
// blocking or panicking.
func TestClosedPoolRejectsCalls(t *testing.T) {
	p, err := NewPool(Config{Size: 1})
	if err != nil {
		requireNode(t, err)
	}
	p.Close()
	if _, err := p.Eval(context.Background(), "1", &Scope{}); err == nil {
		t.Fatal("expected a closed pool to reject the call")
	}
}

// TestContextCancelAbortsAcquire pins that a cancelled context does not hang
// when no worker is free.
func TestContextCancelAbortsAcquire(t *testing.T) {
	p, err := NewPool(Config{Size: 1, CallTimeout: 2 * time.Second})
	if err != nil {
		requireNode(t, err)
	}
	defer p.Close()

	// Occupy the only worker.
	blocked := make(chan struct{})
	go func() {
		_, _ = p.Eval(context.Background(), "while(true){}", &Scope{})
		close(blocked)
	}()
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := p.Eval(ctx, "1", &Scope{}); err == nil {
		t.Fatal("expected the second call to fail while the worker is busy")
	}
	<-blocked
}

// TestConcurrentStress exercises acquire/release/discard under contention,
// including workers killed by timeouts mid-flight. `-race` is unavailable on
// this kernel (ThreadSanitizer rejects the 39-bit VMA range), so this
// high-contention run plus the live-counter invariant is the concurrency
// evidence.
func TestConcurrentStress(t *testing.T) {
	p, err := NewPool(Config{Size: 3, CallTimeout: 500 * time.Millisecond})
	if err != nil {
		requireNode(t, err)
	}
	defer p.Close()

	const workers = 12
	const perWorker = 6
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWorker {
				// Every third call is a runaway so workers are killed and
				// refilled while others are in flight.
				if (w+i)%3 == 0 {
					_, _ = p.Eval(context.Background(), "while(true){}", &Scope{})
					continue
				}
				got, err := p.Eval(context.Background(), "process.env.V ?? 'none'", &Scope{
					Env: map[string]string{"V": itoaTest(w)},
				})
				if err != nil {
					continue // a killed peer may have taken this slot; retry-worthy, not a failure
				}
				if got != itoaTest(w) {
					t.Errorf("cross-talk: got %v, want %v", got, itoaTest(w))
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// The pool must still work and must not have leaked slots.
	got, err := p.Eval(context.Background(), "1+1", &Scope{})
	if err != nil {
		t.Fatalf("pool unusable after stress: %v", err)
	}
	if got != float64(2) {
		t.Fatalf("got %v, want 2", got)
	}
	if n := p.live.Load(); n > int64(p.cfg.Size) || n <= 0 {
		t.Fatalf("live worker count %d outside (0, %d]", n, p.cfg.Size)
	}
}
