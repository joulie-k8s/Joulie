package otlpjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// FileWriter appends exports of one signal to a file in the format of the
// OTel file exporter (specification v1.61.0, protocol/file-exporter.md): JSON
// Lines, one MetricsData or LogsData object per line, one data type per
// file. A line is byte for byte an Export request body, so it can be POSTed
// as it is. It is safe for concurrent use.
type FileWriter struct {
	mu     sync.Mutex
	f      *os.File
	path   string
	signal Signal
}

// OpenFile opens path for appending exports of signal, creating it with
// mode 0644. When the file ends in a partial line, as after a crash in the
// middle of a write, a newline is added first, so that line stays alone and
// the next one is whole.
func OpenFile(path string, signal Signal) (*FileWriter, error) {
	if signal != SignalMetrics && signal != SignalLogs {
		return nil, fmt.Errorf("otlpjson: open %s: unknown signal %q", path, signal)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("otlpjson: open %s: %w", path, err)
	}
	if err := fileEndLine(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("otlpjson: open %s: %w", path, err)
	}
	return &FileWriter{f: f, path: path, signal: signal}, nil
}

// Path is the file the writer appends to.
func (w *FileWriter) Path() string { return w.path }

// Signal is the one signal the file holds.
func (w *FileWriter) Signal() Signal { return w.signal }

// WriteMetrics appends d as one line. It fails on a logs file.
func (w *FileWriter) WriteMetrics(d MetricsData) error { return w.write(SignalMetrics, d) }

// WriteLogs appends d as one line. It fails on a metrics file.
func (w *FileWriter) WriteLogs(d LogsData) error { return w.write(SignalLogs, d) }

// Close closes the file. Later writes fail.
func (w *FileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

func (w *FileWriter) write(s Signal, v any) error {
	if s != w.signal {
		return fmt.Errorf("otlpjson: %s holds %s; a file holds one data type, not %s", w.path, w.signal, s)
	}
	line, err := fileMarshalLine(v)
	if err != nil {
		return fmt.Errorf("otlpjson: encode %s for %s: %w", s, w.path, err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return fmt.Errorf("otlpjson: %s is closed", w.path)
	}
	// One write per line: with O_APPEND a line is never interleaved with
	// another writer's.
	if _, err := w.f.Write(line); err != nil {
		return fmt.Errorf("otlpjson: write %s: %w", w.path, err)
	}
	return nil
}

// fileMarshalLine encodes v as one JSON line ending in a newline. HTML
// characters are kept as they are, so a line reads as it was written.
func fileMarshalLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// fileEndLine appends a newline to a non-empty file whose last byte is not
// one.
func fileEndLine(f *os.File) error {
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	_, err = f.Write([]byte{'\n'})
	return err
}
