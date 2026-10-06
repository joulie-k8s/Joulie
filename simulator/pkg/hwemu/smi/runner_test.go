package smi

import (
	"errors"
	"os/exec"
	"testing"
)

// A tool the node does not have fails through Runner exactly as exec fails
// for a binary that is not on PATH, so the agent's vendor probe
// (detectGPUVendor in cmd/agent) falls through to the next tool as on a real
// node. The fakesmi binary exits 127 instead when it runs as a tool that
// tools.json does not list; Render links only the listed tools in bin/.
func TestS10ToolMissingFromManifestIsExecNotFound(t *testing.T) {
	t.Parallel()
	n := newNVLNode(t)
	out, err := n.run(agentRocmProduct...)
	var execErr *exec.Error
	if !errors.As(err, &execErr) || !errors.Is(err, exec.ErrNotFound) || execErr.Name != "rocm-smi" || out != nil {
		t.Fatalf("rocm-smi on an NVIDIA node: %v (%T), output %q; want *exec.Error wrapping ErrNotFound and no output", err, err, out)
	}
	if want := `exec: "rocm-smi": executable file not found in $PATH`; err.Error() != want {
		t.Fatalf("error text %q, want %q", err.Error(), want)
	}
	if _, err := n.run(agentNvidiaList...); err != nil {
		t.Fatalf("nvidia-smi, which tools.json lists: %v", err)
	}

	code, stdout, stderr := n.main("", agentRocmProduct...)
	if code != 127 || stdout != "" || stderr != "fakesmi: rocm-smi is not installed on this emulated node\n" {
		t.Fatalf("Main: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
