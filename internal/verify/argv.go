// Package verify owns how ocaw turns a stored command string into a running
// process: parsing an argv, and running it with a deadline.
//
// The parser lives here rather than in state or doctor because three places
// need it and they must agree. A gate command stored in state.json, a verify_cmd
// recorded in agent.yaml, and a command an agent typed on the command line are
// all the same kind of string. Two parsers for one format means a command ocaw
// accepts in one place and refuses in another, and the user finds out which by
// trying it.
//
// The one rule that shapes the whole package: an argv, never a shell. ocaw has
// no reason to interpolate, expand, or run a pipeline on a string a human read
// off a config file, and every shell construct it does not implement is a
// quoting bug someone will report.
package verify

import (
	"fmt"
	"strings"
)

// shellOperators are refused wherever they appear unquoted. There is no shell
// in ocaw — it calls exec directly — so an unquoted `&&` is not a pipeline that
// would misbehave, it is someone writing a command for a shell that is not
// there. Refusing it says so.
//
// Inside quotes the same characters are inert literal text, and refusing them
// would cost real expressiveness: `-run 'TestA && TestB'` and
// `-run 'TestSub|^other'` are ordinary test selections. Rejecting those because
// a character that means nothing here happens to appear inside them is the kind
// of strictness that teaches people to work around the tool.
var shellOperators = []string{
	"&&", "||", ">>", "<<",
	"|", ";", "<", ">", "(", ")", "$", "`", "&", "\\",
}

// maxArgvTokens bounds the token count. Nothing legitimate reaches this, and a
// string that does is a mistake worth reporting rather than allocating for.
const maxArgvTokens = 64

// Tokenize splits a stored command string into an argv.
//
// It accepts whitespace-separated tokens, single-quoted and double-quoted
// strings, and nothing else. There are no escapes and no interpolation: a
// backslash is refused rather than passed through as a literal, because a
// command whose backslash handling depends on which of the two the user typed is
// a command nobody can predict.
//
// An error names the column and the construct, because a refusal an agent
// cannot act on is a refusal it will work around.
func Tokenize(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, fmt.Errorf("the command is empty")
	}
	var (
		out     []string
		cur     strings.Builder
		started bool
	)
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}

	for i := 0; i < len(s); {
		r, size := decodeRune(s[i:])
		switch {
		case r == '\'' || r == '"':
			quote := r
			flush()
			started = true
			closed := false
			i += size
			for i < len(s) {
				q, qsize := decodeRune(s[i:])
				if q == quote {
					closed = true
					i += qsize
					break
				}
				// Newline inside a quoted argument still breaks the command: a
				// gate that spans lines is a paste accident, not a command.
				if q == '\n' || q == '\r' {
					return nil, fmt.Errorf("column %d: a quoted argument spans a line break", i+1)
				}
				cur.WriteRune(q)
				i += qsize
			}
			if !closed {
				return nil, fmt.Errorf("column %d: the %s quote opened here is never closed", i+1, quoteName(quote))
			}

		case r == ' ' || r == '\t':
			flush()
			i += size

		case r == '\n' || r == '\r':
			// A newline is whitespace to a shell, which is exactly why a
			// one-line config field holding two is a mistake. Refusing names the
			// problem instead of quietly running the first line.
			return nil, fmt.Errorf(
				"column %d: the command spans a line break; a gate or verify_cmd is a single line", i+1)

		default:
			if op := operatorAt(s[i:]); op != "" {
				return nil, fmt.Errorf(
					"column %d holds %q, which is a shell operator; ocaw runs an argv, not a shell",
					i+1, op)
			}
			cur.WriteRune(r)
			started = true
			i += size
		}
	}
	flush()

	if len(out) == 0 {
		return nil, fmt.Errorf("the command is empty")
	}
	if len(out) > maxArgvTokens {
		return nil, fmt.Errorf("the command has %d arguments, over the limit of %d", len(out), maxArgvTokens)
	}
	return out, nil
}

// operatorAt returns the shell operator at the start of s, longest first so
// `&&` is named as `&&` rather than as two `&`.
func operatorAt(s string) string {
	for _, op := range shellOperators {
		if strings.HasPrefix(s, op) {
			return op
		}
	}
	return ""
}

func quoteName(r rune) string {
	if r == '\'' {
		return "single"
	}
	return "double"
}

func decodeRune(s string) (rune, int) {
	for _, r := range s {
		return r, len(string(r))
	}
	return 0, 1
}

// MustTokenize is Tokenize for a string ocaw itself wrote. It panics on
// failure, which is correct: a bad command in that position is an ocaw bug, and
// a bug should stop the program rather than produce a wrong argv.
func MustTokenize(s string) []string {
	argv, err := Tokenize(s)
	if err != nil {
		panic(fmt.Sprintf("verify: ocaw wrote an unparseable command %q: %v", s, err))
	}
	return argv
}
