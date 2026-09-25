package yaml

import (
	"strconv"
	"strings"
)

// The accessors below are the only place a scalar becomes a Go value other than
// text. They all fail loudly: a field that is absent, of the wrong type, or of an
// unrecognised spelling is a typed error, never a zero value. A silently defaulted
// field is how a corrupted manifest turns into a wrong-but-plausible one.

// kindName names a parsed value's type for an error message, so the caller learns
// which construct was found instead of just that decoding failed.
func kindName(v any) string {
	switch v.(type) {
	case *Map:
		return "a mapping"
	case Seq:
		return "a list"
	case string:
		return "a scalar"
	case nil:
		return "nothing"
	}
	return "an unsupported value"
}

// checkKeys rejects any key the target type does not declare. Dropping it would
// be the silent field loss this package exists to prevent. The reported line is
// the offending key's own line, so a hand-edited manifest points at the field to
// fix rather than at the top of the block.
func checkKeys(m *Map, path string, known ...string) error {
	for _, k := range m.Order() {
		if !contains(known, k) {
			return errf(m.LineOf(k), "%s: unknown field %q (expected one of: %s)", path, k, strings.Join(known, ", "))
		}
	}
	return nil
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func mustMap(v any, path string) (*Map, error) {
	if v == nil {
		return nil, errf(0, "%s: expected a mapping, got an empty document", path)
	}
	m, ok := v.(*Map)
	if !ok {
		return nil, errf(0, "%s: expected a mapping, got %s", path, kindName(v))
	}
	return m, nil
}

// str reads a required string field.
func str(m *Map, key, path string) (string, error) {
	v, ok := m.Get(key)
	if !ok || v == nil {
		return "", errf(m.Line, "%s: missing required field %q", path, key)
	}
	s, ok := v.(string)
	if !ok {
		return "", errf(m.Line, "%s: field %q must be a scalar, got %s", path, key, kindName(v))
	}
	return s, nil
}

// optStr reads an optional string field, defaulting to "" when absent.
func optStr(m *Map, key, path string) (string, error) {
	v, ok := m.Get(key)
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", errf(m.Line, "%s: field %q must be a scalar, got %s", path, key, kindName(v))
	}
	return s, nil
}

// boolean reads a required bool field. Only true/false are accepted: the YAML 1.1
// spellings yes/no/on/off parse as booleans in some tools and as strings in others,
// so accepting them would reintroduce exactly the ambiguity this subset avoids.
func boolean(m *Map, key, path string) (bool, error) {
	s, err := str(m, key, path)
	if err != nil {
		return false, err
	}
	return parseBool(s, m.Line, path, key)
}

func optBool(m *Map, key, path string, def bool) (bool, error) {
	v, ok := m.Get(key)
	if !ok || v == nil {
		return def, nil
	}
	s, ok := v.(string)
	if !ok {
		return false, errf(m.Line, "%s: field %q must be true or false, got %s", path, key, kindName(v))
	}
	return parseBool(s, m.Line, path, key)
}

func parseBool(s string, line int, path, key string) (bool, error) {
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errf(line, "%s: field %q must be true or false, got %q", path, key, s)
}

// stringList reads a required field as a list of scalars, used for the
// knowledge index's `supersedes`.
func stringList(m *Map, key, path string) ([]string, error) {
	v, ok := m.Get(key)
	if !ok {
		return nil, errf(m.Line, "%s: missing required field %q", path, key)
	}
	seq, ok := v.(Seq)
	if !ok {
		return nil, errf(m.Line, "%s: field %q must be a list, got %s", path, key, kindName(v))
	}
	out := make([]string, 0, len(seq))
	for i, item := range seq {
		s, ok := item.(string)
		if !ok {
			return nil, errf(m.Line, "%s: field %q item %d must be a scalar, got %s", path, key, i, kindName(item))
		}
		out = append(out, s)
	}
	return out, nil
}

// enum checks a string against a closed set, naming the set on failure so the
// error is actionable without a doc lookup.
func enum(m *Map, key, path string, allowed ...string) (string, error) {
	s, err := str(m, key, path)
	if err != nil {
		return "", err
	}
	if !contains(allowed, s) {
		return "", errf(m.Line, "%s: field %q must be one of: %s (got %q)", path, key, strings.Join(allowed, "|"), s)
	}
	return s, nil
}

// --- writer helpers ---
//
// kv* render one entry's body without leading padding, and kv wraps that with
// indentation. Both exist because a sequence item's first key sits on the dash
// line and its remaining keys sit at the item's own indent, so a renderer that
// always owned its padding could not produce either shape.

// kvBody renders "key: value\n" with no leading padding.
func kvBody(key, val string) string {
	return key + ": " + quoteIfNeeded(val) + "\n"
}

// kvbBody renders a boolean entry's body, always bare so the file stays readable
// as YAML rather than as a list of quoted words.
func kvbBody(key string, val bool) string {
	return key + ": " + strconv.FormatBool(val) + "\n"
}

// kv renders a scalar entry at the given indent, quoting the value only if it
// would otherwise re-read as a different type.
func kv(indent int, key, val string) string { return pad(indent) + kvBody(key, val) }

// kvb renders a boolean entry at the given indent.
func kvb(indent int, key string, val bool) string { return pad(indent) + kvbBody(key, val) }

// kvSeq renders a list of scalars, using the `[]` spelling when it is empty so
// the result matches the spec's own fixtures byte for byte.
func kvSeq(indent int, key string, vals []string) string {
	if len(vals) == 0 {
		return pad(indent) + key + ": []\n"
	}
	var b strings.Builder
	b.WriteString(pad(indent) + key + ":\n")
	for _, v := range vals {
		b.WriteString(pad(indent+2) + "- " + quoteIfNeeded(v) + "\n")
	}
	return b.String()
}

func pad(indent int) string { return strings.Repeat(" ", indent) }
