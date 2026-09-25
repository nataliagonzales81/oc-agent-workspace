package cli

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/doctor"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/schema"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/verify"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

// schemaDocs holds the generated documents. They are committed rather than
// built at run time so `go:embed` can carry them, and
// TestEmbeddedSchemasAreCurrent regenerates every one and compares the bytes —
// which is what makes embedding safe instead of a snapshot that quietly rots.
//
//go:embed schemas/*.json
var schemaDocs embed.FS

const schemaCommandName = "schema"

// schemaEntry is one shipped schema. A payload shared by several subcommands is
// one entry with several keys, because the document describes the envelope's
// `data`, not the subcommand that produced it.
type schemaEntry struct {
	keys        []string
	sample      any
	title       string
	description string
}

// schemaEntries lists every command in SPEC §7, in the order a human reads
// them. The test that walks this list is what fails when a command is added
// without a schema.
func schemaEntries() []schemaEntry {
	return []schemaEntry{
		{
			keys:        []string{versionCommandName},
			sample:      versionData{},
			title:       "ocaw version",
			description: "Build information and the highest schema major ocaw ships.",
		},
		{
			keys:        []string{initCommandName},
			sample:      initData{},
			title:       "ocaw init",
			description: "What `ocaw init` created, what it left alone, and what it detected.",
		},
		{
			keys:        []string{doctorCommandName},
			sample:      doctorData{},
			title:       "ocaw doctor",
			description: "Every finding from one health pass, plus any fixes applied.",
		},
		{
			keys:        []string{statusCommandName},
			sample:      statusData{},
			title:       "ocaw status",
			description: "The orientation summary: counts, the ready queue, what is blocked, and the last verification result.",
		},
		{
			keys:        []string{taskCommandName, "task.add", "task.dep", "task.list", "task.next", "task.rm", "task.set", "task.show"},
			sample:      taskData{},
			title:       "ocaw task",
			description: "One payload shape for all seven subcommands, so a caller does not have to know which one it ran. `detail` carries the structured half of a refusal: the blocking dep ids, the cycle path, the dependent that would be stranded.",
		},
		{
			keys:        []string{verifyCommandName, "verify.detect", "verify.history", "verify.run"},
			sample:      verifyData{},
			title:       "ocaw verify",
			description: "One payload shape for detect, run, and history. `attempts` is empty when every gate was skipped, which is not the same as a run that passed nothing.",
		},
		{
			keys:        []string{workflowCommandName, "workflow.accept", "workflow.set", "workflow.show"},
			sample:      workflowData{},
			title:       "ocaw workflow",
			description: "The authored half: request, scope, constraints, and acceptance criteria, plus the drift state of the file they are rendered into.",
		},
		{
			keys:        []string{reportCommandName},
			sample:      reportData{},
			title:       "ocaw report",
			description: "The rendered document, its drift state before and after, and whether it was written.",
		},
		{
			keys:        []string{schemaCommandName},
			sample:      schemaData{},
			title:       "ocaw schema",
			description: "One shipped schema, or the index of all of them.",
		},
	}
}

// schemaBuilder registers the closed vocabularies. Each is a real constraint the
// runtime enforces, so a validator can enforce it too.
//
// `error.code` is deliberately absent. The runtime maps an undeclared code to
// `internal` rather than failing, so constraining it in the schema would make
// the schema stricter than the tool: a document that rejects what ocaw accepts
// is wrong in the direction that breaks users.
func schemaBuilder() *schema.Builder {
	b := schema.NewBuilder()
	b.Enum(state.StatusPending, "pending", "in_progress", "done", "blocked", "cancelled")
	b.Enum(state.GatePending, "pending", "pass", "fail", "timeout")
	b.Enum(doctor.SeverityError, "error", "warning", "info")
	b.Enum(report.StatusCurrent, "current", "stale", "edited", "foreign", "missing")
	b.Enum(verify.Pass, "pass", "fail", "timeout")
	return b
}

type schemaData struct {
	// Key is the name this document was asked for. Several keys can share one
	// document, because they describe the same envelope's data.
	Key       string          `json:"key"`
	SchemaID  string          `json:"schema_id"`
	Major     int             `json:"major"`
	Title     string          `json:"title"`
	Keys      []string        `json:"keys"`
	Available []string        `json:"available"`
	MaxMajor  int             `json:"schema_max"`
	Count     int             `json:"count"`
	Bytes     int             `json:"bytes"`
	Document  json.RawMessage `json:"document,omitempty"`
}

var schemaCommand = &command{
	name:      schemaCommandName,
	summary:   "print the JSON Schema of a command's data object",
	usage:     "ocaw schema <" + strings.Join(schemaKeys(), "|") + ">",
	takesArgs: true,
	run:       runSchema,
	human:     humanSchema,
	setup:     func(*flag.FlagSet, *Context) {},
}

func schemaKeys() []string {
	var out []string
	for _, entry := range schemaEntries() {
		out = append(out, entry.keys...)
	}
	sort.Strings(out)
	return out
}

// envelopeSchemaID is the schema field an envelope with this key carries. A key
// with a dot is a subcommand of the one before it, and both share the payload.
func envelopeSchemaID(key string) string {
	command := key
	if head, _, found := strings.Cut(key, "."); found {
		command = head
	}
	return string(envelope.SchemaID(command, envelope.SchemaMajor))
}

func schemaFileName(schemaID string) string {
	return strings.ReplaceAll(schemaID, "/", "-") + ".json"
}

