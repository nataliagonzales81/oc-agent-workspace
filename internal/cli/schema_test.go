package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/schema"
)

const schemaCommandName = "schema"

// envelopeOf runs a command in a workspace and returns its envelope, bytes as
// they went to stdout.
func envelopeOf(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	full := append([]string{"--dir", dir}, args...)
	if code := cli.Main(full, &out, &bytes.Buffer{}, false); code != 0 {
		t.Fatalf("ocaw %s: exit %d", strings.Join(args, " "), code)
	}
	return bytes.TrimSpace(out.Bytes())
}

// dataOf returns the `data` member of an envelope. The schema documents that
// object, not the envelope around it.
func dataOf(t *testing.T, envBytes []byte) []byte {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(envBytes, &env); err != nil {
		t.Fatalf("not an envelope: %v\n%s", err, envBytes)
	}
	return env.Data
}

type schemaPayload struct {
	Key       string          `json:"key"`
	SchemaID  string          `json:"schema_id"`
	Major     int             `json:"major"`
	Title     string          `json:"title"`
	Keys      []string        `json:"keys"`
	Available []string        `json:"available"`
	MaxMajor  int             `json:"schema_max"`
	Count     int             `json:"count"`
	Bytes     int             `json:"bytes"`
	Document  json.RawMessage `json:"document"`
}

// schemaRun asks for the envelope. The default stdout for `ocaw schema <key>`
// is the document itself, so a caller that wants the metadata asks for --json.
func schemaRun(t *testing.T, args ...string) (int, envelope.Envelope, schemaPayload) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main(append([]string{schemaCommandName, "--json"}, args...), &out, &errOut, false)
	raw := bytes.TrimSpace(out.Bytes())
	var env envelope.Envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("schema %v: stdout is not a JSON envelope: %v\n%s", args, err, raw)
		}
	}
	return code, env, decodeSchema(t, env)
}

func decodeSchema(t *testing.T, env envelope.Envelope) schemaPayload {
	t.Helper()
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var p schemaPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("schema payload does not match the documented shape: %v\n%s", err, raw)
	}
	return p
}

// AC1 — `ocaw schema task.list` emits valid JSON Schema that validates a real
// envelope. Both halves matter: a schema nothing validates against is a
// document, not a contract.
func TestAC1SchemaValidatesARealEnvelope(t *testing.T) {
	dir := initialisedTaskRepo(t)
	chain(t, dir)
	mustTask(t, dir, "set", "1", "--status", "done")

	// A real envelope, verbatim, exactly as a caller receives it. The document
	// describes the envelope's `data` object, so the instance under test is that
	// object — and the envelope's own `schema` field has to match the document's
	// `$id`, which is the part that ties the two together.
	envelopeBytes := envelopeOf(t, dir, taskCommandName, "list")
	instance := dataOf(t, envelopeBytes)
	var decoded envelope.Envelope
	if err := json.Unmarshal(envelopeBytes, &decoded); err != nil {
		t.Fatal(err)
	}

	code, _, payload := schemaRun(t, "task.list")
	if code != 0 {
		t.Fatalf("ocaw schema task.list: exit %d", code)
	}
	if len(payload.Document) == 0 {
		t.Fatal("no document in the payload")
	}

	if string(decoded.Schema) != payload.SchemaID {
		t.Errorf("the envelope says schema %q, the document says %q", decoded.Schema, payload.SchemaID)
	}
	ok, skipped, err := schema.Validate(payload.Document, instance)
	if !ok {
		t.Fatalf("the schema rejects a payload the tool produced: %v", err)
	}
	if len(skipped) > 0 {
		t.Errorf("the validator skipped keywords the document uses: %v", skipped)
	}

	// And it is real JSON Schema, not just something that happens to load.
	var doc map[string]any
	if err := json.Unmarshal(payload.Document, &doc); err != nil {
		t.Fatalf("the document is not JSON: %v", err)
	}
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %v, want the 2020-12 dialect", doc["$schema"])
	}
	if doc["$id"] != "ocaw/task@1" {
		t.Errorf("$id = %v, want ocaw/task@1, the value the envelope carries", doc["$id"])
	}
	if _, ok := doc["properties"]; !ok {
		t.Error("the document has no properties")
	}

	// A schema that accepts everything is not a schema. This must fail.
	tampered := bytes.Replace(instance, []byte(`"pending"`), []byte(`"invented"`), 1)
	if len(tampered) == len(instance) {
		t.Skip("could not tamper with the payload")
	}
	if ok, _, _ := schema.Validate(payload.Document, tampered); ok {
		t.Error("the schema accepts a status outside the closed vocabulary")
	}
}

