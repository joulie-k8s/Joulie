package simcluster_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSC28HwemuModes: SIMCLUSTER_HWEMU=fuse, the default, creates the k3s
// container with the FUSE device, makes the hwemu root a shared mount on
// every up.sh (a container's mounts are private again on each start), and
// installs the simulator and Joulie with the FUSE values files, which
// deploy.env records, so a reload installs with them again; files does
// none of that; any other value, and fuse without the device, fail before
// anything runs.
func TestSC28HwemuModes(t *testing.T) {
	const root = "/var/lib/joulie-hwemu"
	for _, tc := range []struct {
		hwemu, want string
	}{
		{"", "fuse"},
		{"fuse", "fuse"},
		{"files", "files"},
	} {
		e := scNew(t)
		vars := map[string]string{"SIMCLUSTER_HWEMU": tc.hwemu}
		fuse := tc.want == "fuse"
		e.mustRun("up.sh", vars)
		e.mustRun("up.sh", vars)
		runs := e.callsOf("docker", "run")
		if len(runs) != 1 || runs[0].has("--device", "/dev/null") != fuse {
			t.Errorf("SIMCLUSTER_HWEMU=%q: docker run %v, want the FUSE device: %t", tc.hwemu, runs, fuse)
		}
		shared := 0
		for _, c := range e.callsOf("docker", "exec") {
			if c.has("mount", "--make-rshared", root) {
				shared++
			}
		}
		if want := map[bool]int{true: 2, false: 0}[fuse]; shared != want {
			t.Errorf("SIMCLUSTER_HWEMU=%q: %d mount --make-rshared %s over two up.sh runs, want %d", tc.hwemu, shared, root, want)
		}

		e.mustRun("deploy.sh", vars)
		simValues := filepath.Join(e.repo, "values/simcluster-simulator-fuse.yaml")
		joulieValues := filepath.Join(e.repo, "values/simcluster-fuse.yaml")
		var installSim scCall
		for _, c := range e.callsOf("helm", "upgrade") {
			switch {
			case c.has("joulie-sim"):
				installSim = c
				if c.has(simValues) != fuse {
					t.Errorf("SIMCLUSTER_HWEMU=%q: the simulator installed with %v, want %s: %t", tc.hwemu, c.after("-f"), simValues, fuse)
				}
			case c.has("joulie"):
				if c.has(joulieValues) != fuse {
					t.Errorf("SIMCLUSTER_HWEMU=%q: Joulie installed with %v, want %s: %t", tc.hwemu, c.after("-f"), joulieValues, fuse)
				}
			}
		}
		if got := scEnvFile(t, filepath.Join(e.dir, "deploy.env"))["HWEMU"]; got != tc.want {
			t.Errorf("SIMCLUSTER_HWEMU=%q: deploy.env HWEMU=%s, want %s", tc.hwemu, got, tc.want)
		}
		// The reload runs without the variable and installs as the deploy did.
		e.resetCalls()
		e.mustRun("reload.sh", nil, "simulator")
		if up := e.callsOf("helm", "upgrade"); installSim == nil || len(up) != 1 || up[0].String() != installSim.String() {
			t.Errorf("SIMCLUSTER_HWEMU=%q: reload.sh simulator ran %v, want deploy.sh's command %v", tc.hwemu, up, installSim)
		}
	}

	out := scNew(t).mustFail("up.sh", map[string]string{"SIMCLUSTER_HWEMU": "tmpfs"})
	if !strings.Contains(out, "SIMCLUSTER_HWEMU=tmpfs is not fuse or files") {
		t.Errorf("an unknown SIMCLUSTER_HWEMU: want it refused:\n%s", out)
	}
	e := scNew(t)
	out = e.mustFail("up.sh", map[string]string{"SIM_FUSE_DEVICE": filepath.Join(e.state, "no-fuse")})
	if !strings.Contains(out, "SIMCLUSTER_HWEMU=fuse needs the FUSE device") || len(e.callsOf("docker", "run")) != 0 {
		t.Errorf("fuse mode without the device: want a refusal before docker run:\n%s", out)
	}
}
