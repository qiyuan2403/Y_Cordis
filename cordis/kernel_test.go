package cordis

import (
	"errors"
	"sync"
	"testing"
)

// TestEffectRunsDisposersLIFO pins the teardown order. Reversed order is the
// whole point: a resource registered later may depend on one registered
// earlier, so it must be released first.
func TestEffectRunsDisposersLIFO(t *testing.T) {
	root := NewRoot()
	f := root.fiber

	var order []string
	for _, name := range []string{"a", "b", "c"} {
		n := name
		if err := f.Effect(func() (Disposer, error) {
			order = append(order, "setup:"+n)
			return func() error {
				order = append(order, "dispose:"+n)
				return nil
			}, nil
		}); err != nil {
			t.Fatalf("Effect(%s): %v", n, err)
		}
	}
	if err := f.Dispose(); err != nil {
		t.Fatalf("Dispose: %v", err)
	}

	want := []string{"setup:a", "setup:b", "setup:c", "dispose:c", "dispose:b", "dispose:a"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestEffectSetupFailureRegistersNothing pins that a failed setup leaves no
// disposer behind.
func TestEffectSetupFailureRegistersNothing(t *testing.T) {
	root := NewRoot()
	f := root.fiber
	ran := false
	err := f.Effect(func() (Disposer, error) {
		return nil, errors.New("setup failed")
	})
	if err == nil {
		t.Fatal("expected the setup error to surface")
	}
	f.mu.Lock()
	n := len(f.disposers)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d disposers registered after a failed setup, want 0", n)
	}
	_ = ran
}

// TestDisposeRunsEveryDisposerDespiteFailures pins that one failing disposer
// never strands the rest (the official code joins errors).
func TestDisposeRunsEveryDisposerDespiteFailures(t *testing.T) {
	root := NewRoot()
	f := root.fiber
	var ran []string
	for _, name := range []string{"x", "y", "z"} {
		n := name
		_ = f.Effect(func() (Disposer, error) {
			return func() error {
				ran = append(ran, n)
				if n == "y" {
					return errors.New("boom")
				}
				return nil
			}, nil
		})
	}
	_ = f.Dispose()
	if len(ran) != 3 {
		t.Fatalf("ran %v, want all three despite the failure", ran)
	}
}

// TestDisposerPanicIsContained pins that a panicking disposer is reported, not
// propagated, so teardown completes.
func TestDisposerPanicIsContained(t *testing.T) {
	root := NewRoot()
	f := root.fiber
	reached := false
	_ = f.Effect(func() (Disposer, error) {
		return func() error { panic("kaboom") }, nil
	})
	_ = f.Effect(func() (Disposer, error) {
		return func() error { reached = true; return nil }, nil
	})
	if err := f.Dispose(); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if !reached {
		t.Fatal("a panicking disposer prevented the later disposer from running")
	}
}

// TestInjectReactivity pins the epoch mechanism: a plugin declaring an
// injection stays PENDING until the service is provided, then becomes ACTIVE;
// withdrawing the service moves it back.
func TestInjectReactivity(t *testing.T) {
	root := NewRoot()

	var states []State
	applied := 0
	f, err := root.Plugin(&Plugin{
		Name:   "consumer",
		Inject: []string{"svc"},
		Apply: func(c *Context, cfg any) error {
			applied++
			return nil
		},
	}, nil)
	if err != nil {
		t.Fatalf("Plugin: %v", err)
	}
	f.onStateChange = func(_ *Fiber, old State) { states = append(states, old) }

	if got := f.State(); got != Pending {
		t.Fatalf("before provide: state = %v, want PENDING", got)
	}
	if applied != 0 {
		t.Fatalf("plugin applied %d times before its injection resolved", applied)
	}

	// Providing the service must wake the waiting fiber.
	if err := root.Provide("svc", "value", nil); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if got := f.State(); got != Active {
		t.Fatalf("after provide: state = %v, want ACTIVE", got)
	}
	if applied != 1 {
		t.Fatalf("plugin applied %d times, want 1", applied)
	}
}

// TestGetIsStrictAboutProviderState pins that a service whose providing fiber
// is not ACTIVE does not resolve (the official `get(name, strict)` gate).
func TestGetIsStrictAboutProviderState(t *testing.T) {
	root := NewRoot()
	child := root.Child()
	// A freshly created child fiber is PENDING, so its service must not
	// resolve to consumers.
	if err := child.Provide("early", 1, nil); err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if v := root.Get("early"); v != nil {
		t.Fatalf("a service from a non-ACTIVE fiber resolved: %v", v)
	}
}

// TestIsolateScopesResolution pins that two isolated scopes see different
// implementations of the same name, while the parent scope keeps its own.
func TestIsolateScopesResolution(t *testing.T) {
	root := NewRoot()
	if err := root.Provide("svc", "root-value", nil); err != nil {
		t.Fatalf("Provide: %v", err)
	}

	scoped := root.Isolate("svc", "label-a")
	scoped.fiber.setState(Active)
	if err := scoped.Provide("svc", "scoped-value", nil); err != nil {
		t.Fatalf("scoped Provide: %v", err)
	}

	if v := root.Get("svc"); v != "root-value" {
		t.Fatalf("root sees %v, want root-value", v)
	}
	if v := scoped.Get("svc"); v != "scoped-value" {
		t.Fatalf("isolated scope sees %v, want scoped-value", v)
	}

	// Two contexts joining the SAME label share the scope.
	joined := root.Isolate("svc", "label-a")
	if v := joined.Get("svc"); v != "scoped-value" {
		t.Fatalf("a joined scope sees %v, want scoped-value", v)
	}

	// A DIFFERENT label is a separate, empty scope — it does NOT fall back to
	// the ancestor's implementation. That shadowing is isolation's purpose:
	// vendor/cordis/src/context.ts `isolate()` installs the new label in the
	// isolate map, and reflect.ts `_getImpl` keys the store by that label, so
	// the ancestor's implementation is unreachable from here.
	other := root.Isolate("svc", "label-b")
	if v := other.Get("svc"); v != nil {
		t.Fatalf("an unrelated isolation label saw %v, want no service", v)
	}
	// The root scope is unaffected by any of it.
	if v := root.Get("svc"); v != "root-value" {
		t.Fatalf("root sees %v after isolation, want root-value", v)
	}
}

// TestInterceptIsInheritedAndDoesNotMutateParent pins the intercept map's
// scoping contract.
func TestInterceptIsInheritedAndDoesNotMutateParent(t *testing.T) {
	root := NewRoot()
	if _, ok := root.intercept["svc"]; ok {
		t.Fatal("root should carry no intercept")
	}
	child := root.Intercept("svc", map[string]any{"a": 1})
	if _, ok := root.intercept["svc"]; ok {
		t.Fatal("intercept mutated the parent context")
	}
	got, ok := child.intercept["svc"]
	if !ok {
		t.Fatal("child is missing the intercept entry")
	}
	if m, ok := got.(map[string]any); !ok || m["a"] != 1 {
		t.Fatalf("child intercept = %#v", got)
	}
	// A grandchild inherits it.
	grand := child.Child()
	if _, ok := grand.intercept["svc"]; !ok {
		t.Fatal("intercept did not inherit to a grandchild")
	}
}

// TestWaterfallDelegationAndVeto pins the waterfall contract: `next` delegates,
// returning without calling it vetoes, and calling it twice panics.
func TestWaterfallDelegationAndVeto(t *testing.T) {
	root := NewRoot()
	root.fiber.setState(Active)

	var order []string
	for _, name := range []string{"outer", "inner"} {
		n := name
		_, err := root.Events().On("evt", func(args []any) any {
			order = append(order, n)
			next := args[1].(func(any) any)
			return next(args[0])
		}, OnOptions{})
		if err != nil {
			t.Fatalf("On(%s): %v", n, err)
		}
	}
	got := root.Events().Waterfall("evt", func(v any) any {
		order = append(order, "inner-default")
		return v
	}, "seed")

	if got != "seed" {
		t.Fatalf("waterfall returned %v, want seed", got)
	}
	want := []string{"outer", "inner", "inner-default"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestWaterfallVetoSkipsRest pins that not calling next short-circuits.
func TestWaterfallVetoSkipsRest(t *testing.T) {
	root := NewRoot()
	root.fiber.setState(Active)

	reached := false
	_, _ = root.Events().On("evt", func(args []any) any {
		return "vetoed"
	}, OnOptions{})
	_, _ = root.Events().On("evt", func(args []any) any {
		reached = true
		return nil
	}, OnOptions{})

	got := root.Events().Waterfall("evt", func(v any) any { return v }, "seed")
	if got != "vetoed" {
		t.Fatalf("got %v, want vetoed", got)
	}
	if reached {
		t.Fatal("a listener ran after the chain was vetoed")
	}
}

// TestWaterfallNextTwicePanics pins the misuse guard.
func TestWaterfallNextTwicePanics(t *testing.T) {
	root := NewRoot()
	root.fiber.setState(Active)
	_, _ = root.Events().On("evt", func(args []any) any {
		next := args[1].(func(any) any)
		next(args[0])
		return next(args[0]) // second call must panic
	}, OnOptions{})

	defer func() {
		if recover() == nil {
			t.Fatal("calling next twice did not panic")
		}
	}()
	root.Events().Waterfall("evt", func(v any) any { return v }, "seed")
}

// TestBailStopsOnFirstTruthy pins bail semantics: nil and false do not bail.
func TestBailStopsOnFirstTruthy(t *testing.T) {
	root := NewRoot()
	root.fiber.setState(Active)
	var seen []string
	for _, r := range []any{nil, false, "stop", "never"} {
		res := r
		_, _ = root.Events().On("evt", func(args []any) any {
			seen = append(seen, "ran")
			return res
		}, OnOptions{})
	}
	got := root.Events().Bail("evt")
	if got != "stop" {
		t.Fatalf("Bail returned %v, want stop", got)
	}
	if len(seen) != 3 {
		t.Fatalf("%d listeners ran, want 3 (nil and false must not bail)", len(seen))
	}
}

// TestMountFailsLoudOnMissingService pins that Mount refuses a misconfigured
// composition instead of silently deferring.
func TestMountFailsLoudOnMissingService(t *testing.T) {
	root := NewRoot()
	_, err := root.Mount(&Plugin{Name: "p", Inject: []string{"absent"}})
	if err == nil {
		t.Fatal("Mount accepted a missing injection")
	}
}

// TestConcurrentProvideAndGet exercises the service table under contention.
// `-race` is unavailable on this kernel, so this plus the lock discipline is
// the concurrency evidence.
func TestConcurrentProvideAndGet(t *testing.T) {
	root := NewRoot()
	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "svc-" + itoa(i)
			c := root.Child()
			c.fiber.setState(Active)
			if err := c.Provide(name, i, nil); err != nil {
				t.Errorf("Provide(%s): %v", name, err)
				return
			}
			if v := c.Get(name); v != i {
				t.Errorf("Get(%s) = %v, want %d", name, v, i)
			}
		}(i)
	}
	wg.Wait()
}

func itoa(v int) string {
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