// The command must work with the workspace directory deleted — a schema query
// has to work on a broken workspace, which is exactly when it is most needed.
func TestSchemaNeedsNoWorkspace(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone", "deeper")
	code, env, payload := cliSchemaAt(t, missing, "task.list")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; error = %+v", code, env.Err)
	}
	if len(payload.Document) == 0 {
		t.Error("no document emitted without a workspace")
	}

	// A cwd that does not exist at all is a different thing and must not panic.
	// And from a directory that exists but is not a project at all, with no
	// .agent, no git, and no state.
	bare := t.TempDir()
	if code := cli.Main([]string{"--dir", bare, schemaCommandName, "status"}, &bytes.Buffer{}, &bytes.Buffer{}, false); code != 0 {
		t.Errorf("exit = %d in a bare directory", code)
	}
	// And from a deleted working directory, which is the case that matters: a
	// schema query has to work when the workspace is the thing that is broken.
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	doomed := filepath.Join(bare, "doomed")
	if err := os.Mkdir(doomed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(doomed); err != nil {
		t.Skipf("cannot chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if err := os.Remove(doomed); err != nil {
		t.Skipf("cannot remove the working directory: %v", err)
	}
	var out bytes.Buffer
	if code := cli.Main([]string{schemaCommandName, "status"}, &out, &bytes.Buffer{}, false); code != 0 {
		t.Errorf("exit = %d with a deleted cwd", code)
	}
	if out.Len() == 0 {
		t.Error("no schema emitted from a deleted working directory")
	}
}

func cliSchemaAt(t *testing.T, dir, key string) (int, envelope.Envelope, schemaPayload) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main([]string{"--dir", dir, schemaCommandName, "--json", key}, &out, &errOut, false)
	raw := bytes.TrimSpace(out.Bytes())
	var env envelope.Envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, raw)
		}
	}
	return code, env, decodeSchema(t, env)
}

// Every command in §7 has a schema — the test that fails when one does not.
func TestEveryCommandHasASchema(t *testing.T) {
	commands := []string{
		"init", "doctor", "status", "task", "verify", "workflow", "report", "schema", "version",
	}
	_, _, index := schemaRun(t)
	available := map[string]bool{}
	for _, key := range index.Available {
		available[key] = true
	}
	for _, name := range commands {
		if !available[name] {
			t.Errorf("§7 lists %q but ocaw schema has no document for it", name)
		}
	}
	// And every subcommand, because the AC names `task.list` and a caller
	// should not have to know that `task` also answers to it.
	for _, key := range []string{
		"task.add", "task.set", "task.dep", "task.rm", "task.show", "task.list", "task.next",
		"verify.detect", "verify.run", "verify.history",
		"workflow.set", "workflow.accept", "workflow.show",
	} {
		if !available[key] {
			t.Errorf("no schema for %q", key)
		}
	}
	// Nothing extra: a document for a command that does not exist is a
	// maintenance trap.
	if len(index.Available) != len(commands)+13 {
		t.Errorf("available = %d keys, want %d", len(index.Available), len(commands)+13)
	}
}

