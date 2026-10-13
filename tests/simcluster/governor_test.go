package simcluster_test

// Tests of experiment 05's Tier B inputs: the governor cluster spec and the
// script that runs an arm on it. Package-level names carry the prefix gv.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matbun/joulie/simulator/pkg/clusterspec"
)

// gvSpec is the governor cluster spec.
var gvSpec = filepath.Join(spSpecsDir, "governor.yaml")

// gvTierB is the Tier B script of experiment 05.
var gvTierB = filepath.Join("..", "..", "experiments", "05-governor-exploration", "scripts", "run_tier_b.sh")

// gvNameWords are the only words a pool name of the governor spec may use:
// vendors, product lines and models of the builtin profiles, and the
// drivers that tell CPU pools apart. An allowlist, because a denylist of
// real site, cluster or node names would itself put them in the
// repository.
var gvNameWords = []string{
	"amd", "intel", "nvidia", "epyc", "xeon", "xeon6", "9534", "6530", "hsmp", "rapl", "tpmi",
	"radeon", "pro", "w7900", "instinct", "mi300x", "h100", "nvl", "sxm", "l40s", "vm", "cpu", "gpu",
}

// gvProfiles are the profiles the spec must cover once each: one per CPU
// frequency driver and one per GPU family of the experiment (section 5.3).
var gvProfiles = []string{
	"amd-epyc-9534-2s-energy-only", // acpi-cpufreq
	"intel-xeon-6530-2s-rapl",      // intel_cpufreq
	"intel-xeon-2s-rapl",           // intel_pstate active, HWP
	"amd-epyc-2s-hsmp",             // amd-pstate-epp
	"amd-radeon-pro-w7900-4gpu",
	"amd-instinct-mi300x-8gpu",
	"nvidia-h100-nvl-8gpu",
	"nvidia-l40s-4gpu",
}

// TestGX05GovernorSpecIsValidWithGenericNames checks that governor.yaml
// loads and expands with the builtin profiles, covers each profile of
// gvProfiles once with at least two Nodes (one performance and one eco per
// family), and names every pool from gvNameWords only and every Node as its
// pool plus an index.
func TestGX05GovernorSpecIsValidWithGenericNames(t *testing.T) {
	s, err := clusterspec.LoadFile(gvSpec)
	if err != nil {
		t.Fatal(err)
	}
	members, err := s.Expand(clusterspec.Profiles(""))
	if err != nil {
		t.Fatalf("expand %s: %v", gvSpec, err)
	}
	word := strings.Join(gvNameWords, "|")
	poolRE := regexp.MustCompile(`^(` + word + `)(-(` + word + `))*$`)
	var profiles []string
	pools := map[string]bool{}
	for _, p := range s.Spec.Pools {
		pools[p.Name] = true
		profiles = append(profiles, p.Profile)
		if !poolRE.MatchString(p.Name) {
			t.Errorf("pool %q is not made of the allowed words %v", p.Name, gvNameWords)
		}
		if p.Count < 2 {
			t.Errorf("pool %s has %d Nodes; two keep one performance and one eco Node of its family", p.Name, p.Count)
		}
	}
	slices.Sort(profiles)
	want := slices.Clone(gvProfiles)
	slices.Sort(want)
	if !slices.Equal(profiles, want) {
		t.Errorf("the pools use profiles %v, want each of %v once", profiles, want)
	}
	nodeRE := regexp.MustCompile(`^(.+)-[0-9]+$`)
	for _, m := range members {
		sub := nodeRE.FindStringSubmatch(m.Name)
		if sub == nil || !pools[sub[1]] || sub[1] != m.Pool {
			t.Errorf("node %q is not its pool %q plus an index", m.Name, m.Pool)
		}
	}
	b, err := os.ReadFile(gvSpec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "measured:") {
		t.Errorf("%s holds a measured: source", gvSpec)
	}
}

// gvFakeDocker answers the two docker calls the context check makes, from
// FAKE_ID and FAKE_PORT, and the running check from FAKE_RUNNING; it logs
// every call to FAKE_LOG.
const gvFakeDocker = `#!/usr/bin/env bash
echo "docker $*" >>"$FAKE_LOG"
case $1 in
  inspect)
    [[ -n ${FAKE_ID:-} ]] || { echo "Error: No such object: $4" >&2; exit 1; }
    case $3 in
      *.Id*) echo "$FAKE_ID" ;;
      *.State.Running*) echo "${FAKE_RUNNING:-true}" ;;
    esac
    ;;
  port) [[ -n ${FAKE_PORT:-} ]] && echo "127.0.0.1:$FAKE_PORT" ;;
  *) exit 1 ;;
esac
`

// gvFakeTool logs a call and fails: no cluster tool may run.
const gvFakeTool = `#!/usr/bin/env bash
echo "$(basename "$0") $*" >>"$FAKE_LOG"
exit 1
`

