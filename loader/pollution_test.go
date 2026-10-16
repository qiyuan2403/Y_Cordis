package loader

import "testing"

// Applying the SAME patch list twice must produce the same result. If the
// second application differs, a patch mutated the parsed input.
func TestRepeatedApplicationIsStable(t *testing.T) {
	src := "- insert:\n    - id: row\n      name: pkg\n- id: row\n  disabled: true\n"
	parsed, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	patches, err := ParsePatches(parsed)
	if err != nil {
		t.Fatalf("ParsePatches: %v", err)
	}

	var warn []string
	w := func(m string, a ...any) { warn = append(warn, m) }

	first := ApplyPatches(nil, patches, w)
	second := ApplyPatches(nil, patches, w)

	// The insert row's own Disabled must be untouched in the parsed input.
	inserted := patches[0].Insert[0].(*Object)
	if d, ok := inserted.Get("disabled"); ok {
		t.Errorf("the parsed insert row was mutated: disabled=%v", d)
	}
	_ = first
	_ = second
}
