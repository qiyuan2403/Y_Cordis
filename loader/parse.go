package loader

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// JsExpr is a `!!js` scalar preserved verbatim.
//
// The official dialect declares it as
// `new yaml.Type('tag:yaml.org,2002:js', { kind: 'scalar', resolve: (d) => typeof d === 'string', construct: (d) => ({ __jsExpr: d }) })`
// over `yaml.JSON_SCHEMA`. The raw node stays in the options so write-back
// keeps the expression form instead of the evaluated value.
type JsExpr struct {
	Expr string
}

// Parse parses one entry-list document into the dialect's value model.
//
// The base schema is `yaml.JSON_SCHEMA` (NOT the default schema): `yes`/`no`
// are strings, timestamps are strings, and `010` is decimal 10. Ordering of
// mapping members is preserved.
func Parse(src []byte) (Value, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("loader: yaml: %w", err)
	}
	if doc.Kind == 0 {
		return nil, nil
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	return fromNode(doc.Content[0])
}

func fromNode(n *yaml.Node) (Value, error) {
	switch n.Kind {
	case yaml.MappingNode:
		obj := NewObject()
		for i := 0; i+1 < len(n.Content); i += 2 {
			keyNode := n.Content[i]
			valNode := n.Content[i+1]
			if keyNode.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("loader: non-scalar mapping key at line %d", keyNode.Line)
			}
			key, err := scalarString(keyNode)
			if err != nil {
				return nil, err
			}
			val, err := fromNode(valNode)
			if err != nil {
				return nil, err
			}
			obj.Set(key, val)
		}
		return obj, nil
	case yaml.SequenceNode:
		out := make([]Value, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fromNode(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, fmt.Errorf("loader: dangling alias at line %d", n.Line)
		}
		return fromNode(n.Alias)
	case yaml.ScalarNode:
		return fromScalar(n)
	default:
		return nil, fmt.Errorf("loader: unsupported node kind %d at line %d", n.Kind, n.Line)
	}
}

// scalarString reads a mapping key. Keys in this dialect are plain names, but
// a quoted or explicitly tagged key still resolves to its literal text.
func scalarString(n *yaml.Node) (string, error) {
	if n.Tag == "!!js" {
		return "", fmt.Errorf("loader: !!js is not a valid mapping key (line %d)", n.Line)
	}
	return n.Value, nil
}

func fromScalar(n *yaml.Node) (Value, error) {
	// `!!js` is the dialect's one extension over JSON_SCHEMA.
	if n.Tag == "!!js" || n.Tag == "tag:yaml.org,2002:js" {
		return JsExpr{Expr: n.Value}, nil
	}

	// A styled scalar is taken at its word: the author quoted it or wrote an
	// explicit tag, so it is the literal text. JSON_SCHEMA's implicit
	// resolvers only run on PLAIN scalars.
	//
	// The test is `Style`, not the inferred `Tag`: yaml.v3 resolves plain
	// scalars under its own schema, so `yes` arrives with Tag "!!str" even
	// though the author wrote it plain. Keying off the tag would skip the
	// resolvers entirely and make them unreachable.
	if n.Style != 0 {
		return n.Value, nil
	}

	// Plain scalar: resolve from the literal text under JSON_SCHEMA's rules.
	// yaml.v3's own guesses are the DEFAULT schema's (`2026-01-01` becomes
	// !!timestamp, which JSON_SCHEMA does not define), so the tag is only
	// consulted to reject a tag this dialect does not know.
	switch n.Tag {
	case "!!null", "!!bool", "!!int", "!!float", "!!timestamp", "!!str", "":
		return resolveScalar(n.Value), nil
	default:
		return nil, fmt.Errorf("loader: unsupported tag %q at line %d", n.Tag, n.Line)
	}
}

// Format renders a value back to the dialect's YAML text, with `!!js` scalars
// printed verbatim as the official writer does.
func Format(v Value) string {
	var b strings.Builder
	writeValue(&b, v, 0)
	return b.String()
}

func writeValue(b *strings.Builder, v Value, indent int) {
	pad := strings.Repeat("  ", indent)
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case int64:
		fmt.Fprintf(b, "%d", t)
	case float64:
		fmt.Fprintf(b, "%v", t)
	case string:
		fmt.Fprintf(b, "%q", t)
	case JsExpr:
		b.WriteString("!!js ")
		b.WriteString(t.Expr)
	case []Value:
		for i, item := range t {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(pad)
			b.WriteString("- ")
			writeValue(b, item, indent+1)
		}
	case *Object:
		for i, k := range t.keys {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString(pad)
			b.WriteString(k)
			b.WriteString(": ")
			writeValue(b, t.vals[k], indent+1)
		}
	default:
		fmt.Fprintf(b, "%v", t)
	}
}
