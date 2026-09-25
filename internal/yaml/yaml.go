// Package yaml implements the strict YAML subset that the ocaw workspace manifest,
// knowledge index, and SKILL.md frontmatter are defined in terms of (SPEC §5).
//
// The subset is deliberately small: block mappings, block sequences, and scalars,
// plus the empty flow sequence `[]` because the spec's own §5.2 fixture uses it.
// Every other YAML construct is a typed *Error rather than a silent drop. A parser
// that quietly discards a field is worse than one that refuses to parse, because
// ocaw writes these files back: a lost field becomes a silently deleted key in a
// config on the next `ocaw init --force`.
//
// Scalars are held as their text exactly as written and are never converted to
// bool, int, or time. The shapes in §5 mix `ocaw: true` with `version: 1.0.0` and
// `created: 2026-09-25T00:00:00Z`, so converting at parse time would make "1.0"
// and "1" indistinguishable on the way out. Typed accessors convert, and it is
// their job to reject a string that is not the type the field declares.
package yaml

import (
	"fmt"
	"strconv"
	"strings"
)

// Error is a parse or decode failure, carrying the 1-based line that caused it so
// a human can find it in a file they hand-edited.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	}
	return e.Msg
}

func errf(line int, format string, args ...any) *Error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, args...)}
}

// Map is a mapping that remembers key order and the line each key appeared on.
// Order is preserved so that a file written back out is byte-identical to what was
// read (SPEC §9.5); per-key lines are kept so a diagnostic can point at the
// offending key instead of at the first line of the block.
type Map struct {
	keys []keyEntry
	vals map[string]any
	Line int
}

type keyEntry struct {
	key  string
	line int
}

// Seq is a sequence. A parsed block sequence is a Seq of any; the §5 shapes only
// ever hold scalars or maps inside one.
type Seq []any

// NewMap returns an empty ordered map.
func NewMap() *Map { return &Map{vals: map[string]any{}} }

// Get returns the value stored under key.
func (m *Map) Get(key string) (any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m.vals[key]
	return v, ok
}

// Set stores value under key, appending the key to the order only the first time.
func (m *Map) Set(key string, value any) { m.SetAt(key, value, 0) }

// SetAt is Set with the line the key appeared on, which the parser supplies and
// hand-built maps leave at zero.
func (m *Map) SetAt(key string, value any, line int) {
	if m.vals == nil {
		m.vals = map[string]any{}
	}
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, keyEntry{key: key, line: line})
	}
	m.vals[key] = value
}

// Order returns the mapping's keys in document order.
func (m *Map) Order() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, k.key)
	}
	return out
}

// LineOf returns the line key appeared on, falling back to the block's own line
// for a map that was built by hand rather than parsed.
func (m *Map) LineOf(key string) int {
	if m == nil {
		return 0
	}
	for _, k := range m.keys {
		if k.key == key {
			if k.line > 0 {
				return k.line
			}
			break
		}
	}
	return m.Line
}

// Parse reads a single YAML document into an ordered *Map, a Seq, or a string
// holding the scalar text. firstLine is added to every reported line number, so a
// caller that extracted a sub-block can still point at real file lines.
func Parse(src string, firstLine int) (any, error) {
	lines, err := scan(src, firstLine)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	p := &parser{lines: lines}
	v, err := p.parseBlock(blockIndent(lines[0]))
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.lines) {
		return nil, errf(p.lines[p.pos].num, "unexpected content after the top-level block")
	}
	return v, nil
}

// --- scanning ---

// pline is one significant source line.
//
// indent and dashCol are tracked separately because a sequence item has two
// columns: the dash sits at the sequence's indent, and the item's content starts
// one to three characters further in. Collapsing them into one number makes a
// block sequence of maps recurse into itself forever, so they stay distinct.
type pline struct {
	indent  int // column at which the content begins
	dashCol int // column of the "-", or -1 for a non-sequence line
	text    string
	num     int
}

// blockIndent is the column a nested block starts at: the dash column for a
// sequence, the key column otherwise.
func blockIndent(l pline) int {
	if l.dashCol >= 0 {
		return l.dashCol
	}
	return l.indent
}

