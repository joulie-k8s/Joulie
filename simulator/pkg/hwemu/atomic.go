package hwemu

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// nodeLeases lets a new Node use leases where they work. Tests turn it off
// to run the unguarded path; those tests never call t.Parallel.
var nodeLeases = true

// nodeLeasesWork reports whether a write lease can be taken on a fresh file in
// dir. A Node probes its own tree this way once: on a file system that has no
// leases, or that refuses them even on a file nobody else holds open, the Node
// writes unguarded rather than taking every file for busy and never writing.
func nodeLeasesWork(dir string) bool {
	f, err := os.CreateTemp(dir, "lease-")
	if err != nil {
		return false
	}
	defer os.Remove(f.Name())
	defer f.Close()
	release, _, ok := nodeLease(f)
	release()
	return ok
}

// nodeSFile is one emulator-owned file (S). It is rewritten by temp file and
// rename, only when its content changes, so a reader never sees it empty or
// half written.
type nodeSFile struct {
	rel  string
	path string
	mode fs.FileMode
	last string
}

// nodeWriteAtomic replaces path with data through a temp file in tmpDir. The
// temp file never sits next to an attribute, because filepath.Glob("*") in a
// zone or policy directory matches dotfiles. tmpDir must be on the same file
// system as path; both live under the node root.
func nodeWriteAtomic(tmpDir, path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(tmpDir, "s-")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// nodeWriteInPlace truncates path and writes data into the same inode, as the
// agent does with os.WriteFile. Agent-writable files and render-owned files
// are only ever written this way: a file bind mount, such as cpuinfo over
// /proc/cpuinfo, pins the inode, so a rename would never reach the pod. A
// read-only file is made writable for the write when the caller owns it,
// which a non-root test needs. A missing file is created with mode.
func nodeWriteInPlace(path string, data []byte, mode fs.FileMode) error {
	_, statErr := os.Lstat(path)
	created := errors.Is(statErr, fs.ErrNotExist)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, mode)
	if errors.Is(err, fs.ErrPermission) && statErr == nil {
		var restore func()
		f, restore, err = nodeOpenOwned(path, os.O_WRONLY|os.O_TRUNC, err)
		defer restore()
	}
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return err
	}
	if created {
		return os.Chmod(path, mode)
	}
	return nil
}

// nodeOpenOwned opens path with flag after denied, the error of a first try,
// by making the file writable for the time of the write, which works when
// the caller owns it. restore puts the mode back; it is safe to call when the
// open fails too.
func nodeOpenOwned(path string, flag int, denied error) (f *os.File, restore func(), err error) {
	restore = func() {}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, restore, denied
	}
	if os.Chmod(path, fi.Mode().Perm()|0o200) != nil {
		return nil, restore, denied
	}
	restore = func() { _ = os.Chmod(path, fi.Mode().Perm()) }
	f, err = os.OpenFile(path, flag, 0)
	return f, restore, err
}

// nodeRewriteInPlace is the in-place write of an A file the readback
// normalizes or restores. It writes data only while the file still holds
// seen, the content the readback acted on, and, when mtime is not zero, still
// carries that modification time; otherwise the agent wrote since, its value
// wins, and nodeRewriteInPlace reports false without writing. It also reports
// false while the file is open elsewhere, a writer between truncate and write.
// With lease, the check and the write run under a lease, so no open can slip
// between them (see readAFile).
func nodeRewriteInPlace(path string, seen, data []byte, mtime time.Time, lease bool) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrPermission) {
		var restore func()
		f, restore, err = nodeOpenOwned(path, os.O_RDWR, err)
		defer restore()
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	release := func() {}
	if lease {
		var busy bool
		if release, busy, _ = nodeLease(f); busy {
			return false, nil
		}
	}
	defer release()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if fi.Size() != int64(len(seen)) || !mtime.IsZero() && !fi.ModTime().Equal(mtime) {
		return false, nil
	}
	cur := make([]byte, len(seen)+1)
	k, err := f.ReadAt(cur, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if !bytes.Equal(cur[:k], seen) {
		return false, nil
	}
	if err := f.Truncate(0); err != nil {
		return false, err
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		return false, err
	}
	// Closing the file also drops the lease, letting a waiting open through.
	return true, f.Close()
}

// sfile returns the S file at abs, or nil when the tree has none there, so a
// Node only ever writes files Render created. The content on disk is the
// starting point: an unchanged value is not rewritten.
func (n *Node) sfile(abs string) *nodeSFile {
	fi, err := os.Lstat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	return &nodeSFile{rel: n.relPath(abs), path: abs, mode: fi.Mode().Perm(), last: string(b)}
}

// writeS writes content to f when it differs from what f last held.
func (n *Node) writeS(f *nodeSFile, content string) error {
	if f == nil || content == f.last {
		return nil
	}
	if err := nodeWriteAtomic(n.tmpDir, f.path, []byte(content), f.mode); err != nil {
		return err
	}
	f.last = content
	return nil
}

// nodeReadWithMTime reads path and the modification time it had before the
// read. A write that lands between the two leaves an older time with the new
// content, which only ever delays the restore of an empty file.
func nodeReadWithMTime(path string) ([]byte, time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, time.Time{}, err
	}
	b, err := io.ReadAll(f)
	return b, fi.ModTime(), err
}

// nodeReadInt reads a file holding one decimal integer, as sysfs prints it.
func nodeReadInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, ok := nodeParseInt(strings.TrimSpace(string(b)))
	return v, ok
}

// nodeReadString reads a file and trims the newline sysfs appends.
func nodeReadString(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}
