//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package emulate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ledgerNoFollow keeps a ledger open from following a symbolic link.
const ledgerNoFollow = syscall.O_NOFOLLOW

// lockRoot takes an exclusive flock(2) on <root>/LockFile, so two simulators
// never step the same trees: both would write every counter, and the ledgers
// would count the same energy twice. The lock belongs to the open file, so a
// second Emulator in the same process is refused as well, and it goes away
// with the process, so a crash leaves nothing to clean up.
func lockRoot(root string) (unlock func() error, err error) {
	f, err := os.OpenFile(filepath.Join(root, LockFile), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("emulate: lock %s: %w", root, err)
	}
	rc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("emulate: lock %s: %w", root, err)
	}
	var lockErr error
	if err := rc.Control(func(fd uintptr) {
		lockErr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
	}); err != nil {
		lockErr = err
	}
	if lockErr != nil {
		_ = f.Close()
		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("emulate: another simulator holds %s", root)
		}
		return nil, fmt.Errorf("emulate: lock %s: %w", root, lockErr)
	}
	// Closing the file releases the lock.
	return f.Close, nil
}
