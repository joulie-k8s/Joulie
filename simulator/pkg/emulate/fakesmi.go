package emulate

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// fakesmiMode is the mode of the installed fakesmi: every bin/<tool> link of
// every node points at it, and the agent runs it as whatever user its pod
// has.
const fakesmiMode fs.FileMode = 0o755

// installFakeSMI copies the fakesmi binary at src to <root>/LibDir/FakeSMIName,
// which each node's bin/<tool> links reach as /emu/lib/fakesmi. An empty src
// skips the copy. The copy goes to a temp file in the same directory and is
// renamed into place, never written over the old file: an agent may be
// running the old binary, and Linux refuses to open a running executable for
// writing (ETXTBSY). A destination that already has the same sha256 is left
// alone, keeping its inode and mtime; installed reports whether a copy was
// made.
func installFakeSMI(root, src string) (installed bool, err error) {
	if src == "" {
		return false, nil
	}
	want, err := fakesmiSum(src)
	if err != nil {
		return false, fmt.Errorf("emulate: fakesmi source: %w", err)
	}
	dir := filepath.Join(root, LibDir)
	dst := filepath.Join(dir, FakeSMIName)
	if got, err := fakesmiSum(dst); err == nil && bytes.Equal(got, want) {
		fi, err := os.Stat(dst)
		if err != nil {
			return false, fmt.Errorf("emulate: fakesmi: %w", err)
		}
		if fi.Mode().Perm() != fakesmiMode {
			if err := os.Chmod(dst, fakesmiMode); err != nil {
				return false, fmt.Errorf("emulate: fakesmi: %w", err)
			}
		}
		return false, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("emulate: fakesmi: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("emulate: fakesmi: %w", err)
	}
	if err := fakesmiCopy(src, dir, dst); err != nil {
		return false, fmt.Errorf("emulate: fakesmi: %w", err)
	}
	return true, nil
}

func fakesmiSum(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// fakesmiCopy copies src to a temp file in dir and renames it onto dst.
func fakesmiCopy(src, dir, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, "."+FakeSMIName+"-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, cerr := io.Copy(tmp, in)
	if err := errors.Join(cerr, tmp.Close()); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, fakesmiMode); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}
