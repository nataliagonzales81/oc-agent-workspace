// Package schema builds JSON Schema documents from Go types.
//
// The schemas ocaw ships are derived from the very structs the encoder writes,
// not maintained beside them. A hand-written schema for a payload is a second
// statement of the same contract, and two statements of one contract disagree:
// silently, in whichever direction the test suite happens to check. Here the
// schema is a projection of the struct, so a field that is added, renamed,
// retyped, or made optional shows up in the schema without anyone editing it.
//
// The output is a subset of JSON Schema 2020-12: enough to validate, not enough
// to be a framework. Stdlib only (SPEC §9.1), no dependencies.
package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Node is one schema node. The zero value is not a valid schema; build them with
// Build. Fields the JSON Schema specification does not use are unexported
// controls rather than serialised.
type Node struct {
	Type        string           `json:"type,omitempty"`
	Description string           `json:"description,omitempty"`
	Properties  map[string]*Node `json:"properties,omitempty"`
	Required    []string         `json:"required,omitempty"`
	Items       *Node            `json:"items,omitempty"`
	Enum        []string         `json:"enum,omitempty"`
	Defs        map[string]*Node `json:"$defs,omitempty"`
	Ref         string           `json:"$ref,omitempty"`

	// Additional is emitted as `additionalProperties`. A nil pointer means the
	// key is absent, which is not the same as `false`, so the three states —
	// absent, free, and typed — have to be distinguishable.
	Additional *Node `json:"additionalProperties,omitempty"`
	// AdditionalClosed emits `additionalProperties: false`, the one value a
	// *Node cannot express. An object that describes every property it has and
	// refuses the rest is a different statement from one that says nothing.
	AdditionalClosed bool `json:"-"`

	// AnyOf carries "this, or anything at all" — the honest description of a
	// value ocaw deliberately tolerates rather than enforces.
	AnyOf []*Node `json:"anyOf,omitempty"`

	// nullable adds "null" to the type list. A pointer may be null, and a
	// caller that has to guess whether it is has to guess wrong half the time.
	nullable bool
	// free is the `{}` schema: anything goes.
	free bool
	// seen and stack detect a recursive type.
	seen  map[reflect.Type]bool
	stack []string
}

// Override replaces the schema for a named type. It exists for the closed
// vocabularies — a task status is one of five values, and a validator should be
// able to say so — and for the values ocaw deliberately does *not* constrain.
type Override struct {
	// Schema, when non-empty, replaces the whole node.
	Schema *Node
	// Enum constrains a string type to a set, without replacing its description.
	Enum []string
	// Type overrides just the JSON type name.
	Type string
}

// Builder generates schemas, with optional per-type overrides.
type Builder struct {
	overrides map[reflect.Type]Override
}

// NewBuilder returns a Builder. The overrides map may be mutated afterwards, so
// a caller can register the vocabularies it owns.
func NewBuilder() *Builder {
	return &Builder{overrides: map[reflect.Type]Override{}}
}

// Override registers a replacement for a type. The key is a zero value of the
// type, not a reflect.Type, so callers do not have to spell out reflect.Zero.
func (b *Builder) Override(sample any, o Override) {
	if b.overrides == nil {
		b.overrides = map[reflect.Type]Override{}
	}
	b.overrides[reflect.TypeOf(sample)] = o
}

// Enum registers a closed vocabulary for a named string type.
func (b *Builder) Enum(sample any, values ...string) {
	b.Override(sample, Override{Enum: values})
}

// Build returns the schema for t. A type containing itself is an error rather
// than a permissive node: a schema that quietly accepts anything where a
// structure was promised is a lie the caller cannot detect, and the honest
// response to "this has no finite schema" is to refuse.
func (b *Builder) Build(t reflect.Type) (*Node, error) {
	return b.build(t, map[reflect.Type]bool{}, nil)
}

