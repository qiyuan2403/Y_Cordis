package loader

import (
	"fmt"
	"strings"
	"testing"
)

// warnCollector captures skipped-patch diagnostics so tests can assert that a
// no-match patch warns rather than silently doing nothing.
type warnCollector struct{ msgs []string }

func (w *warnCollector) warn(msg string, args ...any) {
	w.msgs = append(w.msgs, fmt.Sprintf(strings.ReplaceAll(msg, "%C", "%v"), args...))
}

func mustParsePatches(t *testing.T, src string) []*Patch {
	t.Helper()
	v, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	p, err := ParsePatches(v)
	if err != nil {
		t.Fatalf("ParsePatches: %v", err)
	}
	return p
}

func ids(t *testing.T, data []Value) []string {
	t.Helper()
	var out []string
	for _, e := range data {
		obj, ok := e.(*Object)
		if !ok {
			t.Fatalf("entry is %T, want *Object", e)
		}
		id, _ := obj.Get("id")
		s, _ := id.(string)
		out = append(out, s)
	}
	return out
}

// TestApplyPatchesInsertRoot pins: an `insert` without an id appends to the
// root, in order.
func TestApplyPatchesInsertRoot(t *testing.T) {
	data := []Value{NewObject()}
	data[0].(*Object).Set("id", "a")
	patches := mustParsePatches(t, "- insert:\n    - id: b\n      name: pkg-b\n")
	var w warnCollector
	got := ApplyPatches(data, patches, w.warn)
	if diff := strings.Join(ids(t, got), ","); diff != "a,b" {
		t.Fatalf("ids = %q, want \"a,b\"", diff)
	}
	if len(w.msgs) != 0 {
		t.Fatalf("unexpected warnings: %v", w.msgs)
	}
}

// TestApplyPatchesDoesNotMutateInput pins the structuredClone contract: the
// caller's slice must be untouched, or a hot reload could never revert a patch.
func TestApplyPatchesDoesNotMutateInput(t *testing.T) {
	orig := NewObject()
	orig.Set("id", "a")
	orig.Set("config", NewObject())
	data := []Value{orig}
	patches := mustParsePatches(t, "- id: a\n  config:\n    x: 1\n")
	var w warnCollector
	got := ApplyPatches(data, patches, w.warn)

	cfg, _ := orig.Get("config")
	if obj, ok := cfg.(*Object); ok && obj.Len() != 0 {
		t.Fatalf("input mutated: original config = %v", obj.Keys())
	}
	gotCfg, _ := got[0].(*Object).Get("config")
	gotObj, ok := gotCfg.(*Object)
	if !ok || !gotObj.Has("x") {
		t.Fatalf("result config missing patched key: %#v", gotCfg)
	}
}

// TestApplyPatchesReplacesWholeField pins: a patch replaces the whole field,
// never merging into it.
func TestApplyPatchesReplacesWholeField(t *testing.T) {
	orig := NewObject()
	orig.Set("id", "a")
	oldCfg := NewObject()
	oldCfg.Set("keep", "gone")
	oldCfg.Set("drop", "gone")
	orig.Set("config", oldCfg)

	patches := mustParsePatches(t, "- id: a\n  config:\n    fresh: 1\n")
	var w warnCollector
	got := ApplyPatches([]Value{orig}, patches, w.warn)

	cfg, _ := got[0].(*Object).Get("config")
	obj := cfg.(*Object)
	if obj.Has("keep") || obj.Has("drop") {
		t.Fatalf("config was merged, not replaced: %v", obj.Keys())
	}
	if !obj.Has("fresh") {
		t.Fatalf("config missing new key: %v", obj.Keys())
	}
}

// TestApplyPatchesIndexesInsertedRows pins: a later patch in the SAME list can
// target a row an earlier patch inserted.
func TestApplyPatchesIndexesInsertedRows(t *testing.T) {
	patches := mustParsePatches(t, `
- insert:
    - id: new
      name: pkg
- id: new
  disabled: true
`)
	var w warnCollector
	got := ApplyPatches(nil, patches, w.warn)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	d, ok := got[0].(*Object).Get("disabled")
	if !ok || d != true {
		t.Fatalf("inserted row not patched by later patch: disabled=%#v", d)
	}
}

// TestApplyPatchesWarnsOnNoMatch pins: a patch matching nothing warns and is
// skipped, never an error.
func TestApplyPatchesWarnsOnNoMatch(t *testing.T) {
	orig := NewObject()
	orig.Set("id", "a")
	patches := mustParsePatches(t, "- id: missing\n  disabled: true\n")
	var w warnCollector
	got := ApplyPatches([]Value{orig}, patches, w.warn)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if len(w.msgs) != 1 || !strings.Contains(w.msgs[0], "missing") {
		t.Fatalf("warnings = %v, want one naming \"missing\"", w.msgs)
	}
}

