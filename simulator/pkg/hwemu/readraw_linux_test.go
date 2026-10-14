//go:build linux

package hwemu

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A file's stamp vouches for its content only while nothing wrote it since
// the read and its change time is older than the read by nodeStampMargin;
// any write, even of the same bytes, moves the change time and voids it.
func TestFileStampVouchesOnlyForAnUntouchedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "power_limit_uw")
	if err := os.WriteFile(path, []byte("165000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf []byte
	var s nodeFileStamp
	b, _, err := nodeReadA(path, &buf, &s, time.Now())
	if err != nil || string(b) != "165000000\n" {
		t.Fatalf("read %q, %v", b, err)
	}
	if s.unchanged(path) {
		t.Error("a stamp read right after the write vouches for the file; the write may share its change time")
	}
	time.Sleep(nodeStampMargin + 50*time.Millisecond)
	if _, _, err := nodeReadA(path, &buf, &s, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !s.unchanged(path) {
		t.Fatal("an untouched file with an old change time is not skipped")
	}
	if err := os.WriteFile(path, []byte("165000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s.unchanged(path) {
		t.Error("a write of the same bytes left the stamp valid")
	}
	if _, _, err := nodeReadA(filepath.Join(filepath.Dir(path), "absent"), &buf, &s, time.Now()); !os.IsNotExist(err) {
		t.Errorf("a missing file: %v, want not exist", err)
	}
}