func (b *Builder) build(t reflect.Type, seen map[reflect.Type]bool, stack []string) (*Node, error) {
	if b.overrides != nil {
		if o, ok := b.overrides[t]; ok {
			node := o.Schema
			if node == nil {
				node = &Node{Type: o.Type}
			}
			if len(o.Enum) > 0 {
				node = cloneNode(node)
				node.Enum = append([]string(nil), o.Enum...)
				if node.Type == "" {
					node.Type = "string"
				}
			}
			return node, nil
		}
	}
	if b == nil {
		b = NewBuilder()
	}

	// any, and anything whose contents ocaw does not constrain.
	if t == nil || t.Kind() == reflect.Interface {
		return &Node{free: true}, nil
	}

	switch t.Kind() {
	case reflect.Pointer:
		elem, err := b.build(t.Elem(), seen, stack)
		if err != nil {
			return nil, err
		}
		// A pointer is the only reason a field can be null.
		out := cloneNode(elem)
		out.nullable = true
		return out, nil
	case reflect.Slice, reflect.Array:
		items, err := b.build(t.Elem(), seen, stack)
		if err != nil {
			return nil, err
		}
		return &Node{Type: "array", Items: items}, nil
	case reflect.Map:
		values, err := b.build(t.Elem(), seen, stack)
		if err != nil {
			return nil, err
		}
		// Keys are always strings in JSON, so only the value type is constrained.
		return &Node{Type: "object", Additional: values}, nil
	case reflect.String:
		return &Node{Type: "string"}, nil
	case reflect.Bool:
		return &Node{Type: "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return &Node{Type: "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return &Node{Type: "number"}, nil
	case reflect.Struct:
		return b.buildStruct(t, seen, stack)
	}
	return nil, fmt.Errorf("no schema for %s (%s): add an override or stop encoding it", t, t.Kind())
}

func (b *Builder) buildStruct(t reflect.Type, seen map[reflect.Type]bool, stack []string) (*Node, error) {
	if seen[t] {
		return nil, fmt.Errorf("%s is recursive, so it has no finite JSON Schema; flatten it or mark it with an override", strings.Join(append(stack, t.String()), " -> "))
	}
	seen[t] = true
	defer delete(seen, t)

	node := &Node{Type: "object", Additional: &Node{free: true}, Properties: map[string]*Node{}}
	required := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)

		// An embedded struct is promoted before the unexported check, because an
		// embedded field of unexported type still has a non-empty PkgPath and its
		// exported fields are still written by encoding/json. Skipping on PkgPath
		// first loses every one of them.
		if field.Anonymous {
			ft := field.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				name, _, _ := parseTag(field.Tag.Get("json"))
				if name == "" {
					inner, err := b.build(ft, seen, append(stack, t.String()))
					if err != nil {
						return nil, err
					}
					for k, v := range inner.Properties {
						node.Properties[k] = v
					}
					for _, k := range inner.Required {
						required[k] = true
					}
					continue
				}
			}
		}
		if field.PkgPath != "" {
			continue // unexported, and not a promotable embedded struct
		}
		name, opts, _ := parseTag(field.Tag.Get("json"))
		if name == "-" {
			continue
		}

		if name == "" {
			name = field.Name
		}
		child, err := b.build(field.Type, seen, append(stack, t.String()))
		if err != nil {
			return nil, err
		}
		node.Properties[name] = child
		if !opts.contains("omitempty") && !opts.contains("omitzero") {
			required[name] = true
		}
	}
	for name := range required {
		node.Required = append(node.Required, name)
	}
	sort.Strings(node.Required)
	return node, nil
}

type tagOptions string

func (o tagOptions) contains(want string) bool {
	s := string(o)
	for s != "" {
		var part string
		part, s, _ = strings.Cut(s, ",")
		if part == want {
			return true
		}
	}
	return false
}

// parseTag splits a json struct tag the way encoding/json does, including the
// documented rule that a malformed name yields an empty name. A tag with no
// options still names a field: `json:"blocked"` has no comma and is not absent.
func parseTag(tag string) (name string, opts tagOptions, hasTag bool) {
	if tag == "" {
		return "", "", false
	}
	raw, rawOpts, _ := strings.Cut(tag, ",")
	if !validTagName(raw) {
		return "", tagOptions(rawOpts), true
	}
	return raw, tagOptions(rawOpts), true
}

func validTagName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func cloneNode(n *Node) *Node {
	if n == nil {
		return nil
	}
	out := *n
	return &out
}

