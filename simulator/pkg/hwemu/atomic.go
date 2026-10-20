package hwemu

import (
	"bytes"
	"errors"
	"fmt"
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

// nodeSFile is one emulator-owned file (S). It is rewritten only when its
// content changes, in place under a write lease or by temp file and rename
// (writeS), so a reader never sees it empty or half written.
type nodeSFile struct {
	rel  string
	path string
	mode fs.FileMode
	last string
}

// nodeAtomicPrefix starts the name of the temp file of an S file's
// replacement. The temp file lives in the node's tmpDir and never next to an
// attribute, because filepath.Glob("*") in a zone or policy directory
// matches dotfiles.
const nodeAtomicPrefix = "s-"

// WriteFileAtomic replaces path with data through a temp file in tmpDir,
// named prefix and a random suffix: the data is written, the temp file
// gets mode and is renamed onto path, so a reader sees the old file or the
// new one, whole, never a partial write. tmpDir must be on the same file
// system as path. A temp file a crash leaves keeps its prefix, by which the
// caller finds and removes it. There is no fsync: the file survives a
// restart of its writer, not of the machine. An error names path and the
// step that failed, never the temp file, whose name is random, so a failure
// that repeats reads the same each time.
func WriteFileAtomic(tmpDir, prefix, path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(tmpDir, prefix)
	if err != nil {
		return atomicErr("create a temp file for", path, err)
	}
	name := f.Name()
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(name)
		return atomicErr("write a temp file for", path, err)
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = os.Remove(name)
		return atomicErr("chmod a temp file for", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return atomicErr("rename a temp file to", path, err)
	}
	return nil
}

// atomicErr is err of op on a temp file for path, with the temp file's name
// left out: the bare errno of a path or link error.
func atomicErr(op, path string, err error) error {
	var le *os.LinkError
	var pe *fs.PathError
	switch {
	case errors.As(err, &le):
		err = le.Err
	case errors.As(err, &pe):
		err = pe.Err
	}
	return fmt.Errorf("%s %s: %w", op, path, err)
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE|nodeNoFollow, mode)
	if errors.Is(err, fs.ErrPermission) && statErr == nil {
		var restore func()
		f, restore, err = nodeOpenOwned(path, os.O_WRONLY|os.O_TRUNC|nodeNoFollow, err)
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
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
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
	f, err := os.OpenFile(path, os.O_RDWR|nodeNoFollow, 0)
	if errors.Is(err, fs.ErrPermission) {
		var restore func()
		f, restore, err = nodeOpenOwned(path, os.O_RDWR|nodeNoFollow, err)
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
// starting point: an unchanged value is not rewritten. A Live node indexes
// it by path (liveS).
func (n *Node) sfile(abs string) *nodeSFile {
	fi, err := os.Lstat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	rel := n.relPath(abs)
	f := &nodeSFile{rel: rel, path: abs, mode: fi.Mode().Perm()}
	if c, ok := n.tree.rendered[rel]; ok && nodeRenderedAsIs(fi, c) {
		f.last = c
	} else if b, err := os.ReadFile(abs); err == nil {
		f.last = string(b)
	} else {
		return nil
	}
	if n.liveS != nil {
		n.liveS[rel] = f
	}
	return f
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

// readFile is the content of the file at path. A file of a laid-out tree
// that still has the render's length and mark (nodeRenderedAsIs) holds
// what the render gave it, so it is not read; any other file is read, so a
// tree's own value, such as a capture's or a limit the agent wrote, wins.
func (n *Node) readFile(path string) ([]byte, error) {
	if c, ok := n.tree.rendered[n.relPath(path)]; ok {
		if fi, err := os.Lstat(path); err == nil && nodeRenderedAsIs(fi, c) {
			return []byte(c), nil
		}
	}
	return os.ReadFile(path)
}

// nodeRenderedAsIs reports whether a file of a laid-out tree with info fi
// still holds content as the render wrote it: a regular file of that
// length whose modification time is still the mark the render gives every
// file (and NewNode every render-owned one), which any write moves.
func nodeRenderedAsIs(fi fs.FileInfo, content string) bool {
	return fi.Mode().IsRegular() && fi.Size() == int64(len(content)) && fi.ModTime().Equal(nodeRenderMTime)
}

// readInt reads a file holding one decimal integer, as sysfs prints it.
func (n *Node) readInt(path string) (int64, bool) {
	b, err := n.readFile(path)
	if err != nil {
		return 0, false
	}
	v, ok := nodeParseInt(strings.TrimSpace(string(b)))
	return v, ok
}

// readString reads a file and trims the newline sysfs appends.
func (n *Node) readString(path string) (string, bool) {
	b, err := n.readFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}
