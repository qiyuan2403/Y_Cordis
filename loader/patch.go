package loader

import "fmt"

// Patch is one patch row applied to an entry list.
//
// The official type is `PatchOptions`; the shape is open (any extra key is an
// override), so unknown keys are carried in Extra and applied verbatim.
type Patch struct {
	ID     string
	Insert []Value
	Name   string
	Extra  *Object
}

// ParsePatches reads a patch list (the top-level array of a `cordis.patch.yml`).
func ParsePatches(v Value) ([]*Patch, error) {
	seq, ok := v.([]Value)
	if !ok {
		if v == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("loader: patch list must be a sequence, got %T", v)
	}
	out := make([]*Patch, 0, len(seq))
	for i, item := range seq {
		obj, ok := item.(*Object)
		if !ok {
			return nil, fmt.Errorf("loader: patch %d must be a mapping, got %T", i, item)
		}
		p := &Patch{Extra: NewObject()}
		for _, k := range obj.Keys() {
			val, _ := obj.Get(k)
			switch k {
			case "id":
				s, ok := val.(string)
				if !ok {
					return nil, fmt.Errorf("loader: patch %d id must be a string", i)
				}
				p.ID = s
			case "insert":
				seq, ok := val.([]Value)
				if !ok {
					return nil, fmt.Errorf("loader: patch %d insert must be a sequence", i)
				}
				p.Insert = seq
			case "name":
				s, ok := val.(string)
				if !ok {
					return nil, fmt.Errorf("loader: patch %d name must be a string", i)
				}
				p.Name = s
			default:
				p.Extra.Set(k, val)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// ApplyPatches applies patch lists to an entry list and returns a detached
// result.
//
// This is THE patch semantics of the dialect, shared by mounting and offline
// config tooling so a dump can never drift from what boots. Ported from
// vendor/include/src/index.ts `applyEntryPatches`:
//
//   - The input is never mutated (the official code `structuredClone`s it):
//     patching shared entry objects would bake earlier patch values into the
//     cached parse, so repeated application over hot-reloads could never
//     revert a removed or changed patch.
//   - An `insert` without `id` appends to the root; with `id` it appends to
//     the target group's `config`, and the target must be a group.
//   - Inserted entries are indexed as they are added, so a later patch in the
//     same list can target a row an earlier patch inserted.
//   - A patch that matches nothing warns and is skipped (never an error).
//   - `name`, when present, must equal the target's name or the patch is
//     skipped.
//   - Every other key replaces the target's whole field (no deep merge).
func ApplyPatches(data []Value, patches []*Patch, warn func(msg string, args ...any)) []Value {
	if len(patches) == 0 {
		return append([]Value(nil), data...)
	}
	data = cloneSeq(data)

	entryMap := map[string]*Object{}
	buildMap := func(entries []Value) {}
	buildMap = func(entries []Value) {
		for _, e := range entries {
			obj, ok := e.(*Object)
			if !ok {
				continue
			}
			if id, ok := obj.Get("id"); ok {
				if s, ok := id.(string); ok && s != "" {
					entryMap[s] = obj
				}
			}
			if g, ok := obj.Get("group"); ok {
				if isTruthy(g) {
					if cfg, ok := obj.Get("config"); ok {
						if seq, ok := cfg.([]Value); ok {
							buildMap(seq)
						}
					}
				}
			}
		}
	}
	buildMap(data)

	for _, patch := range patches {
		if len(patch.Insert) > 0 {
			// Clone the inserted rows before they enter the tree. The official
			// code relies on the patch list being re-parsed from YAML on every
			// application; a Go host that CACHES the parsed patches (the normal
			// case for a hot-reloaded config) would otherwise mutate them here,
			// and the mutation would leak into every later application.
			insert := cloneSeq(patch.Insert)
			if patch.ID != "" {
				target, ok := entryMap[patch.ID]
				if !ok {
					warn("patch insert: entry %C not found", patch.ID)
					continue
				}
				g, _ := target.Get("group")
				if !isTruthy(g) {
					warn("patch insert: entry %C is not a group", patch.ID)
					continue
				}
				cfg, _ := target.Get("config")
				seq, ok := cfg.([]Value)
				if !ok {
					seq = []Value{}
				}
				seq = append(seq, insert...)
				target.Set("config", seq)
			} else {
				data = append(data, insert...)
			}
			// Index what this patch added so a LATER patch in the same list can
			// target it. Patch lists compose one layer per source, and a layer
			// must be able to configure a row an earlier layer inserted.
			buildMap(insert)
			continue
		}

		if patch.ID == "" {
			warn("patch: id is required for non-insert patches")
			continue
		}

		target, ok := entryMap[patch.ID]
		if !ok {
			warn("patch: entry %C not found", patch.ID)
			continue
		}

		if patch.Name != "" {
			got, _ := target.Get("name")
			gotStr, _ := got.(string)
			if patch.Name != gotStr {
				warn("patch: name mismatch for %C (expected %C, got %C), skipping", patch.ID, gotStr, patch.Name)
				continue
			}
		}

		for _, k := range patch.Extra.Keys() {
			if k == "id" {
				continue
			}
			v, _ := patch.Extra.Get(k)
			target.Set(k, v)
		}
	}

	return data
}

func cloneSeq(in []Value) []Value {
	out := make([]Value, len(in))
	for i := range in {
		out[i] = CloneValue(in[i])
	}
	return out
}

func isTruthy(v Value) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	default:
		return true
	}
}