// scan splits source into indentation-annotated lines, dropping blank and
// comment-only lines and rejecting the constructs the subset does not support.
func scan(src string, firstLine int) ([]pline, error) {
	var out []pline
	for i, raw := range strings.Split(src, "\n") {
		num := firstLine + i
		body := strings.TrimPrefix(raw, "\r")
		if strings.TrimSpace(body) == "" {
			continue
		}
		indent := len(body) - len(strings.TrimLeft(body, " \t"))
		if strings.ContainsRune(body[:indent], '\t') {
			return nil, errf(num, "tab in indentation is not supported")
		}
		text := stripComment(body[indent:])
		if err := checkConstructs(text, num); err != nil {
			return nil, err
		}
		if text == "" {
			continue
		}
		// "- key: value" is shorthand for a mapping indented past the dash.
		// Recording the dash column and keeping the content column separately
		// lets one code path parse the item's first line and its continuation
		// lines identically.
		if rest, ok := dashRest(text); ok {
			if strings.HasPrefix(rest, "\t") {
				return nil, errf(num, "tab in indentation is not supported")
			}
			lead := len(rest) - len(strings.TrimLeft(rest, " "))
			out = append(out, pline{
				indent:  indent + 1 + lead,
				dashCol: indent,
				text:    strings.TrimLeft(rest, " "),
				num:     num,
			})
			continue
		}
		out = append(out, pline{indent: indent, dashCol: -1, text: text, num: num})
	}
	return out, nil
}

// dashRest splits a leading sequence marker off a line. Either whitespace
// character YAML allows after the dash is accepted as the separator, so the
// tab case can be rejected with a useful message instead of being read as part
// of the item's content.
func dashRest(text string) (rest string, ok bool) {
	if text == "-" {
		return "", true
	}
	if len(text) > 1 && text[0] == '-' && (text[1] == ' ' || text[1] == '\t') {
		return text[1:], true
	}
	return "", false
}

// stripComment removes a trailing comment. The scan is quote-aware so a `#`
// inside a quoted value stays part of the value; a `#` with no preceding space
// is scalar text, as in `anchor: main#1`.
func stripComment(text string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if c == '\\' {
				i++
			} else if c == '"' {
				inDouble = false
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '#':
			if i == 0 || text[i-1] == ' ' || text[i-1] == '\t' {
				return strings.TrimRight(text[:i], " \t")
			}
		}
	}
	return strings.TrimRight(text, " \t")
}

// checkConstructs rejects the YAML constructs outside the subset by name. It runs
// before the dash split so "- &anchor" is caught as an anchor, not as a scalar.
func checkConstructs(text string, num int) error {
	if rest, ok := dashRest(text); ok {
		text = strings.TrimLeft(rest, " \t")
		if text == "" {
			return nil
		}
	}
	switch {
	case strings.HasPrefix(text, "%"):
		return errf(num, "YAML directives are not supported")
	case strings.HasPrefix(text, "---"):
		return errf(num, "multi-document streams are not supported")
	case strings.HasPrefix(text, "..."):
		return errf(num, "document end markers are not supported")
	case strings.HasPrefix(text, "&"):
		return errf(num, "anchors are not supported")
	case strings.HasPrefix(text, "*"):
		return errf(num, "aliases are not supported")
	case strings.HasPrefix(text, "?"):
		return errf(num, "complex mapping keys are not supported")
	}
	return nil
}

// --- parsing ---

type parser struct {
	lines []pline
	pos   int
}

// parseBlock parses the block starting at pos into one value. indent is the
// column that block begins at, as reported by blockIndent.
func (p *parser) parseBlock(indent int) (any, error) {
	if p.pos >= len(p.lines) {
		return nil, nil
	}
	cur := p.lines[p.pos]
	if got := blockIndent(cur); got != indent {
		return nil, errf(cur.num, "unexpected indentation: expected %d, got %d", indent, got)
	}
	if cur.dashCol >= 0 {
		return p.parseSeq(indent)
	}
	return p.parseMap(indent)
}

