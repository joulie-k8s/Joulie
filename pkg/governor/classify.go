package governor

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
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

// exitStatusText is how *exec.ExitError prints a non-zero exit, a text the
// simulator's in-process fake tools copy.
const exitStatusText = "exit status "

// toolExitCode returns the exit code of a failed tool run in err: from an
// error with an ExitCode method (*exec.ExitError), else from the first
// "exit status N" in its text. ok is false when err holds neither.
func toolExitCode(err error) (code int, ok bool) {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode(), true
	}
	text := err.Error()
	i := strings.Index(text, exitStatusText)
	if i < 0 {
		return 0, false
	}
	digits := text[i+len(exitStatusText):]
	n := 0
	for n < len(digits) && digits[n] >= '0' && digits[n] <= '9' {
		n++
	}
	code, convErr := strconv.Atoi(digits[:n])
	return code, convErr == nil
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
