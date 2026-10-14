package main

import (
	"context"
	"errors"
	"testing"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/simulator/pkg/hwemu/hwemutest"
)

// runCommand keeps a failed tool's exit status as an error value, for the
// real binary and for the simulator's in-process fake alike, so
// governor.Classify sorts it by the documented code rather than by the
// wording of the message.
func TestRunCommandKeepsTheToolExitCode(t *testing.T) {
	old := commandRunner
	t.Cleanup(func() { commandRunner = old })

	commandRunner = OSCommandRunner{}
	_, err := runCommand(context.Background(), "sh", "-c", "echo Terminating early due to previous errors.; exit 4")
	checkExit(t, "binary", err, 4, joulie.GovernorResultRefused)

	_, tree, _ := hwemutest.NewNode(t, "nvidia-h100-sxm-4gpu", nil)
	useEmulatedNode(t, tree, nil)
	// A known option without its value is an invalid argument, exit 2.
	_, err = runCommand(context.Background(), "nvidia-smi", "-pl")
	checkExit(t, "fake tool", err, 2, joulie.GovernorResultUnsupported)
}

func checkExit(t *testing.T, name string, err error, code int, result string) {
	t.Helper()
	var ec interface{ ExitCode() int }
	if !errors.As(err, &ec) || ec.ExitCode() != code {
		t.Fatalf("%s: %v (%T), want an error carrying exit code %d", name, err, err, code)
	}
	if got, msg := governor.Classify(err, "", ""); got != result {
		t.Errorf("%s: classified %q (%s), want %q", name, got, msg, result)
	}
}