func (p *parser) parseMap(indent int) (any, error) {
	m := NewMap()
	m.Line = p.lines[p.pos].num
	for p.pos < len(p.lines) {
		start := p.pos
		cur := p.lines[p.pos]
		if cur.dashCol >= 0 {
			if cur.dashCol > indent {
				return nil, errf(cur.num, "unexpected indentation: expected %d, got %d", indent, cur.dashCol)
			}
			break // a sequence at this column belongs to the enclosing block
		}
		if cur.indent < indent {
			break
		}
		if cur.indent > indent {
			return nil, errf(cur.num, "unexpected indentation: expected %d, got %d", indent, cur.indent)
		}
		key, val, ok, err := splitEntry(cur.text, cur.num)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errf(cur.num, "expected a %q entry, got the bare scalar %q", "key: value", cur.text)
		}
		if _, dup := m.Get(key); dup {
			return nil, errf(cur.num, "duplicate key %q", key)
		}
		p.pos++
		switch {
		case val != "":
			v, err := parseScalar(val, cur.num)
			if err != nil {
				return nil, err
			}
			m.SetAt(key, v, cur.num)
		case p.nextIsDeeperBlock(indent):
			v, err := p.parseBlock(blockIndent(p.lines[p.pos]))
			if err != nil {
				return nil, err
			}
			m.SetAt(key, v, cur.num)
		default:
			m.SetAt(key, nil, cur.num) // "key:" with nothing under it
		}
		if p.pos == start {
			// Unreachable while the branches above all advance pos. Kept because a
			// loop that can stall here silently swallows the rest of the document,
			// and a stalled parser is the exact failure this package is guarding
			// against.
			return nil, errf(cur.num, "internal parser error: no progress at this line")
		}
	}
	return m, nil
}

// nextIsDeeperBlock reports whether the next line opens a block belonging to the
// key just read. A block mapping or scalar must be indented past its key, but a
// block sequence may sit at the key's own column, which is the ordinary spelling
// for a list of scalars:
//
//	tags:
//	- one
//	- two
//
// Rejecting that form would refuse documents a large share of real YAML uses, and
// it is a shape, not a construct, so the subset has to cover it.
func (p *parser) nextIsDeeperBlock(indent int) bool {
	if p.pos >= len(p.lines) {
		return false
	}
	next := p.lines[p.pos]
	return blockIndent(next) > indent || (next.dashCol == indent && next.dashCol >= 0)
}

func (p *parser) parseSeq(dashCol int) (any, error) {
	seq := Seq{}
	for p.pos < len(p.lines) {
		start := p.pos
		cur := p.lines[p.pos]
		if cur.dashCol < dashCol {
			break
		}
		if cur.dashCol > dashCol {
			return nil, errf(cur.num, "unexpected indentation in sequence: dash at %d, expected %d", cur.dashCol, dashCol)
		}
		var v any
		var err error
		switch {
		case cur.text == "":
			// A lone "-": the item is whatever is indented under it.
			p.pos++
			if p.pos < len(p.lines) && blockIndent(p.lines[p.pos]) > dashCol {
				v, err = p.parseBlock(blockIndent(p.lines[p.pos]))
			} else {
				v = nil
			}
		case cur.text == "-" || strings.HasPrefix(cur.text, "- "):
			return nil, errf(cur.num, "nested sequences are not supported")
		default:
			_, _, ok, serr := splitEntry(cur.text, cur.num)
			if serr != nil {
				return nil, serr
			}
			if ok {
				// The item is a mapping whose first line is this one. Clearing the
				// dash marker hands that line to the mapping parser as an ordinary
				// entry, so it and the item's continuation lines parse the same way.
				p.lines[p.pos].dashCol = -1
				v, err = p.parseMap(cur.indent)
			} else {
				v, err = parseScalar(cur.text, cur.num)
				p.pos++
			}
		}
		if err != nil {
			return nil, err
		}
		seq = append(seq, v)
		if p.pos == start {
			return nil, errf(cur.num, "internal parser error: sequence made no progress at this line")
		}
	}
	return seq, nil
}

// splitEntry separates "key: value" on the first quote-aware ": " boundary. ok is
// false when the line is not an entry at all, which is how a bare sequence item
// is told apart from a mapping.
func splitEntry(text string, num int) (key, val string, ok bool, err error) {
	var inSingle, inDouble bool
	colon := -1
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			if c == '\\' {
				i++
			} else if c == '"' {
				inDouble = false
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == ':':
			if i+1 == len(text) || text[i+1] == ' ' {
				colon = i
			}
		}
		if colon >= 0 {
			break
		}
	}
	if colon < 0 {
		return "", "", false, nil
	}
	rawKey := strings.TrimRight(text[:colon], " ")
	rawVal := strings.TrimLeft(text[colon+1:], " ")
	switch {
	case rawKey == "":
		return "", "", false, errf(num, "empty key before %q", ":")
	case rawKey[0] == '"' || rawKey[0] == '\'':
		key, err = unquote(rawKey, num)
		if err != nil {
			return "", "", false, err
		}
	default:
		key = rawKey
	}
	if key == "<<" {
		return "", "", false, errf(num, "merge keys are not supported")
	}
	if strings.ContainsAny(key, "\n\t") {
		return "", "", false, errf(num, "key %q contains an unsupported control character", key)
	}
	if rawVal == "" {
		return key, "", true, nil
	}
	return key, rawVal, true, nil
}

