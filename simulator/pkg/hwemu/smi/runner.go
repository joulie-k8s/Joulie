package smi

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Runner runs the fake tools in process. Its Run method has the signature of
// the agent's CommandRunner interface (cmd/agent), so a test assigns a Runner
// to the agent's commandRunner and the agent's real GPU code drives the
// emulated node without a subprocess.
type Runner struct {
	Env Env
}

// Run behaves like OSCommandRunner.Run in cmd/agent: stdout and stderr are
// interleaved into one buffer, as CombinedOutput does, and stdin is at EOF,
// as exec gives a command without Stdin. A tool absent from tools.json fails
// the way exec fails when the binary is not on PATH, with an *exec.Error
// wrapping exec.ErrNotFound and no output. A non-zero exit returns the output
// and an *ExitError.
func (r Runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tool := filepath.Base(name)
	m, err := readManifest(r.Env)
	if err != nil {
		return nil, err
	}
	if _, ok := m.Tools[tool]; !ok {
		return nil, &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	var out bytes.Buffer
	code := Main(append([]string{tool}, args...), r.Env, strings.NewReader(""), &out, &out)
	if code != 0 {
		return out.Bytes(), &ExitError{Code: code}
	}
	return out.Bytes(), nil
}

// ExitError is a non-zero exit of a fake tool run by Runner. Its text is the
// text of the *exec.ExitError the fakesmi binary gives for the same exit, so
// the agent's error messages read the same either way.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return "exit status " + strconv.Itoa(e.Code)
}