// gvKubeconfig is a kubeconfig of one cluster at server.
func gvKubeconfig(server string, contexts int) string {
	s := "apiVersion: v1\nclusters:\n- cluster:\n    server: " + server + "\n  name: default\ncontexts:\n"
	for i := 0; i < contexts; i++ {
		s += "- context:\n    cluster: default\n    user: default\n  name: ctx" + string(rune('a'+i)) + "\n"
	}
	return s + "current-context: ctxa\nkind: Config\nusers:\n- name: default\n  user: {}\n"
}

// TestGX06TierBRefusesAnyOtherContext runs run_tier_b.sh against fake
// docker, kubectl, helm and go: its context check passes only for the
// kubeconfig of the container it recorded as created, whatever KUBECONFIG
// the caller has, and an arm never starts over a container that existed
// before it.
func TestGX06TierBRefusesAnyOtherContext(t *testing.T) {
	for _, tool := range []string{"bash", "awk", "grep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	root := t.TempDir()
	bin, dir, home := filepath.Join(root, "bin"), filepath.Join(root, "state"), filepath.Join(root, "home")
	for _, d := range []string{bin, dir, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"docker": gvFakeDocker, "kubectl": gvFakeTool, "helm": gvFakeTool, "go": gvFakeTool, "jq": gvFakeTool} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(root, "calls.log")
	userKube := filepath.Join(root, "user-kubeconfig")
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(userKube, gvKubeconfig("https://10.0.0.1:6443", 1))
	run := func(env map[string]string, args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", append([]string{gvTierB}, args...)...)
		cmd.Env = []string{
			"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + home, "LANG=C",
			"SIMCLUSTER_NAME=joulie-sim-gx06", "SIMCLUSTER_DIR=" + dir, "FAKE_LOG=" + log,
			// The caller's own cluster, which the script must never use.
			"KUBECONFIG=" + userKube,
		}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	marker := filepath.Join(dir, "tier-b.created")
	kube := filepath.Join(dir, "kubeconfig")
	good := map[string]string{"FAKE_ID": "c0ffee", "FAKE_PORT": "41234"}

	for _, c := range []struct {
		name     string
		marker   string
		kube     string
		env      map[string]string
		accepted bool
		why      string
	}{
		{"the recorded container and its port", "c0ffee", gvKubeconfig("https://127.0.0.1:41234", 1), good, true, ""},
		{"no record of a creation", "", gvKubeconfig("https://127.0.0.1:41234", 1), good, false, "did not create"},
		{"another container under the name", "c0ffee", gvKubeconfig("https://127.0.0.1:41234", 1), map[string]string{"FAKE_ID": "beef", "FAKE_PORT": "41234"}, false, "not the one this script created"},
		{"no container", "c0ffee", gvKubeconfig("https://127.0.0.1:41234", 1), map[string]string{}, false, "not the one this script created"},
		{"a remote server", "c0ffee", gvKubeconfig("https://10.0.0.1:6443", 1), good, false, "is not the container's published port"},
		{"another local port", "c0ffee", gvKubeconfig("https://127.0.0.1:6443", 1), good, false, "is not the container's published port"},
		{"two contexts", "c0ffee", gvKubeconfig("https://127.0.0.1:41234", 2), good, false, "not exactly one of each"},
		{"no kubeconfig", "c0ffee", "", good, false, "is missing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			os.Remove(marker)
			os.Remove(kube)
			os.Remove(log)
			if c.marker != "" {
				write(marker, c.marker+"\n")
			}
			if c.kube != "" {
				write(kube, c.kube)
			}
			out, err := run(c.env, "--check-context")
			if c.accepted {
				if err != nil {
					t.Fatalf("refused the harness cluster: %v\n%s", err, out)
				}
			} else {
				if err == nil {
					t.Fatalf("accepted it:\n%s", out)
				}
				if !strings.Contains(out, "refusing") || !strings.Contains(out, c.why) {
					t.Errorf("the refusal does not say %q:\n%s", c.why, out)
				}
			}
			calls, _ := os.ReadFile(log)
			for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if line != "" && !strings.HasPrefix(line, "docker inspect") && !strings.HasPrefix(line, "docker port") {
					t.Errorf("the check ran %q", line)
				}
			}
		})
	}

	t.Run("an arm over an existing container", func(t *testing.T) {
		os.Remove(log)
		out, err := run(map[string]string{"FAKE_ID": "c0ffee", "FAKE_RUNNING": "true", "XDG_CACHE_HOME": filepath.Join(root, "cache")}, "all")
		if err == nil || !strings.Contains(out, "exists already") {
			t.Fatalf("started over a container it did not create: %v\n%s", err, out)
		}
		calls, _ := os.ReadFile(log)
		if strings.Contains(string(calls), "docker run") || strings.Contains(string(calls), "kubectl") || strings.Contains(string(calls), "helm") {
			t.Errorf("it touched a cluster:\n%s", calls)
		}
	})
	t.Run("a name outside the guard", func(t *testing.T) {
		out, err := run(map[string]string{"SIMCLUSTER_NAME": "prod"}, "--check-context")
		if err == nil || !strings.Contains(out, "does not match") {
			t.Fatalf("accepted the name prod: %v\n%s", err, out)
		}
	})
}
