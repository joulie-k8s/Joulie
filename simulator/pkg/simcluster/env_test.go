package simcluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ParseEnvFile reads what the scripts write and source: bare, quoted and
// escaped words, bash arrays, comments and blank lines, with line numbers;
// it refuses a line that is not KEY=VALUE and a quote left open.
func TestParseEnvFile(t *testing.T) {
	text := "# comment\n\nA=plain\nB='it'\\''s'\nC=\"double\"\nD=(one 'two words' \"three\")\nE=\nF=()\nG=a b\nH=x=y\n"
	vars, err := ParseEnvFile([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvVar{
		{Line: 3, Key: "A", Value: "plain", Words: []string{"plain"}},
		{Line: 4, Key: "B", Value: `'it'\''s'`, Words: []string{"it's"}},
		{Line: 5, Key: "C", Value: `"double"`, Words: []string{"double"}},
		{Line: 6, Key: "D", Value: `(one 'two words' "three")`, Words: []string{"one", "two words", "three"}, Array: true},
		{Line: 7, Key: "E", Value: "", Words: []string{""}},
		{Line: 8, Key: "F", Value: "()", Array: true},
		{Line: 9, Key: "G", Value: "a b", Words: []string{"a", "b"}},
		{Line: 10, Key: "H", Value: "x=y", Words: []string{"x=y"}},
	}
	if !reflect.DeepEqual(vars, want) {
		t.Errorf("ParseEnvFile:\n%+v\nwant\n%+v", vars, want)
	}
	for text, msg := range map[string]string{
		"A=1\nexport B\n": "line 2: \"export B\" is not KEY=VALUE",
		"=1\n":            "is not KEY=VALUE",
		"A='open\n":       "line 1: unterminated quote",
	} {
		if _, err := ParseEnvFile([]byte(text)); err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("%q: error %v, want one containing %q", text, err, msg)
		}
	}
	for _, s := range []string{"", "plain", "it's", "a b", `back\slash`, "'''"} {
		vars, err := ParseEnvFile([]byte("K=" + ShellQuote(s) + "\n"))
		if err != nil || len(vars) != 1 || !reflect.DeepEqual(vars[0].Words, []string{s}) {
			t.Errorf("ShellQuote(%q) reads back as %+v, %v", s, vars, err)
		}
	}
}

// ReadEnvFile keeps each value as written, the last line of a key winning,
// and names the file in an error.
func TestReadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.env")
	if err := os.WriteFile(path, []byte("JOULIE_VALUES=/a b/x.yaml,/c.yaml\nTIMING=dev\nTIMING=production\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"JOULIE_VALUES": "/a b/x.yaml,/c.yaml", "TIMING": "production"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ReadEnvFile = %v, want %v", got, want)
	}
	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("not a pair\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEnvFile(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Errorf("ReadEnvFile(bad): %v, want an error naming the file", err)
	}
}