func parseScalar(raw string, num int) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errf(num, "empty value")
	}
	// A node's first character is where a construct must appear, so checking the
	// first byte catches anchors, aliases, and tags in value position, which the
	// line-level prefix check cannot see. A "!" further along, as in
	// `title: Wow!`, is ordinary scalar text and stays allowed.
	switch raw[0] {
	case '&':
		return nil, errf(num, "anchors are not supported")
	case '*':
		return nil, errf(num, "aliases are not supported")
	case '!':
		return nil, errf(num, "explicit tags are not supported")
	case '[':
		// The spec's own §5.2 fixture writes an empty list as `[]`, and an empty
		// flow sequence is the same node as nothing, so it is supported. Every
		// other flow sequence is a construct no §5 shape uses.
		if raw == "[]" {
			return Seq{}, nil
		}
		return nil, errf(num, "flow sequences are not supported; use a block sequence")
	case '{':
		return nil, errf(num, "flow mappings are not supported; use a block mapping")
	case '>', '|':
		// Folded and literal block scalars, with or without a chomping indicator.
		// Naming them beats letting the indented continuation lines surface later
		// as a bare indentation error pointing at the wrong line.
		return nil, errf(num,
			"block scalars are not supported; quote the value on a single line")
	case '"', '\'':
		return unquote(raw, num)
	}
	if strings.Contains(raw, ": ") {
		return nil, errf(num, "unquoted value contains %q; quote the value", ": ")
	}
	return raw, nil
}

// unquote decodes the single- and double-quoted forms. The writer only ever emits
// these five double-quoted escapes, so the set is closed and round-trips exactly.
func unquote(raw string, num int) (string, error) {
	q := raw[0]
	if q == '\'' {
		body := raw[1:]
		if len(body) < 1 || !strings.HasSuffix(body, "'") {
			return "", errf(num, "unterminated single-quoted string")
		}
		body = body[:len(body)-1]
		var b strings.Builder
		for i := 0; i < len(body); i++ {
			if body[i] != '\'' {
				b.WriteByte(body[i])
				continue
			}
			// In a single-quoted string '' is the only escape for a quote.
			if i+1 < len(body) && body[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			return "", errf(num, "unescaped %q inside a single-quoted string", "'")
		}
		return b.String(), nil
	}
	var b strings.Builder
	body := raw[1:]
	closed := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch c {
		case '"':
			closed = true
			if rest := strings.TrimSpace(body[i+1:]); rest != "" {
				return "", errf(num, "unexpected %q after a double-quoted string", rest)
			}
			return b.String(), nil
		case '\\':
			i++
			if i >= len(body) {
				return "", errf(num, "trailing escape in a double-quoted string")
			}
			switch body[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				return "", errf(num, "unsupported escape %q in a double-quoted string", `\`+string(body[i]))
			}
		default:
			b.WriteByte(c)
		}
	}
	if !closed {
		return "", errf(num, "unterminated double-quoted string")
	}
	return b.String(), nil
}

// --- writing ---

// quoteIfNeeded returns val as a plain scalar when that re-reads unambiguously,
// and double-quoted otherwise. A value is quoted if it is empty, would re-read as
// a number or a YAML 1.1 boolean, starts with an indicator character, or contains
// a sequence end, a comment start, or a newline.
func quoteIfNeeded(val string) string {
	if !needsQuote(val) {
		return val
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(val); i++ {
		switch c := val[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func needsQuote(val string) bool {
	if val == "" {
		return true
	}
	if strings.TrimSpace(val) != val {
		return true
	}
	if strings.Contains(val, ": ") || strings.HasSuffix(val, ":") {
		return true
	}
	if strings.ContainsAny(val, "\n\r\t") {
		return true
	}
	if strings.ContainsRune("-?:,[]{}#&*!|>'\"%@`", rune(val[0])) {
		return true
	}
	switch strings.ToLower(val) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return true
	}
	if _, err := strconv.ParseFloat(val, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseInt(val, 0, 64); err == nil {
		return true
	}
	return false
}
