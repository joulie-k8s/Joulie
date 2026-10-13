package otlpjson

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// OJ06 (files): a FileWriter appends one whole object per line, which the
// strict decoder reads back; it refuses the other signal; concurrent
// writers never interleave; a partial last line left by a crash stays on a
// line of its own.
func TestOJ06FileWriterOneObjectPerLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metrics.jsonl")
	w, err := OpenFile(path, SignalMetrics)
	if err != nil {
		t.Fatal(err)
	}
	if w.Signal() != SignalMetrics || w.Path() != path {
		t.Errorf("writer = %s %s", w.Signal(), w.Path())
	}
	if err := w.WriteLogs(LogsData{}); err == nil || !strings.Contains(err.Error(), "one data type") {
		t.Errorf("WriteLogs on a metrics file = %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.WriteMetrics(exportTestMetrics()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteMetrics(exportTestMetrics()); err == nil {
		t.Error("WriteMetrics after Close succeeded")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	if len(lines) != 20 || !bytes.HasSuffix(b, []byte("\n")) {
		t.Fatalf("%d lines, want 20 each ending in a newline:\n%s", len(lines), b)
	}
	for i, l := range lines {
		d, err := DecodeMetricsStrict(l)
		if err != nil || len(d.ResourceMetrics) != 1 {
			t.Errorf("line %d: %v", i+1, err)
		}
	}

	// Reopen a file whose last line was cut short.
	logs := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(logs, []byte(`{"resourceLogs":[]}`+"\n"+`{"resourceLo`), 0o644); err != nil {
		t.Fatal(err)
	}
	lw, err := OpenFile(logs, SignalLogs)
	if err != nil {
		t.Fatal(err)
	}
	if err := lw.WriteMetrics(MetricsData{}); err == nil {
		t.Error("WriteMetrics on a logs file succeeded")
	}
	if err := lw.WriteLogs(LogsData{ResourceLogs: []ResourceLogs{}}); err != nil {
		t.Fatal(err)
	}
	if err := lw.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(logs)
	lines = bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
	if len(lines) != 3 || string(lines[1]) != `{"resourceLo` {
		t.Fatalf("after a cut line the file is\n%s", b)
	}
	if _, err := DecodeLogsStrict(lines[2]); err != nil {
		t.Errorf("the line written after a cut line: %v", err)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm()&^0o644 != 0 {
		t.Errorf("a new file has mode %v, want at most 0644", fi.Mode().Perm())
	}
	if _, err := OpenFile(filepath.Join(dir, "x.jsonl"), Signal("traces")); err == nil {
		t.Error("OpenFile accepted the traces signal")
	}
}
