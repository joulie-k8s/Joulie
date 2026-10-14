package simcluster

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
)

// EnvVar is one KEY=VALUE line of a file the simulated cluster scripts
// write or source: a timing file, deploy.env, images.env, a scenario's
// variant.env. Line is its 1-based line number and Value the text after
// the first '=', as written. Words is the value as bash reads it: one word
// for a scalar (the empty string for an empty value), the elements of a
// bash array (...) otherwise.
type EnvVar struct {
	Line  int
	Key   string
	Value string
	Words []string
	Array bool
}

// ParseEnvFile reads the KEY=VALUE lines of b in file order, skipping blank
// lines and # comments. A value is bare, single-quoted (each quote inside
// written as a closing quote, an escaped quote and an opening quote, as
// ShellQuote writes it), double-quoted without escapes, a
// backslash-escaped character, or a bash array of such words. A line that is
// not KEY=VALUE, or a quote left open, is an error naming the line. A scalar
// with more than one word is returned as it is; whether that is an error is
// the caller's to say, since bash runs such a line as a command.
func ParseEnvFile(b []byte) ([]EnvVar, error) {
	var out []EnvVar
	sc := bufio.NewScanner(bytes.NewReader(b))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("line %d: %q is not KEY=VALUE", n, line)
		}
		e := EnvVar{Line: n, Key: k, Value: v}
		words := v
		if e.Array = strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")"); e.Array {
			words = v[1 : len(v)-1]
		}
		var err error
		if e.Words, err = envWords(words); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if !e.Array && len(e.Words) == 0 {
			e.Words = []string{""}
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// ReadEnvFile reads the file at path with ParseEnvFile and returns each
// key's value as written, the last line winning for a key set twice, as when
// bash sources the file.
func ReadEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	vars, err := ParseEnvFile(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]string, len(vars))
	for _, v := range vars {
		out[v.Key] = v.Value
	}
	return out, nil
}

// ShellQuote single-quotes s for bash, the quoting ParseEnvFile reads back.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// envWords splits s into words: bare runs, double-quoted runs without
// escapes, single-quoted runs, and backslash-escaped characters.
func envWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			end := strings.IndexByte(s[i+1:], c)
			if end < 0 {
				return nil, fmt.Errorf("unterminated quote in %q", s)
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(s[i+1])
			i++
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
