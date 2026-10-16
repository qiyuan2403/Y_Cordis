package loader

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenNode is the order-preserving tagged form emitted by
// scripts/gen-golden.mjs from the OFFICIAL parser (js-yaml + the `!!js` type
// declared in vendor/include/src/index.ts).
type goldenNode struct {
	T string          `json:"t"`
	V json.RawMessage `json:"v"`
}

// TestParseMatchesOfficialGolden parses each vendored official bundle (and a
// scalar edge-case fixture) with this package and compares the result — value
// by value AND key order by key order — against the official parser's output.
// A mismatch means the Go dialect has drifted from the one the loader mounts.
//
// The bundle fixtures are copied into testdata/official/ so the test never
// depends on a checkout outside the repository: a missing fixture must FAIL,
// not skip, or the load-bearing comparison would silently vanish in CI.
func TestParseMatchesOfficialGolden(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"base", "../testdata/official/base.cordis.patch.yml"},
		{"web-app", "../testdata/official/web-app.cordis.patch.yml"},
		{"sdk-app", "../testdata/official/sdk-app.cordis.patch.yml"},
		{"headless", "../testdata/official/headless.cordis.patch.yml"},
		{"scalars", "../testdata/scalars.yml"},
		{"adversarial", "../testdata/adversarial.yml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, err := os.ReadFile(tc.src)
			if err != nil {
				t.Fatalf("fixture %s: %v", tc.src, err)
			}
			got, err := Parse(src)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join("..", "testdata", "golden", tc.name+".json"))
			if err != nil {
				t.Fatalf("golden: %v", err)
			}
			var want goldenNode
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("golden decode: %v", err)
			}
			compare(t, tc.name, "", got, want)
		})
	}
}

func compare(t *testing.T, path, key string, got Value, want goldenNode) {
	t.Helper()
	at := path
	if key != "" {
		at = path + "." + key
	}
	switch want.T {
	case "null":
		if got != nil {
			t.Fatalf("%s: got %#v, want null", at, got)
		}
	case "bool":
		var b bool
		mustUnmarshal(t, want.V, &b)
		gb, ok := got.(bool)
		if !ok || gb != b {
			t.Fatalf("%s: got %#v, want bool %v", at, got, b)
		}
	case "num":
		// One number type on both sides: compare numerically against the
		// official decimal spelling.
		var s string
		mustUnmarshal(t, want.V, &s)
		gf, ok := toFloat(got)
		if !ok {
			t.Fatalf("%s: got %T, want number (%s)", at, got, s)
		}
		if !numberEqual(gf, s) {
			t.Fatalf("%s: got %v, want %s", at, gf, s)
		}
	case "str":
		var s string
		mustUnmarshal(t, want.V, &s)
		gs, ok := got.(string)
		if !ok || gs != s {
			t.Fatalf("%s: got %#v, want string %q", at, got, s)
		}
	case "js":
		var s string
		mustUnmarshal(t, want.V, &s)
		gj, ok := got.(JsExpr)
		if !ok {
			t.Fatalf("%s: got %T, want JsExpr", at, got)
		}
		if gj.Expr != s {
			t.Fatalf("%s: got !!js %q, want %q", at, gj.Expr, s)
		}
	case "seq":
		var items []goldenNode
		mustUnmarshal(t, want.V, &items)
		gs, ok := got.([]Value)
		if !ok {
			t.Fatalf("%s: got %T, want sequence", at, got)
		}
		if len(gs) != len(items) {
			t.Fatalf("%s: got %d items, want %d", at, len(gs), len(items))
		}
		for i := range items {
			compare(t, at, "["+itoa(i)+"]", gs[i], items[i])
		}
	case "map":
		var entries [][]json.RawMessage
		mustUnmarshal(t, want.V, &entries)
		obj, ok := got.(*Object)
		if !ok {
			t.Fatalf("%s: got %T, want *Object", at, got)
		}
		if obj.Len() != len(entries) {
			t.Fatalf("%s: got %d members (%v), want %d", at, obj.Len(), obj.Keys(), len(entries))
		}
		// Key ORDER is observable behavior (sortKeys / --dump-config).
		for i, pair := range entries {
			var k string
			mustUnmarshal(t, pair[0], &k)
			gotKey := obj.Keys()[i]
			if gotKey != k {
				t.Fatalf("%s: member %d key = %q, want %q (got order %v)", at, i, gotKey, k, obj.Keys())
			}
			v, _ := obj.Get(k)
			var child goldenNode
			mustUnmarshal(t, pair[1], &child)
			compare(t, at, k, v, child)
		}
	default:
		t.Fatalf("%s: unknown golden type %q", at, want.T)
	}
}

func mustUnmarshal(t *testing.T, raw json.RawMessage, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s: %v", string(raw), err)
	}
}

func toFloat(v Value) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case float64:
		return t, true
	default:
		return 0, false
	}
}

func numberEqual(got float64, want string) bool {
	switch want {
	case "Infinity":
		return got > 1e308
	case "-Infinity":
		return got < -1e308
	case "NaN":
		return got != got
	}
	var w float64
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		return false
	}
	return got == w
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

// TestFormatRoundTripsJSVerbatim pins the write-back contract: a `!!js` node
// prints as the raw expression, never as its evaluated value. The official
// `--dump-config` relies on this.
func TestFormatRoundTripsJSVerbatim(t *testing.T) {
	src := []byte("a: !!js process.env.DSH_DUMP_SPEC\nb: 1\n")
	got, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	out := Format(got)
	if !strings.Contains(out, "!!js process.env.DSH_DUMP_SPEC") {
		t.Fatalf("!!js not printed verbatim: %q", out)
	}
}
