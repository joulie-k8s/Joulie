//go:build linux

package hwfuse

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Bind bind-mounts the directory src at dst, creating dst and its parents
// when they are missing, as root does with CAP_SYS_ADMIN. dst may already
// be a mount point, even one whose FUSE server is gone: the new mount goes
// on top of it, and a mount that propagates from the old one, such as a
// container's volume with mount propagation HostToContainer, gets the new
// one too. Nothing is ever unmounted from dst, since an unmount would end
// that propagation.
func Bind(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("hwfuse: %w", err)
	}
	// A mount point whose server is gone fails a stat with ENOTCONN, while
	// mkdir sees the name taken without reaching it.
	if err := os.Mkdir(dst, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("hwfuse: %w", err)
	}
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("hwfuse: bind %s at %s: %w", src, dst, err)
	}
	return nil
}