// schema_max equals the highest @major present — computed, not asserted as a
// constant, so a document at major 2 cannot ship while version still says 1.
func TestSchemaMaxEqualsTheHighestMajorPresent(t *testing.T) {
	_, _, index := schemaRun(t)
	if index.MaxMajor == 0 {
		t.Fatal("schema_max is 0, but documents are present")
	}
	highest := 0
	for _, key := range index.Available {
		_, payload := schemaRun2(t, key)
		major := schema.MajorOf(payload.SchemaID)
		if major == 0 {
			t.Errorf("%s has schema id %q, which carries no @major", key, payload.SchemaID)
		}
		if major > highest {
			highest = major
		}
	}
	if index.MaxMajor != highest {
		t.Errorf("schema_max = %d, highest @major in the documents = %d", index.MaxMajor, highest)
	}
	// And version agrees with the schema command.
	code, versionEnv := versionRun(t)
	if code != 0 {
		t.Fatalf("ocaw version: exit %d (%+v)", code, versionEnv.Err)
	}
	raw, err := json.Marshal(versionEnv.Data)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		SchemaMax int `json:"schema_max"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data.SchemaMax != highest {
		t.Errorf("version reports schema_max %d, the documents say %d", data.SchemaMax, highest)
	}
}

func schemaRun2(t *testing.T, key string) (int, schemaPayload) {
	t.Helper()
	code, _, payload := schemaRun(t, key)
	return code, payload
}

func versionRun(t *testing.T) (int, envelope.Envelope) {
	t.Helper()
	var out bytes.Buffer
	if code := cli.Main([]string{"version"}, &out, &bytes.Buffer{}, false); code != 0 {
		t.Fatalf("ocaw version: exit %d", code)
	}
	var env envelope.Envelope
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &env); err != nil {
		t.Fatal(err)
	}
	return 0, env
}

// Every embedded document is byte-for-byte what the generator produces. This is
// what makes embedding safe instead of a snapshot that quietly rots.
// A bare `ocaw schema` lists everything, in human mode as well as JSON.
func TestSchemaIndex(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{schemaCommandName}, &out, &errOut, true); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	text := out.String()
	if strings.Contains(text, `"available"`) {
		t.Errorf("a TTY got JSON:\n%s", text)
	}
	for _, key := range []string{"task.list", "verify.run", "workflow.show", "version"} {
		if !strings.Contains(text, key) {
			t.Errorf("the index does not mention %q:\n%s", key, text)
		}
	}
}

// Asking for a document with --output writes the file and leaves stdout empty
// (§4.1), so an agent can save a validator spec without capturing the envelope.
// The document is the payload, so a redirect produces a loadable file. This is
// what makes the command useful from a shell, which is the whole point of it.
func TestSchemaToStdoutIsTheDocument(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{schemaCommandName, "status"}, &out, &errOut, false); code != 0 {
		t.Fatalf("exit = %d: %s", code, errOut.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &doc); err != nil {
		t.Fatalf("stdout is not a schema document: %v\n%s", err, out.String())
	}
	if doc["$id"] != "ocaw/status@1" {
		t.Errorf("$id = %v, want ocaw/status@1", doc["$id"])
	}
	// --json gives the envelope instead, for a caller that wants the metadata.
	var envOut bytes.Buffer
	if code := cli.Main([]string{schemaCommandName, "status", "--json"}, &envOut, &bytes.Buffer{}, false); code != 0 {
		t.Fatal(code)
	}
	var env envelope.Envelope
	if err := json.Unmarshal(bytes.TrimSpace(envOut.Bytes()), &env); err != nil {
		t.Fatalf("--json did not emit an envelope: %v", err)
	}
	payload := decodeSchema(t, env)
	if payload.SchemaID != "ocaw/status@1" {
		t.Errorf("the envelope carries schema_id %q", payload.SchemaID)
	}
	// --quiet writes nothing at all.
	var quiet bytes.Buffer
	if code := cli.Main([]string{schemaCommandName, "status", "--quiet"}, &quiet, &bytes.Buffer{}, false); code != 0 {
		t.Fatal(code)
	}
	if quiet.Len() != 0 {
		t.Errorf("--quiet wrote to stdout:\n%s", quiet.String())
	}
}

func TestSchemaOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task.json")
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{schemaCommandName, "task.list", "--output", path}, &out, &errOut, false); code != 0 {
		t.Fatalf("exit = %d: %s", code, errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("--output wrote to stdout as well:\n%s", out.String())
	}
	// The file holds the document, not an envelope with the document nested
	// inside it: the reason to ask for a schema is to load it.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("nothing was written: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the written file is not JSON: %v", err)
	}
	if doc["$id"] != "ocaw/task@1" {
		t.Errorf("$id = %v", doc["$id"])
	}
}

func TestSchemaErrors(t *testing.T) {
	for _, args := range [][]string{
		{"bogus"},
		{"task", "list"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, env, _ := schemaRun(t, args...)
			if code != 2 {
				t.Errorf("exit = %d, want 2 (usage); error = %+v", code, env.Err)
			}
			if env.Err == nil || env.Err.Code != envelope.CodeUsage {
				t.Errorf("code = %v, want usage", env.Err.Code)
			}
		})
	}
	// The error message has to be usable: it names what is available.
	_, env, _ := schemaRun(t, "bogus")
	if !strings.Contains(env.Err.Message, "task.list") {
		t.Errorf("message = %q, want it to list what does exist", env.Err.Message)
	}
}

// Aliases share one document byte for byte, so a caller who learned
// `ocaw schema task` and a caller who learned `ocaw schema task.next` end up
// with the same bytes.
func TestAliasesShareOneDocument(t *testing.T) {
	_, _, bare := schemaRun(t, "task")
	_, _, sub := schemaRun(t, "task.list")
	if string(bare.Document) != string(sub.Document) {
		t.Error("two keys for the same payload produced different documents")
	}
	if bare.SchemaID != sub.SchemaID {
		t.Errorf("schema ids differ: %q vs %q", bare.SchemaID, sub.SchemaID)
	}
	// A validator built from either validates the same envelope.
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	var raw bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, taskCommandName, "next"}, &raw, &bytes.Buffer{}, false); code != 0 {
		t.Fatal(code)
	}
	instance := dataOf(t, bytes.TrimSpace(raw.Bytes()))
	for name, doc := range map[string]json.RawMessage{"task": bare.Document, "task.next": sub.Document} {
		if ok, _, err := schema.Validate(doc, instance); !ok {
			t.Errorf("%s: the document rejects a real payload: %v", name, err)
		}
	}
}

// The binary carries the documents: `ocaw schema` has to work with no source
// tree, no files, and no network.
func TestSchemasShipInTheBinary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ocaw")
	build := exec.Command("go", "build", "-o", binary, "github.com/nataliagonzales81/oc-agent-workspace/cmd/ocaw")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build the binary here: %v\n%s", err, out)
	}
	// The default output is the document, and a redirect must produce a
	// loadable schema file — which is the shape a caller wants to commit.
	out, err := exec.Command(binary, schemaCommandName, "status").Output()
	if err != nil {
		t.Fatalf("the built binary cannot answer a schema query: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out), &doc); err != nil {
		t.Fatalf("the built binary did not emit a schema document: %v\n%s", err, out)
	}
	if doc["$id"] != "ocaw/status@1" {
		t.Errorf("$id = %v, want ocaw/status@1", doc["$id"])
	}
	if _, ok := doc["properties"]; !ok {
		t.Error("the emitted document has no properties")
	}
	// And the embedded copy is byte-identical to the committed one, so a build
	// from a clean checkout is the build that was tested.
	committed, err := os.ReadFile(filepath.Join("schemas", "ocaw-status@1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(committed) != string(bytes.TrimSpace(out)) {
		t.Error("the binary's embedded document differs from the committed one")
	}
}
