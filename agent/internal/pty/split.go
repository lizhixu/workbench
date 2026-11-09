package pty

import (
	"fmt"
	"strings"
	"unicode"
)

// splitShellCommand splits a shell command line into argv words following
// POSIX shell word rules: words are separated by whitespace, single quotes
// keep their contents literal, double quotes allow backslash escapes, and
// a backslash outside quotes escapes the next character. It performs no
// expansion and no pipeline/redirection handling: the result is passed
// straight to exec.Command, so `docker exec -it <container> /bin/sh` starts
// docker with those arguments instead of being looked up as one file name.
func splitShellCommand(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	hasWord := false // true once the current word has any content (incl. empty quotes)
	quote := byte(0) // '\'' or '"' while inside quotes
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(runes) {
				// Inside double quotes a backslash escapes the next character.
				i++
				cur.WriteRune(runes[i])
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote = byte(c)
			hasWord = true
		case c == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			hasWord = true
		case unicode.IsSpace(c):
			if hasWord {
				words = append(words, cur.String())
				cur.Reset()
				hasWord = false
			}
		default:
			cur.WriteRune(c)
			hasWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote in shell command %q", s)
	}
	if hasWord {
		words = append(words, cur.String())
	}
	return words, nil
}