// generateSchema builds the document for a key. This is the single definition of
// what ocaw's payloads look like; the embedded files are checked against it.
func generateSchema(key string) ([]byte, error) {
	entry, found := schemaEntryFor(key)
	if !found {
		return nil, fmt.Errorf("no schema for %q", key)
	}
	node, err := schemaBuilder().Build(reflect.TypeOf(entry.sample))
	if err != nil {
		return nil, err
	}
	return schema.Assemble(envelopeSchemaID(key), entry.title, entry.description, node)
}

func schemaEntryFor(key string) (schemaEntry, bool) {
	for _, entry := range schemaEntries() {
		for _, k := range entry.keys {
			if k == key {
				return entry, true
			}
		}
	}
	return schemaEntry{}, false
}

func embeddedSchema(key string) ([]byte, bool) {
	raw, err := schemaDocs.ReadFile("schemas/" + schemaFileName(envelopeSchemaID(key)))
	if err != nil {
		return nil, false
	}
	return raw, true
}

// schemaMaxMajor is the highest @major across every embedded document, which is
// what `ocaw version` reports as schema_max.
func schemaMaxMajor() int {
	maxMajor := 0
	for _, key := range schemaKeys() {
		if major := schema.MajorOf(envelopeSchemaID(key)); major > maxMajor {
			maxMajor = major
		}
	}
	return maxMajor
}

func runSchema(c *Context, args []string) envelope.Result {
	res := envelope.Result{Command: schemaCommandName}
	available := schemaKeys()

	// No workspace is resolved, and none is required: a schema query has to work
	// on a broken workspace, which is exactly when it is most needed.
	// Flags may sit after the command name, so re-read them over the remainder.
	fs := newFlagSet("ocaw " + schemaCommandName)
	registerGlobals(fs, &c.Options)
	rest, parseErr := parseInterspersed(fs, args)
	if parseErr != nil {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw schema --help",
			"%s", parseErr.Error(),
		)
		return res
	}
	args = rest

	if len(args) == 0 {
		res.Data = schemaData{
			Available: available,
			MaxMajor:  schemaMaxMajor(),
			Count:     len(available),
			Keys:      []string{},
		}
		return res
	}
	if len(args) > 1 {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw schema --help",
			"schema takes one command name, got %d", len(args),
		)
		return res
	}

	key := args[0]
	entry, found := schemaEntryFor(key)
	if !found {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw schema",
			"no schema for %q; known commands are %s", key, strings.Join(available, "|"),
		)
		res.Data = schemaData{Available: available, MaxMajor: schemaMaxMajor(), Count: len(available), Keys: []string{}}
		return res
	}

	raw, ok := embeddedSchema(key)
	if !ok {
		// The generator is authoritative, so this branch is a packaging bug
		// rather than a user error. Say which file is missing, because that is
		// the only useful thing the person reading it can act on.
		res.Err = envelope.Errorf(
			envelope.CodeInternal,
			"rebuild with `go test ./internal/cli/ -run TestEmbeddedSchemasAreCurrent -update`",
			"the embedded document for %q (%s) is missing", key, schemaFileName(envelopeSchemaID(key)),
		)
		return res
	}

	// Parse before printing: emitting bytes that are not JSON would make this
	// command's own output unusable for the one thing it exists for.
	var doc json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		res.Err = envelope.Errorf(
			envelope.CodeInternal,
			"rebuild the embedded documents",
			"the embedded schema for %q is not valid JSON: %v", key, err,
		)
		return res
	}

	// The document itself is the payload on stdout, not the envelope. This is
	// the one command whose output is a document rather than a description of
	// one, and `ocaw schema task.list > task.json` producing a loadable schema
	// is the whole reason it exists. `--json` still gives the envelope, for a
	// caller that wants the metadata alongside.
	if !c.Options.JSON && !c.Options.Quiet {
		if c.Options.Output != "" {
			if werr := writePrimary(c.Options.Output, raw); werr != nil {
				res.Err = writeFailure("write "+c.Options.Output, werr)
				return res
			}
		} else if _, werr := c.Stdout.Write(raw); werr != nil {
			res.Err = writeFailure("write the schema to stdout", werr)
			return res
		}
		c.wrotePayload()
	}

	res.Data = schemaData{
		Key:       key,
		SchemaID:  envelopeSchemaID(key),
		Major:     envelope.SchemaMajor,
		Title:     entry.title,
		Keys:      entry.keys,
		Available: available,
		MaxMajor:  schemaMaxMajor(),
		Bytes:     len(raw),
		Document:  json.RawMessage(raw),
	}
	return res
}

// writePrimary writes bytes to a path, creating the file atomically. It is the
// same routine the rest of ocaw uses for a file that must never be seen
// half-written, because a truncated schema is a schema that validates nothing.
func writePrimary(path string, raw []byte) error {
	return workspace.WriteFileAtomic(path, raw)
}

func humanSchema(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(schemaData)
	if !ok {
		return fmt.Errorf("schema: unexpected data type %T", env.Data)
	}
	if len(data.Document) > 0 {
		_, err := w.Write(data.Document)
		return err
	}
	// The index form, which is what a bare `ocaw schema` produces.
	if _, err := fmt.Fprintf(w, "%d schemas, highest major %d\n\n", data.Count, data.MaxMajor); err != nil {
		return err
	}
	for _, key := range data.Available {
		if _, err := fmt.Fprintf(w, "  %-18s %s\n", key, envelopeSchemaID(key)); err != nil {
			return err
		}
	}
	return nil
}
