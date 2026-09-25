package yaml

import (
	"path/filepath"
	"strconv"
	"strings"
)

// KnowledgeSchema is the schema id written into every knowledge/index.yaml.
const KnowledgeSchema = "ocaw/knowledge@1"

// Knowledge entry kinds.
const (
	KindFact     = "fact"
	KindDecision = "decision"
	KindGotcha   = "gotcha"
)

// KnowledgeKinds is the closed set of values entry.kind may take.
var KnowledgeKinds = []string{KindFact, KindDecision, KindGotcha}

// KnowledgeIndex is the .agent/knowledge/index.yaml entry list (SPEC §5.2).
type KnowledgeIndex struct {
	Schema  string
	Entries []KnowledgeEntry
}

// KnowledgeEntry is one durable project fact. Anchor is repo-relative and points
// outside .agent/, so a fact survives the workspace being regenerated and can be
// found by someone who never read the entry.
type KnowledgeEntry struct {
	ID         string
	Title      string
	Kind       string
	Anchor     string
	Created    string
	Supersedes []string
}

// ParseKnowledgeIndex reads a knowledge/index.yaml document.
func ParseKnowledgeIndex(src string) (KnowledgeIndex, error) {
	v, err := Parse(src, 1)
	if err != nil {
		return KnowledgeIndex{}, err
	}
	m, err := mustMap(v, "knowledge/index.yaml")
	if err != nil {
		return KnowledgeIndex{}, err
	}
	if err := checkKeys(m, "knowledge/index.yaml", KeySchema, "entries"); err != nil {
		return KnowledgeIndex{}, err
	}

	var idx KnowledgeIndex
	schema, err := optStr(m, KeySchema, "knowledge/index.yaml")
	if err != nil {
		return KnowledgeIndex{}, err
	}
	if schema != "" && schema != KnowledgeSchema {
		return KnowledgeIndex{}, errf(m.Line, "knowledge/index.yaml: unsupported schema %q (this build writes %s)",
			schema, KnowledgeSchema)
	}
	idx.Schema = KnowledgeSchema

	ev, ok := m.Get("entries")
	if !ok {
		return KnowledgeIndex{}, errf(m.Line, "knowledge/index.yaml: missing required field %q", "entries")
	}
	// A bare `entries:` key parses as null; in this shape null and an empty list
	// mean the same thing, so a hand-trimmed index still reads.
	seq := Seq{}
	if ev != nil {
		var isSeq bool
		if seq, isSeq = ev.(Seq); !isSeq {
			return KnowledgeIndex{}, errf(m.Line, "knowledge/index.yaml: field %q must be a list, got %s", "entries", kindName(ev))
		}
	}
	idx.Entries = make([]KnowledgeEntry, 0, len(seq))
	seen := map[string]int{}
	for i, item := range seq {
		where := "knowledge/index.yaml entries[" + strconv.Itoa(i) + "]"
		em, err := mustMap(item, where)
		if err != nil {
			return KnowledgeIndex{}, err
		}
		if err := checkKeys(em, where, "id", "title", "kind", "anchor", "created", "supersedes"); err != nil {
			return KnowledgeIndex{}, err
		}
		var e KnowledgeEntry
		if e.ID, err = str(em, "id", where); err != nil {
			return KnowledgeIndex{}, err
		}
		if e.Title, err = str(em, "title", where); err != nil {
			return KnowledgeIndex{}, err
		}
		if e.Kind, err = enum(em, "kind", where, KnowledgeKinds...); err != nil {
			return KnowledgeIndex{}, err
		}
		if e.Anchor, err = str(em, "anchor", where); err != nil {
			return KnowledgeIndex{}, err
		}
		if err := validAnchor(em, where, e.Anchor); err != nil {
			return KnowledgeIndex{}, err
		}
		if e.Created, err = optStr(em, "created", where); err != nil {
			return KnowledgeIndex{}, err
		}
		if _, ok := em.Get("supersedes"); ok {
			if e.Supersedes, err = stringList(em, "supersedes", where); err != nil {
				return KnowledgeIndex{}, err
			}
		} else {
			e.Supersedes = []string{}
		}
		if prev, dup := seen[e.ID]; dup {
			return KnowledgeIndex{}, errf(em.Line, "%s: duplicate entry id %q (already used by entry %d)", where, e.ID, prev)
		}
		seen[e.ID] = i
		idx.Entries = append(idx.Entries, e)
	}
	return idx, nil
}

// validAnchor enforces the two §5.2 anchor rules. They are checked here as well as
// in doctor because this package serialises the field: a writer that could emit an
// anchor doctor rejects would be a defect reachable without any malformed input.
func validAnchor(m *Map, where, anchor string) error {
	if strings.TrimSpace(anchor) == "" {
		return errf(m.Line, "%s: %q must not be empty", where, "anchor")
	}
	if filepath.IsAbs(anchor) || strings.HasPrefix(anchor, "/") || strings.HasPrefix(anchor, "~") {
		return errf(m.Line, "%s: anchor %q must be repo-relative, not absolute", where, anchor)
	}
	clean := filepath.ToSlash(filepath.Clean(anchor))
	if clean == ".agent" || strings.HasPrefix(clean, ".agent/") {
		return errf(m.Line, "%s: anchor %q points inside .agent/, where the workspace is regenerated", where, anchor)
	}
	return nil
}

// Encode renders the index in the §5.2 field order, with `supersedes: []` for an
// empty list so the output matches the spec fixture byte for byte.
func (k KnowledgeIndex) Encode() string {
	var b strings.Builder
	b.WriteString(kv(0, KeySchema, KnowledgeSchema))
	if len(k.Entries) == 0 {
		b.WriteString("entries: []\n")
		return b.String()
	}
	b.WriteString("entries:\n")
	for _, e := range k.Entries {
		b.WriteString(pad(2) + "- " + kvBody("id", e.ID))
		b.WriteString(kv(4, "title", e.Title))
		b.WriteString(kv(4, "kind", e.Kind))
		b.WriteString(kv(4, "anchor", e.Anchor))
		b.WriteString(kv(4, "created", e.Created))
		b.WriteString(kvSeq(4, "supersedes", e.Supersedes))
	}
	return b.String()
}
