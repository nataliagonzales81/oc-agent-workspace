package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func reflectTypeOf(sample any) reflect.Type { return reflect.TypeOf(sample) }

type inner struct {
	A string `json:"a"`
	B int    `json:"b,omitempty"`
}

type outer struct {
	inner                         // embedded: promoted, not nested
	Name    string                `json:"name"`
	Ptr     *inner                `json:"ptr"`
	PtrList []*inner              `json:"ptr_list"`
	List    []inner               `json:"list"`
	Dict    map[string]int        `json:"dict"`
	Multi   map[string][]string   `json:"multi"`
	Free    map[string]any        `json:"free"`
	Any     any                   `json:"any"`
	Blank   string                // no tag: the field name is the key
	Skipped string                `json:"-"`
	Omitted string                `json:"omitted,omitempty"`
	Named   namedString           `json:"named"`
	Nested  struct{ Deep string } `json:"nested"`
	Anon    struct{ X, Y string } `json:"anon"`
	PtrInt  *int                  `json:"ptr_int"`
	private string
}

type namedString string

func build(t *testing.T, sample any, setup func(*Builder)) *Node {
	t.Helper()
	b := NewBuilder()
	if setup != nil {
		setup(b)
	}
	node, err := b.Build(reflectTypeOf(sample))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return node
}

func mustJSON(t *testing.T, node *Node) map[string]any {
	t.Helper()
	raw, err := node.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	return out
}

func TestEmbeddedStructFieldsArePromoted(t *testing.T) {
	// encoding/json flattens an embedded struct, so a schema with a property
	// named after the embedded type would describe a field the encoder never
	// writes and omit every field it does.
	props := mustJSON(t, build(t, outer{}, nil))["properties"].(map[string]any)
	for _, want := range []string{"a", "name", "b"} {
		if _, ok := props[want]; !ok {
			t.Errorf("properties is missing %q: %v", want, keysOf(props))
		}
	}
	if _, ok := props["inner"]; ok {
		t.Error("the embedded type appears as a property of its own")
	}
}

func TestJSONTagDrivesTheKeyAndTheRequiredSet(t *testing.T) {
	node := mustJSON(t, build(t, outer{}, nil))
	props := node["properties"].(map[string]any)
	required := map[string]bool{}
	for _, r := range node["required"].([]any) {
		required[r.(string)] = true
	}

	if _, ok := props["name"]; !ok {
		t.Errorf("the tag name was not used: %v", keysOf(props))
	}
	if _, ok := props["Blank"]; !ok {
		t.Errorf("an untagged field should keep its Go name: %v", keysOf(props))
	}
	if _, ok := props["-"]; ok {
		t.Error(`a field tagged json:"-" appeared in the schema`)
	}
	if _, ok := props["private"]; ok {
		t.Error("an unexported field appeared in the schema")
	}
	if required["omitted"] {
		t.Error("an omitempty field was marked required")
	}
	if !required["name"] {
		t.Error("a field without omitempty was not marked required")
	}
	// A promoted field keeps its own omitempty, so b is optional.
	if required["b"] {
		t.Error("a promoted omitempty field was marked required")
	}
}

func TestPointerFieldsMayBeNull(t *testing.T) {
	props := mustJSON(t, build(t, outer{}, nil))["properties"].(map[string]any)
	// A pointer field is a nullable object.
	for _, name := range []string{"ptr", "ptr_int"} {
		types, ok := props[name].(map[string]any)["type"].([]any)
		if !ok {
			t.Errorf("%s: type is %v, want a list including null", name, props[name])
			continue
		}
		if !contains(types, "null") {
			t.Errorf("%s: type list %v does not include null", name, types)
		}
	}
	// A slice of pointers is an array whose *items* may be null, not a nullable
	// array. The distinction matters: a nullable array is a type the encoder
	// cannot produce, because a nil slice marshals to [].
	list := props["ptr_list"].(map[string]any)
	if list["type"] != "array" {
		t.Errorf("ptr_list type = %v, want array", list["type"])
	}
	itemTypes, ok := list["items"].(map[string]any)["type"].([]any)
	if !ok || !contains(itemTypes, "null") {
		t.Errorf("ptr_list items = %v, want a nullable object", list["items"])
	}
	// A value field is not nullable: only a pointer can be absent.
	if got := props["name"].(map[string]any)["type"]; got != "string" {
		t.Errorf("name type = %v, want a plain string", got)
	}
}

