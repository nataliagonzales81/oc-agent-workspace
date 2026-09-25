package yaml

import "strings"

// Frontmatter field names, exported so doctor can name them in its own errors
// without repeating the literals.
const (
	KeyName         = "name"
	KeyVersion      = "version"
	KeyDescription  = "description"
	KeyAllowedTools = "allowed-tools"
	KeyHidden       = "hidden"
)

// Frontmatter is the leading `---` block of a SKILL.md file (SPEC §5.3). Only name
// is required; the rest default to the zero value, and a key outside this set is
// an error rather than a discard, because a skill's frontmatter is how an agent
// decides whether to load it at all.
type Frontmatter struct {
	Name         string
	Version      string
	Description  string
	AllowedTools []string
	Hidden       bool
}

// ParseFrontmatter reads the leading `---` block of doc. Nothing after the
// closing fence is parsed: a markdown body routinely contains `---` rules and
// YAML-invalid text, and treating those as document structure would reject files
// that are perfectly good skill docs.
func ParseFrontmatter(doc string) (Frontmatter, error) {
	block, firstLine, err := frontmatterBlock(doc)
	if err != nil {
		return Frontmatter{}, err
	}
	v, err := Parse(block, firstLine)
	if err != nil {
		return Frontmatter{}, err
	}
	if strings.TrimSpace(block) == "" {
		return Frontmatter{}, errf(firstLine, "frontmatter is empty; at least %q is required", KeyName)
	}
	m, err := mustMap(v, "frontmatter")
	if err != nil {
		return Frontmatter{}, err
	}
	if err := checkKeys(m, "frontmatter", KeyName, KeyVersion, KeyDescription, KeyAllowedTools, KeyHidden); err != nil {
		return Frontmatter{}, err
	}

	var f Frontmatter
	if f.Name, err = str(m, KeyName, "frontmatter"); err != nil {
		return Frontmatter{}, err
	}
	if strings.TrimSpace(f.Name) == "" {
		return Frontmatter{}, errf(m.Line, "frontmatter: %q must not be empty", KeyName)
	}
	if f.Version, err = optStr(m, KeyVersion, "frontmatter"); err != nil {
		return Frontmatter{}, err
	}
	if f.Description, err = optStr(m, KeyDescription, "frontmatter"); err != nil {
		return Frontmatter{}, err
	}
	tools, err := optStr(m, KeyAllowedTools, "frontmatter")
	if err != nil {
		return Frontmatter{}, err
	}
	f.AllowedTools = splitTools(tools)
	if f.Hidden, err = optBool(m, KeyHidden, "frontmatter", false); err != nil {
		return Frontmatter{}, err
	}
	return f, nil
}

// splitTools reads the comma-separated `allowed-tools` scalar. §5.3 writes it as
// one string rather than a YAML list, and a list here is a type error, not a
// second spelling: accepting both would leave a file that reads as valid to ocaw
// and invalid to whatever renders these docs.
func splitTools(tools string) []string {
	if strings.TrimSpace(tools) == "" {
		return nil
	}
	parts := strings.Split(tools, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// frontmatterBlock extracts the text between the opening and closing `---` fences
// and reports the 1-based file line the block starts on, so parse errors inside it
// point at the real line in the file rather than at a line in a sub-document.
func frontmatterBlock(doc string) (string, int, error) {
	body := strings.TrimPrefix(doc, "\ufeff")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", 0, errf(1, "missing frontmatter: the file must begin with a %q line", "---")
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), 2, nil
		}
	}
	return "", 0, errf(len(lines), "unterminated frontmatter: no closing %q line", "---")
}
