// Package shellwords splits command strings the way a POSIX shell would,
// without performing any expansion.
package shellwords

import (
	"errors"
	"strings"
)

// Split breaks s into words, honoring single quotes, double quotes and
// backslash escapes.
func Split(s string) ([]string, error) {
	var (
		words   []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped = true
			inWord = true
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape in command")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// NeedsShell reports whether s uses shell syntax that Split cannot represent.
func NeedsShell(s string) bool {
	return strings.ContainsAny(s, "|&;<>$`(){}*?~\n")
}

// Command converts a command string into an argv. Strings using shell syntax
// are wrapped in `sh -c`.
func Command(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if NeedsShell(s) {
		return []string{"sh", "-c", s}, nil
	}
	return Split(s)
}

// Quote returns s quoted for safe use as a single shell word.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\|&;<>$`(){}*?~#!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Join quotes and joins args into a shell command line.
func Join(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = Quote(a)
	}
	return strings.Join(out, " ")
}