func TestMapsAndFreeValues(t *testing.T) {
	props := mustJSON(t, build(t, outer{}, nil))["properties"].(map[string]any)

	// map[string]int constrains the values; keys are strings in JSON anyway.
	dict := props["dict"].(map[string]any)
	if dict["type"] != "object" {
		t.Errorf("dict type = %v", dict["type"])
	}
	if dict["additionalProperties"].(map[string]any)["type"] != "integer" {
		t.Errorf("dict values are not constrained: %v", dict["additionalProperties"])
	}
	// map[string][]string nests the array.
	multi := props["multi"].(map[string]any)["additionalProperties"].(map[string]any)
	if multi["type"] != "array" {
		t.Errorf("multi values are not arrays: %v", multi)
	}
	// map[string]any is genuinely unconstrained, and says so with `true`
	// rather than with an empty object that looks like a mistake.
	if got, ok := props["free"].(map[string]any)["additionalProperties"]; !ok || got != true {
		t.Errorf("free = %v, want additionalProperties true", props["free"])
	}
	// A bare any is the `{}` schema: everything, stated plainly.
	if got := mustJSON(t, build(t, struct {
		V any `json:"v"`
	}{}, nil))["properties"].(map[string]any)["v"]; len(got.(map[string]any)) != 0 {
		t.Errorf("an any field produced %v, want {}", got)
	}
}

// A recursive type has no finite schema. Saying so is the honest answer; a
// permissive node would be a schema that validates anything where a structure
// was promised, and the caller could not tell.
func TestRecursiveTypeIsRefused(t *testing.T) {
	type node struct {
		Child *node `json:"child"`
	}
	_, err := NewBuilder().Build(reflectTypeOf(node{}))
	if err == nil {
		t.Fatal("Build accepted a recursive type")
	}
	if !strings.Contains(err.Error(), "recursive") {
		t.Errorf("error = %q, want it to name the reason", err)
	}
	// And an override is the documented way out.
	type pair struct{ A, B string }
	if _, err := NewBuilder().Build(reflectTypeOf(pair{})); err != nil {
		t.Errorf("a plain struct was refused: %v", err)
	}
}

func TestOverrides(t *testing.T) {
	node := mustJSON(t, build(t, outer{}, func(b *Builder) {
		b.Enum(namedString(""), "a", "b")
	}))
	props := node["properties"].(map[string]any)
	named := props["named"].(map[string]any)
	if named["type"] != "string" {
		t.Errorf("an enum override changed the type: %v", named["type"])
	}
	enum, _ := named["enum"].([]any)
	if len(enum) != 2 || enum[0] != "a" {
		t.Errorf("enum = %v, want [a b]", named["enum"])
	}
	// The override is applied to the type, wherever it appears.
	if got := mustJSON(t, build(t, struct {
		V namedString `json:"v"`
	}{}, func(b *Builder) {
		b.Enum(namedString(""), "a", "b")
	}))["properties"].(map[string]any)["v"].(map[string]any)["enum"]; got == nil {
		t.Error("an override did not reach a nested use of the type")
	}
}

func TestOutputIsStable(t *testing.T) {
	// encoding/json sorts map keys, which would reorder the document. The bytes
	// are compared in TestEmbeddedSchemasAreCurrent, so stability is not
	// optional.
	first, err := build(t, outer{}, nil).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		again, err := build(t, outer{}, nil).MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("two builds of the same type differ:\n%s\n%s", first, again)
		}
	}
}

func TestMajorOf(t *testing.T) {
	cases := map[string]int{
		"ocaw/task@1":  1,
		"ocaw/task@12": 12,
		"ocaw/task":    0,
		"ocaw/task@":   0,
		"ocaw/task@x":  0,
		"ocaw/@1":      1,
	}
	for input, want := range cases {
		if got := MajorOf(input); got != want {
			t.Errorf("MajorOf(%q) = %d, want %d", input, got, want)
		}
	}
}

