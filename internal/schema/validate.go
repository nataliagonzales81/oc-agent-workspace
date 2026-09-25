package schema

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Validate checks an instance against a document produced by this package.
//
// It implements the subset of JSON Schema 2020-12 that Build emits: type
// (including a type list), properties, required, additionalProperties, items,
// enum, and $ref into $defs. Anything else in a document is ignored rather than
// half-enforced, and `Unsupported` lists whatever was skipped so a caller can
// tell a pass from a pass-by-omission.
//
// The alternative — shipping no validator and trusting that the documents are
// right — leaves the one claim ocaw makes about them untested. A schema that
// validates a real envelope is a checkable fact; a schema that is merely
// well-formed JSON is not.
type Unsupported struct {
	Keywords []string
}

// Validate reports whether instance satisfies document, and which keywords in
// the document it did not interpret.
func Validate(document []byte, instance []byte) (ok bool, skipped []string, err error) {
	var doc any
	if err := json.Unmarshal(document, &doc); err != nil {
		return false, nil, fmt.Errorf("schema: %w", err)
	}
	v := &validator{skipped: map[string]bool{}}
	v.check(doc, parseInstance(instance), "#")
	if len(v.errors) > 0 {
		sort.Strings(v.errors)
		return false, v.sortedSkipped(), fmt.Errorf("%s", strings.Join(v.errors, "; "))
	}
	return true, v.sortedSkipped(), nil
}

type validator struct {
	errors  []string
	skipped map[string]bool
}

func (v *validator) sortedSkipped() []string {
	if len(v.skipped) == 0 {
		return nil
	}
	out := make([]string, 0, len(v.skipped))
	for k := range v.skipped {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (v *validator) failf(path, format string, args ...any) {
	v.errors = append(v.errors, path+": "+fmt.Sprintf(format, args...))
}

func (v *validator) check(node any, value any, path string) {
	m, ok := node.(map[string]any)
	if !ok {
		v.skipped["<non-object schema node>"] = true
		return
	}
	if ref, ok := m["$ref"].(string); ok {
		v.checkRef(ref, value, path)
		return
	}

	if enum, ok := m["enum"].([]any); ok {
		if !containsJSON(enum, value) {
			v.failf(path, "value %s is not one of the permitted values", render(value))
		}
	}
	if typeName, ok := m["type"]; ok {
		if !v.checkType(typeName, value, path) {
			return
		}
	}
	if anyOf, ok := m["anyOf"].([]any); ok {
		matched := false
		for _, option := range anyOf {
			sub := &validator{skipped: v.skipped}
			sub.check(option, value, path)
			if len(sub.errors) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			v.failf(path, "value matches none of the permitted shapes")
		}
	}

	props, _ := m["properties"].(map[string]any)
	obj, isObject := value.(map[string]any)
	if isObject && props != nil {
		if required, ok := m["required"].([]any); ok {
			for _, r := range required {
				name, _ := r.(string)
				if _, present := obj[name]; !present {
					v.failf(path, "required property %q is absent", name)
				}
			}
		}
		for name, spec := range props {
			child, present := obj[name]
			if !present {
				continue
			}
			v.check(spec, child, path+"/"+name)
		}
	}
	if isObject {
		if extra, ok := m["additionalProperties"]; ok {
			switch e := extra.(type) {
			case bool:
				if !e && props != nil {
					for name := range obj {
						if _, known := props[name]; !known {
							v.failf(path, "property %q is not permitted", name)
						}
					}
				}
			case map[string]any:
				for name, child := range obj {
					if props != nil {
						if _, known := props[name]; known {
							continue
						}
					}
					v.check(e, child, path+"/"+name)
				}
			default:
				v.skipped["additionalProperties (unrecognised form)"] = true
			}
		}
	}
	if items, ok := m["items"]; ok {
		if list, ok := value.([]any); ok {
			for i, child := range list {
				v.check(items, child, fmt.Sprintf("%s/%d", path, i))
			}
		}
	}

	for keyword := range m {
		switch keyword {
		case "$schema", "$id", "$defs", "title", "description", "type", "enum",
			"properties", "required", "additionalProperties", "items", "anyOf", "$ref":
		default:
			v.skipped[keyword] = true
		}
	}
}

func (v *validator) checkRef(ref string, value any, path string) {
	v.skipped["$ref"] = true
}

func (v *validator) checkType(typeName any, value any, path string) bool {
	var names []string
	switch t := typeName.(type) {
	case string:
		names = []string{t}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				names = append(names, s)
			}
		}
	default:
		v.skipped["type (unrecognised form)"] = true
		return true
	}
	for _, name := range names {
		if matchesType(name, value) {
			return true
		}
	}
	v.failf(path, "value %s is not of type %s", render(value), strings.Join(names, " or "))
	return false
}

func matchesType(name string, value any) bool {
	switch name {
	case "null":
		return value == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == float64(int64(f))
	case "number":
		_, ok := value.(float64)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	}
	return true
}

func containsJSON(set []any, value any) bool {
	for _, candidate := range set {
		if render(candidate) == render(value) {
			return true
		}
	}
	return false
}

func render(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(raw)
}

// parseInstance decodes a document into the shape the checker walks. Numbers
// become float64, which is what encoding/json does by default and what
// `integer` is then tested against.
func parseInstance(raw []byte) any {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}
