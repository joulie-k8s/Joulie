package governor

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	joulie "github.com/matbun/joulie/pkg/api"
)

// errnoNames names the errnos Classify sorts, as the kernel spells them.
var errnoNames = map[syscall.Errno]string{
	syscall.EACCES:     "EACCES",
	syscall.EPERM:      "EPERM",
	syscall.EROFS:      "EROFS",
	syscall.EINVAL:     "EINVAL",
	syscall.EOPNOTSUPP: "EOPNOTSUPP",
	syscall.EBUSY:      "EBUSY",
}

// smiExitResults sorts the documented nvidia-smi return values (RETURN
// VALUE section of its manual): 4 "The current user does not have
// permission to access this device or perform this operation"; 2 "A
// supplied argument or flag is invalid"; 3 "The requested operation is not
// available on target device"; 13 "Local version of NVML doesn't implement
// this function". Any other code is left to the message.
var smiExitResults = map[int]string{
	4:  joulie.GovernorResultRefused,
	2:  joulie.GovernorResultUnsupported,
	3:  joulie.GovernorResultUnsupported,
	13: joulie.GovernorResultUnsupported,
}

// toolExitCode returns the exit code of a failed tool run in err, from an
// error in its chain with an ExitCode method: *exec.ExitError for a binary,
// the simulator's in-process fake tools for theirs. ok is false when the
// chain holds none.
func toolExitCode(err error) (code int, ok bool) {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode(), true
	}
	return 0, false
}

// Phrases of a failed nvidia-smi (or amd-smi) run, in lower case, that tell
// a missing privilege from a value the GPU does not take.
var (
	refusedPhrases     = []string{"insufficient permission", "permission denied", "no permission", "not permitted", "requires root", "requires elevation"}
	unsupportedPhrases = []string{"not supported", "unsupported", "does not work", "invalid argument", "invalid combination"}
)

// Classify turns the outcome of one write into a pkg/api GovernorResult*
// value and a message: the write's error (an errno from sysfs, or a
// nvidia-smi failure), the value written and the value read back after it.
//
// EACCES, EPERM and EROFS are refused; EINVAL, EOPNOTSUPP and EBUSY are
// unsupported; any other errno, and a missing tool, is error. A failed
// nvidia-smi is sorted by its documented exit code (smiExitResults), else
// by its message the same way. A write without error is applied when the
// readback equals the value (surrounding white space ignored), else
// not-applied: a silent no-op.
func Classify(writeErr error, want, readback string) (result, message string) {
	if writeErr == nil {
		w, r := strings.TrimSpace(want), strings.TrimSpace(readback)
		if w == r {
			return joulie.GovernorResultApplied, ""
		}
		return joulie.GovernorResultNotApplied, fmt.Sprintf("wrote %q but read back %q", w, r)
	}
	var errno syscall.Errno
	if errors.As(writeErr, &errno) {
		name, known := errnoNames[errno]
		if !known {
			return joulie.GovernorResultError, writeErr.Error()
		}
		msg := name + ": " + writeErr.Error()
		switch errno {
		case syscall.EACCES, syscall.EPERM, syscall.EROFS:
			return joulie.GovernorResultRefused, msg
		}
		return joulie.GovernorResultUnsupported, msg
	}
	if errors.Is(writeErr, exec.ErrNotFound) {
		return joulie.GovernorResultError, writeErr.Error()
	}
	if code, ok := toolExitCode(writeErr); ok {
		if result, known := smiExitResults[code]; known {
			return result, writeErr.Error()
		}
	}
	text := strings.ToLower(writeErr.Error())
	for _, p := range refusedPhrases {
		if strings.Contains(text, p) {
			return joulie.GovernorResultRefused, writeErr.Error()
		}
	}
	for _, p := range unsupportedPhrases {
		if strings.Contains(text, p) {
			return joulie.GovernorResultUnsupported, writeErr.Error()
		}
	}
	return joulie.GovernorResultError, writeErr.Error()
}

// ClassifyRun sorts one run of a GPU tool's set command, named command in
// the message: a run that failed as Classify sorts its error; a run that
// succeeded is unsupported when a line of its output says "not supported",
// in any case, and applied otherwise. nvidia-smi reports a setting the GPU
// does not support as a warning, "Setting locked memory clocks is not
// supported for GPU <bus id>.", then "Treating as warning and moving on.",
// and exits 0, so its exit code alone says nothing.
func ClassifyRun(command string, runErr error, out []byte) (result, message string) {
	if runErr != nil {
		return Classify(runErr, "", "")
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(strings.ToLower(line), "not supported") {
			return joulie.GovernorResultUnsupported, command + ": " + strings.TrimSpace(line)
		}
	}
	return joulie.GovernorResultApplied, ""
}