func TestAssembleCarriesTheDocumentHeader(t *testing.T) {
	node, err := NewBuilder().Build(reflectTypeOf(struct {
		A string `json:"a"`
	}{}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Assemble("ocaw/x@1", "ocaw x", "a test document", node)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["$schema"] != Draft {
		t.Errorf("$schema = %v", doc["$schema"])
	}
	if doc["$id"] != "ocaw/x@1" || doc["title"] != "ocaw x" || doc["description"] != "a test document" {
		t.Errorf("header = %v", doc)
	}
	if _, ok := doc["properties"]; !ok {
		t.Error("the assembled document lost its properties")
	}
}

// ---- the validator ----

func docOf(t *testing.T, node *Node) []byte {
	t.Helper()
	raw, err := node.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestValidateAcceptsAConformingInstance(t *testing.T) {
	doc := docOf(t, build(t, struct {
		Name  string   `json:"name"`
		Count int      `json:"count"`
		Tags  []string `json:"tags"`
		Sub   *inner   `json:"sub"`
	}{}, nil))
	instance := []byte(`{"name":"x","count":3,"tags":["a"],"sub":{"a":"y","b":1}}`)
	ok, skipped, err := Validate(doc, instance)
	if !ok {
		t.Fatalf("Validate: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped %v on a document ocaw produced", skipped)
	}
}

func TestValidateRejects(t *testing.T) {
	doc := docOf(t, build(t, struct {
		Name  string   `json:"name"`
		Count int      `json:"count"`
		Tags  []string `json:"tags"`
		Sub   *inner   `json:"sub"`
	}{}, nil))
	cases := map[string]string{
		"a missing required property": `{"count":3,"tags":[],"sub":null}`,
		"a wrong type":                `{"name":1,"count":3,"tags":[],"sub":null}`,
		"a non-integer number":        `{"name":"x","count":1.5,"tags":[],"sub":null}`,
		"an array where a string is":  `{"name":["x"],"count":3,"tags":[],"sub":null}`,
		"a wrong item type":           `{"name":"x","count":3,"tags":[1],"sub":null}`,
		"a wrong nested type":         `{"name":"x","count":3,"tags":[],"sub":{"a":true,"b":1}}`,
	}
	for name, instance := range cases {
		t.Run(name, func(t *testing.T) {
			if ok, _, _ := Validate(doc, []byte(instance)); ok {
				t.Error("the validator accepted a non-conforming instance")
			}
		})
	}
}

func TestValidateEnforcesEnums(t *testing.T) {
	doc := docOf(t, build(t, struct {
		Status namedString `json:"status"`
	}{}, func(b *Builder) {
		b.Enum(namedString(""), "a", "b")
	}))
	if ok, _, err := Validate(doc, []byte(`{"status":"a"}`)); !ok {
		t.Errorf("a permitted value was rejected: %v", err)
	}
	if ok, _, _ := Validate(doc, []byte(`{"status":"c"}`)); ok {
		t.Error("a value outside the enum was accepted")
	}
}

// additionalProperties: false is the one value a *Node cannot express, so it
// has its own field — and a closed object is a different statement from one
// that says nothing at all.
func TestValidateChecksAdditionalProperties(t *testing.T) {
	openDoc := `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":true}`
	if ok, _, _ := Validate([]byte(openDoc), []byte(`{"a":"x","extra":1}`)); !ok {
		t.Error("additionalProperties:true was not honoured")
	}

	node, err := NewBuilder().Build(reflectTypeOf(struct {
		A string `json:"a"`
	}{}))
	if err != nil {
		t.Fatal(err)
	}
	node.AdditionalClosed = true
	if ok, _, _ := Validate(docOf(t, node), []byte(`{"a":"x","extra":1}`)); ok {
		t.Error("a closed object accepted a property it does not describe")
	}
	if ok, _, err := Validate(docOf(t, node), []byte(`{"a":"x"}`)); !ok {
		t.Errorf("a closed object refused a conforming instance: %v", err)
	}
}

func TestValidateReportsSkippedKeywords(t *testing.T) {
	doc := []byte(`{"type":"object","patternProperties":{"^x":{"type":"string"}},"properties":{"a":{"type":"string"}}}`)
	ok, skipped, err := Validate(doc, []byte(`{"a":"y"}`))
	if !ok {
		t.Fatalf("Validate: %v", err)
	}
	if len(skipped) == 0 {
		t.Error("a keyword the validator does not implement was not reported")
	}
}

func TestValidateRejectsNonJSON(t *testing.T) {
	if ok, _, _ := Validate([]byte("not json"), []byte(`{}`)); ok {
		t.Error("a document that is not JSON was accepted")
	}
	if ok, _, _ := Validate([]byte(`{"type":"object"}`), []byte("not json")); ok {
		t.Error("an instance that is not JSON was accepted")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(list []any, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
