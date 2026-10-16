package loader

import "strings"

// Object is an insertion-ordered string-keyed mapping.
//
// Key order is OBSERVABLE behavior in this dialect: the official loader's
// `sortKeys` reorders entry rows to `id`, `name`, ... , `config` before write
// back, and `dsh --dump-config` prints the composed tree. A plain Go map would
// silently lose that order (and `encoding/json` would sort keys), so every
// config object parsed here keeps its source order.
type Object struct {
	keys []string
	vals map[string]Value
}

// NewObject returns an empty ordered object.
func NewObject() *Object {
	return &Object{vals: map[string]Value{}}
}

// Len reports the number of members.
func (o *Object) Len() int { return len(o.keys) }

// Keys returns the member names in insertion order. The slice is a copy.
func (o *Object) Keys() []string { return append([]string(nil), o.keys...) }

// Get returns the member value and whether it is present.
func (o *Object) Get(key string) (Value, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Has reports whether the member is present.
func (o *Object) Has(key string) bool {
	_, ok := o.vals[key]
	return ok
}

// Set assigns a member, appending the key when it is new.
func (o *Object) Set(key string, value Value) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = value
}

// Delete removes a member, preserving the order of the rest.
func (o *Object) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Clone returns a deep copy. The official patch algorithm clones its input
// (`structuredClone`) so repeated application over a hot-reloaded config can
// never bake an earlier patch into the cached parse.
func (o *Object) Clone() *Object {
	out := NewObject()
	for _, k := range o.keys {
		out.Set(k, CloneValue(o.vals[k]))
	}
	return out
}

// CloneValue deep-copies a parsed value.
func CloneValue(v Value) Value {
	switch t := v.(type) {
	case *Object:
		return t.Clone()
	case []Value:
		out := make([]Value, len(t))
		for i := range t {
			out[i] = CloneValue(t[i])
		}
		return out
	default:
		return v
	}
}

// SortKeys reorders members to `id`, `name`, then the remaining keys sorted
// ascending, then `config` last.
//
// Mirrors the official `sortKeys(object, ['id','name'], ['config'])`: the
// prepended and appended groups keep their declared order, and the middle
// group sorts by `localeCompare`. Ordering governs dump and write-back output,
// so it must be reproducible.
func (o *Object) SortKeys() {
	const (
		prependKey1 = "id"
		prependKey2 = "name"
		appendKey   = "config"
	)
	var head, mid, tail []string
	for _, k := range o.keys {
		switch k {
		case prependKey1, prependKey2:
			head = append(head, k)
		case appendKey:
			tail = append(tail, k)
		default:
			mid = append(mid, k)
		}
	}
	// `localeCompare` with the default locale is a lexicographic comparison for
	// the ASCII key set this dialect uses.
	sortStrings(mid)
	// The prepend group follows the declared order, not the source order.
	ordered := make([]string, 0, len(o.keys))
	for _, k := range []string{prependKey1, prependKey2} {
		if o.Has(k) {
			ordered = append(ordered, k)
		}
	}
	_ = head
	ordered = append(ordered, mid...)
	if o.Has(appendKey) {
		ordered = append(ordered, appendKey)
	}
	o.keys = ordered
}

func sortStrings(s []string) {
	// Insertion sort: config objects are small (single-digit members) and this
	// avoids pulling in sort for one call site.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.Compare(s[j-1], s[j]) > 0; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