// TestApplyPatchesNameMismatchSkips pins: a name that disagrees with the
// target skips the patch.
func TestApplyPatchesNameMismatchSkips(t *testing.T) {
	orig := NewObject()
	orig.Set("id", "a")
	orig.Set("name", "pkg-a")
	patches := mustParsePatches(t, "- id: a\n  name: pkg-other\n  disabled: true\n")
	var w warnCollector
	got := ApplyPatches([]Value{orig}, patches, w.warn)
	if _, ok := got[0].(*Object).Get("disabled"); ok {
		t.Fatal("patch applied despite name mismatch")
	}
	if len(w.msgs) != 1 || !strings.Contains(w.msgs[0], "name mismatch") {
		t.Fatalf("warnings = %v, want a name-mismatch warning", w.msgs)
	}
}

// TestApplyPatchesInsertIntoGroup pins: an `insert` WITH an id appends to that
// group's config, and a non-group target is refused.
func TestApplyPatchesInsertIntoGroup(t *testing.T) {
	group := NewObject()
	group.Set("id", "grp")
	group.Set("group", true)
	group.Set("config", []Value{})

	patches := mustParsePatches(t, "- insert:\n    - id: child\n      name: pkg\n  id: grp\n")
	var w warnCollector
	got := ApplyPatches([]Value{group}, patches, w.warn)
	cfg, _ := got[0].(*Object).Get("config")
	seq := cfg.([]Value)
	if len(seq) != 1 {
		t.Fatalf("group config has %d rows, want 1", len(seq))
	}

	// A non-group target with the SAME id must warn and skip.
	plain := NewObject()
	plain.Set("id", "grp")
	plain.Set("name", "pkg")
	w2 := warnCollector{}
	got2 := ApplyPatches([]Value{plain}, patches, w2.warn)
	if _, ok := got2[0].(*Object).Get("config"); ok {
		t.Fatal("insert into a non-group target was applied")
	}
	if len(w2.msgs) != 1 || !strings.Contains(w2.msgs[0], "not a group") {
		t.Fatalf("warnings = %v, want a not-a-group warning", w2.msgs)
	}
}

// TestApplyPatchesNonInsertRequiresID pins: a patch without `id` and without
// `insert` warns.
func TestApplyPatchesNonInsertRequiresID(t *testing.T) {
	patches := mustParsePatches(t, "- disabled: true\n")
	var w warnCollector
	ApplyPatches(nil, patches, w.warn)
	if len(w.msgs) != 1 || !strings.Contains(w.msgs[0], "id is required") {
		t.Fatalf("warnings = %v, want an id-required warning", w.msgs)
	}
}

// TestSortKeysOrdersIDNameThenRestConfigLast pins the write-back ordering that
// governs --dump-config output.
func TestSortKeysOrdersIDNameThenRestConfigLast(t *testing.T) {
	o := NewObject()
	o.Set("config", 1)
	o.Set("zzz", 2)
	o.Set("name", "n")
	o.Set("aaa", 3)
	o.Set("id", "i")
	o.Set("mmm", 4)
	o.SortKeys()
	got := strings.Join(o.Keys(), ",")
	if got != "id,name,aaa,mmm,zzz,config" {
		t.Fatalf("order = %q, want \"id,name,aaa,mmm,zzz,config\"", got)
	}
}

// TestParsePatchesAgainstShippedBundles pins that every shipped bundle's patch
// list parses, and reports the row count so a regression in the dialect shows
// up as a number change rather than a silent skip.
//
// The fixtures live in testdata/official/ INSIDE this repository. Reading them
// from a checkout outside the repo would make this assertion vanish in CI,
// which is exactly the silent-skip failure mode these tests exist to prevent.
func TestParsePatchesAgainstShippedBundles(t *testing.T) {
	cases := []struct {
		path string
		want int
	}{
		{"../testdata/official/base.cordis.patch.yml", 1},
		{"../testdata/official/web-app.cordis.patch.yml", 28},
	}
	for _, tc := range cases {
		src, err := osReadFile(tc.path)
		if err != nil {
			t.Fatalf("fixture %s: %v", tc.path, err)
		}
		v, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: Parse: %v", tc.path, err)
		}
		patches, err := ParsePatches(v)
		if err != nil {
			t.Fatalf("%s: ParsePatches: %v", tc.path, err)
		}
		if len(patches) != tc.want {
			t.Fatalf("%s: %d patches, want %d", tc.path, len(patches), tc.want)
		}
		// base is a single insert-everything layer; web-app is 26 id-targeted
		// overrides plus 2 insert rows. Both shapes must parse.
		var inserts, overrides int
		for _, p := range patches {
			if len(p.Insert) > 0 {
				inserts++
			} else {
				overrides++
			}
		}
		if inserts == 0 {
			t.Fatalf("%s: no insert rows parsed", tc.path)
		}
		if tc.want > 1 && overrides == 0 {
			t.Fatalf("%s: no id-targeted override rows parsed", tc.path)
		}
	}
}
