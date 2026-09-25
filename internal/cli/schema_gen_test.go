package cli

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateSchemas regenerates the committed documents. It is a test rather than a
// separate program because the payload types are unexported, so this is the only
// place in the module that can see them.
var updateSchemas = flag.Bool("update", false, "rewrite internal/cli/schemas from the payload types")

func TestEmbeddedSchemasAreCurrent(t *testing.T) {
	dir := filepath.Join("schemas")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, key := range schemaKeys() {
		raw, err := generateSchema(key)
		if err != nil {
			t.Fatalf("generate %s: %v", key, err)
		}
		// A document is generated once per entry, not once per alias: two keys
		// with the same envelope schema must produce identical bytes.
		name := schemaFileName(envelopeSchemaID(key))
		if seen[name] {
			continue
		}
		seen[name] = true
		if *updateSchemas {
			if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			continue
		}
		committed, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s is missing: the schemas drifted from the payload types. "+
				"Regenerate with: go test ./internal/cli/ -run TestEmbeddedSchemasAreCurrent -update", name)
		}
		if string(committed) != string(raw) {
			t.Errorf("%s differs from the payload types. Regenerate with:\n"+
				"  go test ./internal/cli/ -run TestEmbeddedSchemasAreCurrent -update", name)
		}
	}
	// Anything left in the directory is a document for a command that no longer
	// exists, which would make `schema_max` wrong.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !seen[entry.Name()] {
			t.Errorf("%s is embedded but no command maps to it", entry.Name())
		}
	}
}