// MarshalJSON renders the node, applying the two things a plain struct tag
// cannot express: the `{}` free schema, and a nullable type list.
func (n *Node) MarshalJSON() ([]byte, error) {
	if n == nil {
		return []byte("{}"), nil
	}
	if n.free {
		return []byte("{}"), nil
	}
	// An explicit Ref wins: it is the whole node.
	if n.Ref != "" {
		return json.Marshal(struct {
			Ref         string `json:"$ref"`
			Description string `json:"description,omitempty"`
		}{n.Ref, n.Description})
	}

	out := map[string]any{}
	if n.Type != "" {
		if n.nullable {
			out["type"] = []string{n.Type, "null"}
		} else {
			out["type"] = n.Type
		}
	}
	if n.Description != "" {
		out["description"] = n.Description
	}
	if len(n.Enum) > 0 {
		out["enum"] = n.Enum
	}
	if n.Properties != nil {
		out["properties"] = n.Properties
	}
	if len(n.Required) > 0 {
		out["required"] = n.Required
	}
	if n.AdditionalClosed {
		out["additionalProperties"] = false
	} else if n.Additional != nil {
		if n.Additional.free {
			// A free-form map is best expressed as no constraint at all, which
			// is different from `additionalProperties: {}` only in emphasis.
			out["additionalProperties"] = true
		} else {
			out["additionalProperties"] = n.Additional
		}
	}
	if n.Items != nil {
		out["items"] = n.Items
	}
	if len(n.AnyOf) > 0 {
		out["anyOf"] = n.AnyOf
	}
	if len(n.Defs) > 0 {
		out["$defs"] = n.Defs
	}
	// encoding/json sorts map keys, which would reorder properties. Properties
	// are a set, so that is harmless for validation but noisy for a human
	// reading the document, and a noisy document gets regenerated less often.
	return marshalOrdered(out)
}

func marshalOrdered(m map[string]any) ([]byte, error) {
	// A fixed key order keeps the bytes stable across runs, which is what lets
	// the committed documents be compared byte for byte.
	order := []string{
		"$schema", "$id", "title", "description", "type", "enum", "const",
		"properties", "required", "additionalProperties", "items", "anyOf", "$defs",
	}
	var b strings.Builder
	b.WriteByte('{')
	first := true
	write := func(key string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(jsonKey(key))
		b.WriteByte(':')
		b.Write(raw)
		return nil
	}
	for _, key := range order {
		if v, ok := m[key]; ok {
			if err := write(key, v); err != nil {
				return nil, err
			}
			delete(m, key)
		}
	}
	rest := make([]string, 0, len(m))
	for k := range m {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, key := range rest {
		if err := write(key, m[key]); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func jsonKey(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// Document is one shipped schema: the JSON Schema itself plus the metadata
// ocaw needs to find and version it.
type Document struct {
	// Key is how the document is addressed: `ocaw schema task.list`. One
	// document serves every alias that shares an envelope schema, so the aliases
	// list rather than duplicating the body.
	Keys []string
	// SchemaID is the value that appears in the envelope's `schema` field, in
	// the form `ocaw/<command>@<major>`.
	SchemaID string
	// Major is the `@major` of SchemaID, parsed out of it rather than stored
	// twice, so the two cannot disagree.
	Major int
	// Title and Description document the payload for a human reading the
	// document.
	Title       string
	Description string

	Body json.RawMessage
}

// Draft is the JSON Schema dialect these documents declare.
const Draft = "https://json-schema.org/draft/2020-12/schema"

// Assemble wraps a built node in a document: dialect, id, title, and the
// envelope's own schema id.
func Assemble(schemaID, title, description string, node *Node) (json.RawMessage, error) {
	body := map[string]any{
		"$schema":     Draft,
		"$id":         schemaID,
		"title":       title,
		"description": description,
	}
	raw, err := node.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var nodeMap map[string]any
	if err := json.Unmarshal(raw, &nodeMap); err != nil {
		return nil, err
	}
	for k, v := range nodeMap {
		if k == "type" || k == "description" {
			continue
		}
		body[k] = v
	}
	return marshalOrdered(body)
}

// MajorOf parses the `@major` out of a schema id, reporting 0 when there is
// none. `schema_max` is defined as the highest major present, so the parse has
// to be the same one the envelope's schema field implies.
func MajorOf(schemaID string) int {
	_, tail, found := strings.Cut(schemaID, "@")
	if !found {
		return 0
	}
	n := 0
	for _, r := range tail {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
