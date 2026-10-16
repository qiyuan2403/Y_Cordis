// Package loader re-implements, in Go, the DSH loader configuration dialect:
// the entry-list YAML shape (`yaml.JSON_SCHEMA` extended with the `!!js`
// scalar tag), the patch algorithm, and the entry tree.
//
// Behavior source: the official tree at packages/bundle/*/cordis.patch.yml and
// vendor/{include,loader}/src. Where this package differs, the official
// behavior wins; the golden tests pin the semantics.
package loader

import (
	"math"
	"strconv"
)

// Value is a resolved YAML value: nil, bool, int64, float64, string,
// []Value, or map[string]Value.
type Value = any

// resolveScalar maps a plain (untagged or !!str-less) scalar to its implicit
// type under yaml.JSON_SCHEMA: null -> bool -> int -> float -> string.
//
// The order and the accept sets mirror js-yaml's JSON_SCHEMA implicit list
// (lib/schema/json.js: null, bool, int, float, then failsafe str). The
// consequence that matters: `yes`/`no`/`on`/`off` are STRINGS (the YAML 1.1
// bool type is not in this schema), and timestamps are STRINGS (the timestamp
// type is not in this schema either).
func resolveScalar(s string) Value {
	if v, ok := resolveNull(s); ok {
		return v
	}
	if v, ok := resolveBool(s); ok {
		return v
	}
	if v, ok := resolveInt(s); ok {
		return v
	}
	if v, ok := resolveFloat(s); ok {
		return v
	}
	return s
}

// resolveNull mirrors js-yaml lib/type/null.js resolveYamlNull:
// `~`, `null`, `Null`, `NULL`, and the empty scalar.
func resolveNull(s string) (Value, bool) {
	switch s {
	case "~", "null", "Null", "NULL", "":
		return nil, true
	}
	return nil, false
}

// resolveBool mirrors js-yaml lib/type/bool.js resolveYamlBoolean:
// exactly true/True/TRUE and false/False/FALSE.
func resolveBool(s string) (Value, bool) {
	switch s {
	case "true", "True", "TRUE":
		return true, true
	case "false", "False", "FALSE":
		return false, true
	}
	return nil, false
}

// resolveInt mirrors js-yaml lib/type/int.js resolveYamlInteger: optional
// sign, then decimal, or `0b`/`0x`/`0o` prefixed bases. A bare leading `0`
// with more digits is DECIMAL (`010` -> 10), matching the official code path
// that falls through to base 10.
func resolveInt(s string) (Value, bool) {
	if s == "" {
		return nil, false
	}
	body := s
	neg := false
	if body[0] == '-' || body[0] == '+' {
		neg = body[0] == '-'
		body = body[1:]
	}
	if body == "" {
		return nil, false
	}

	base := 10
	digits := body
	if body[0] == '0' && len(body) > 1 {
		switch body[1] {
		case 'b':
			base, digits = 2, body[2:]
		case 'x':
			base, digits = 16, body[2:]
		case 'o':
			base, digits = 8, body[2:]
		}
	}
	if digits == "" {
		return nil, false
	}
	if !allDigits(digits, base) {
		return nil, false
	}
	// A leading zero that is not a base prefix stays decimal: the digits
	// check above already validated it as base 10.
	n, err := strconv.ParseInt(digits, base, 64)
	if err != nil {
		// Out of int64 range: js-yaml would still produce a JS number (a
		// float). Fall through to the float resolver instead of guessing.
		return nil, false
	}
	if neg {
		n = -n
	}
	return n, true
}

func allDigits(s string, base int) bool {
	for i := range len(s) {
		c := s[i]
		switch base {
		case 2:
			if c != '0' && c != '1' {
				return false
			}
		case 8:
			if c < '0' || c > '7' {
				return false
			}
		case 10:
			if c < '0' || c > '9' {
				return false
			}
		case 16:
			if !isHex(c) {
				return false
			}
		}
	}
	return true
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// resolveFloat mirrors js-yaml lib/type/float.js, whose YAML_FLOAT_PATTERN
// accepts:
//
//	[-+]?digits[.digits][eE[-+]digits]   |  [-+]?.digits[eE[-+]digits]
//	| [-+]?\.(?:inf|Inf|INF)             |  \.(?:nan|NaN|NAN)
//
// The asymmetries matter and are pinned by the adversarial golden corpus:
// `.inf` accepts a sign, `.nan` does NOT, and only those exact case spellings
// are special — `.iNF` and `.nAn` are plain strings.
func resolveFloat(s string) (Value, bool) {
	if s == "" {
		return nil, false
	}

	// Specials first, before any sign stripping: the nan branch has no sign in
	// its pattern, so `-.nan` must fall through to the string case.
	if isSignedInf(s) {
		if s[0] == '-' {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	if s == ".nan" || s == ".NaN" || s == ".NAN" {
		return math.NaN(), true
	}

	body := s
	neg := false
	if body[0] == '-' || body[0] == '+' {
		neg = body[0] == '-'
		body = body[1:]
	}

	if !isFloatLiteral(body) {
		return nil, false
	}
	f, err := strconv.ParseFloat(body, 64)
	if err != nil {
		return nil, false
	}
	if neg {
		f = -f
	}
	return f, true
}

// isSignedInf reports whether s is `[-+]?.inf`/`.Inf`/`.INF`.
func isSignedInf(s string) bool {
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	}
	return s == ".inf" || s == ".Inf" || s == ".INF"
}

// isFloatLiteral accepts `digits[.digits][e[sign]digits]` and
// `.digits[e[sign]digits]`, i.e. a form that needs a dot or an exponent to be
// a float rather than an int.
func isFloatLiteral(s string) bool {
	i := 0
	intDigits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		intDigits++
	}
	fracDigits := 0
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			fracDigits++
		}
	}
	if intDigits == 0 && fracDigits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		expDigits := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			expDigits++
		}
		if expDigits == 0 {
			return false
		}
	}
	return i == len(s)
}
