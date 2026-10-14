package governor

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
	fakesmi "github.com/matbun/joulie/simulator/pkg/hwemu/smi"
)

// GA11: Classify sorts a write's outcome: no permission is refused, a value
// the driver rejects is unsupported, a write that reads back different is
// not-applied, anything else is error.
func TestGA11ClassifySortsWriteOutcomes(t *testing.T) {
	pathErr := func(errno syscall.Errno) error {
		return &fs.PathError{Op: "write", Path: "/host-sys/devices/system/cpu/cpufreq/policy0/scaling_governor", Err: errno}
	}
	for _, tc := range []struct {
		name     string
		err      error
		want     string
		readback string
		result   string
		inMsg    string
	}{
		{"EACCES", pathErr(syscall.EACCES), "conservative", "", joulie.GovernorResultRefused, "EACCES"},
		{"EPERM", pathErr(syscall.EPERM), "conservative", "", joulie.GovernorResultRefused, "EPERM"},
		{"EROFS", pathErr(syscall.EROFS), "conservative", "", joulie.GovernorResultRefused, "EROFS"},
		{"wrapped EACCES", fmt.Errorf("governor write: %w", pathErr(syscall.EACCES)), "x", "", joulie.GovernorResultRefused, "EACCES"},
		{"EINVAL", pathErr(syscall.EINVAL), "high", "", joulie.GovernorResultUnsupported, "EINVAL"},
		{"EOPNOTSUPP", pathErr(syscall.EOPNOTSUPP), "high", "", joulie.GovernorResultUnsupported, "EOPNOTSUPP"},
		{"EBUSY", pathErr(syscall.EBUSY), "balance_power", "", joulie.GovernorResultUnsupported, "EBUSY"},
		{"ENOENT", pathErr(syscall.ENOENT), "high", "", joulie.GovernorResultError, "no such file"},
		{"silent no-op", nil, "high", "auto\n", joulie.GovernorResultNotApplied, `read back "auto"`},
		{"read back equal", nil, "schedutil", "schedutil\n", joulie.GovernorResultApplied, ""},
		{"nvidia-smi without root", errors.New("exit status 4: Insufficient Permissions"), "", "", joulie.GovernorResultRefused, "Insufficient"},
		{"nvidia-smi on a family without the lock", errors.New("exit status 3: Setting locked memory clocks is not supported for GPU 00000000:18:00.0."), "", "", joulie.GovernorResultUnsupported, "not supported"},
		{"missing tool", &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}, "", "", joulie.GovernorResultError, "nvidia-smi"},
		{"anything else", errors.New("exit status 15: GPU is lost"), "", "", joulie.GovernorResultError, "GPU is lost"},
	} {
		result, msg := Classify(tc.err, tc.want, tc.readback)
		if result != tc.result {
			t.Errorf("%s: result %q, want %q (message %q)", tc.name, result, tc.result, msg)
		}
		if !strings.Contains(msg, tc.inMsg) || (tc.inMsg == "" && msg != "") {
			t.Errorf("%s: message %q, want it to name %q", tc.name, msg, tc.inMsg)
		}
	}
}

// codeless hides the text of a wrapped error, so a classification can only
// come from the error value.
type codeless struct{ err error }

func (c codeless) Error() string { return "the tool failed" }
func (c codeless) Unwrap() error { return c.err }

// exitOf runs a shell that exits with code and returns its *exec.ExitError.
func exitOf(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != code {
		t.Fatalf("sh exit %d: %v, want an *exec.ExitError with that code", code, err)
	}
	return err
}

// Classify sorts a failed nvidia-smi by its documented return value (4: the
// user has no permission; 2: an invalid argument; 3: not available on the
// device; 13: not implemented by the local NVML) before its message, whose
// wording is not documented: the output here names no known phrase. The
// code comes from the error value, for the real binary (*exec.ExitError) and
// for the simulator's in-process fake (fakesmi.ExitError) alike, also when the
// "exit status N" text is not in the message.
func TestClassifySortsNvidiaSMIByExitCode(t *testing.T) {
	const output = "Terminating early due to previous errors."
	wrap := func(err error) error {
		return fmt.Errorf("nvidia-smi -i 00000000:18:00.0 -lgc 300,1100 failed (%s): %w", output, codeless{err})
	}
	for code, want := range map[int]string{
		4:   joulie.GovernorResultRefused,
		2:   joulie.GovernorResultUnsupported,
		3:   joulie.GovernorResultUnsupported,
		13:  joulie.GovernorResultUnsupported,
		9:   joulie.GovernorResultError,
		255: joulie.GovernorResultError,
	} {
		for name, err := range map[string]error{"binary": exitOf(t, code), "in-process": &fakesmi.ExitError{Code: code}} {
			if got, msg := Classify(wrap(err), "", ""); got != want {
				t.Errorf("%s exit %d: %q (message %q), want %q", name, code, got, msg, want)
			}
		}
	}
}
