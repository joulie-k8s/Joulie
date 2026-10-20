//go:build simcluster

package simcluster_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matbun/joulie/simulator/pkg/scenario"
)

// e2eHwemuFUSE is SIMCLUSTER_HWEMU=fuse as deploy.env records it, the
// agents reading their Nodes through the simulator's FUSE views; any other
// value, or none, is the trees' plain files.
const e2eHwemuFUSE = "fuse"

// e2eDeadView is what a read or write through a view whose FUSE server is
// gone fails with (ENOTCONN), as the agent logs it.
const e2eDeadView = "transport endpoint is not connected"

// e2eAgentPod is an emulated agent pod as E17 compares it across the
// simulator's restart: its UID and the restarts of its containers.
type e2eAgentPod struct {
	UID      string
	Restarts int32
}

// e2eRestartProblems lists what E17 finds wrong, by pod name: an agent pod
// that is gone, replaced or new, one whose containers restarted, and every
// pod whose log, from one agent reconcile after the new simulator was
// ready, still reads a dead view. Sorted, so a failure reads the same each
// run.
func e2eRestartProblems(before, after map[string]e2eAgentPod, logs map[string]string) []string {
	var out []string
	for name, b := range before {
		a, ok := after[name]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("agent pod %s is gone after the simulator restart", name))
		case a.UID != b.UID:
			out = append(out, fmt.Sprintf("agent pod %s was replaced (UID %s, was %s)", name, a.UID, b.UID))
		case a.Restarts != b.Restarts:
			out = append(out, fmt.Sprintf("agent pod %s restarted %d times during the simulator restart", name, a.Restarts-b.Restarts))
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			out = append(out, fmt.Sprintf("agent pod %s is new after the simulator restart", name))
		}
	}
	for name, log := range logs {
		var dead []string
		for _, line := range strings.Split(log, "\n") {
			if strings.Contains(line, e2eDeadView) {
				dead = append(dead, line)
			}
		}
		if len(dead) > 0 {
			out = append(out, fmt.Sprintf("agent pod %s still reads a dead view %d times after the new simulator was ready, first: %s", name, len(dead), dead[0]))
		}
	}
	sort.Strings(out)
	return out
}

// agentPods returns the emulated agent pods by name.
func (e *e2eEnv) agentPods() (map[string]e2eAgentPod, error) {
	pods, err := e.pods(scenario.NamespaceJoulie, client.MatchingLabels{e2eLabelComponent: e2eComponentEmulated})
	if err != nil {
		return nil, err
	}
	out := map[string]e2eAgentPod{}
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		var restarts int32
		for _, s := range p.Status.ContainerStatuses {
			restarts += s.RestartCount
		}
		out[p.Name] = e2eAgentPod{UID: string(p.UID), Restarts: restarts}
	}
	return out, nil
}

// E17: in FUSE mode a restarted simulator's views reach the running
// agents. reload.sh simulator --force restarts the simulator; every
// emulated agent keeps its pod, with no container restart, and from one
// agent reconcile after the new simulator is ready, over two more, no
// agent reads a dead view; status.sh then exits 0. With plain files the
// agents mount the trees themselves and a restart cannot cut them off, so
// it skips.
func e2eE17SimulatorRestartKeepsTheAgents(c *e2eCase) {
	e := c.env
	if e.Hwemu != e2eHwemuFUSE {
		c.Skipf("SIMCLUSTER_HWEMU=%s: the agents mount the trees' plain files, which a simulator restart leaves in place", e.Hwemu)
	}
	before, err := e.agentPods()
	if err != nil {
		c.Fatalf("agent pods: %v", err)
	}
	if out, err := e.script("reload", map[string]string{"SIMCLUSTER_FORCE_RELOAD": ""}, "simulator", "--force"); err != nil {
		c.Fatalf("reload.sh simulator --force: %v\n%s", err, e2eTail(out, 30))
	}
	if err := e2eEventually(e.agentSettle(), func() error {
		_, err := e.proxyGet(scenario.NamespaceSimulator, scenario.SimulatorService, "readyz", nil)
		return err
	}); err != nil {
		c.Fatalf("the restarted simulator's /readyz: %v", err)
	}
	time.Sleep(e.Timing.AgentReconcile)
	from := metav1.NewTime(time.Now())
	time.Sleep(2 * e.Timing.AgentReconcile)

	after, err := e.agentPods()
	if err != nil {
		c.Fatalf("agent pods: %v", err)
	}
	logs := map[string]string{}
	for name := range after {
		ctx, cancel := e.ctx()
		b, err := e.Clientset.CoreV1().Pods(scenario.NamespaceJoulie).GetLogs(name, &corev1.PodLogOptions{SinceTime: &from}).DoRaw(ctx)
		cancel()
		if err != nil {
			c.Fatalf("logs of %s: %v", name, err)
		}
		logs[name] = string(b)
	}
	problems := e2eRestartProblems(before, after, logs)
	c.observe(float64(len(problems)), "== 0")
	for _, p := range problems {
		c.Errorf("%s", p)
	}

	var out string
	if err := e2eEventually(e.agentSettle(), func() error {
		var err error
		out, err = e.script("status", nil)
		return err
	}); err != nil {
		c.Errorf("status.sh after the simulator restart: %v\n%s", err, e2eTail(out, 40))
	}
}

// E17's verdict, without a cluster: pods kept are fine, and every pod that
// is gone, replaced, new, restarted or still reading a dead view is named.
func TestE2E17RestartProblems(t *testing.T) {
	before := map[string]e2eAgentPod{
		"kept":      {UID: "u1"},
		"gone":      {UID: "u2"},
		"replaced":  {UID: "u3"},
		"restarted": {UID: "u4", Restarts: 1},
	}
	after := map[string]e2eAgentPod{
		"kept":      {UID: "u1"},
		"replaced":  {UID: "u3b"},
		"restarted": {UID: "u4", Restarts: 3},
		"new":       {UID: "u5"},
	}
	logs := map[string]string{
		"kept":      "desired state source=nodetwin\n",
		"replaced":  "reconcile failed node=n: open /host-sys/x: transport endpoint is not connected\nok\n",
		"restarted": "",
	}
	got := e2eRestartProblems(before, after, logs)
	want := []string{
		"agent pod gone is gone after the simulator restart",
		"agent pod new is new after the simulator restart",
		"agent pod replaced still reads a dead view 1 times after the new simulator was ready, first: reconcile failed node=n: open /host-sys/x: transport endpoint is not connected",
		"agent pod replaced was replaced (UID u3b, was u3)",
		"agent pod restarted restarted 2 times during the simulator restart",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if p := e2eRestartProblems(map[string]e2eAgentPod{"a": {UID: "u"}}, map[string]e2eAgentPod{"a": {UID: "u"}}, map[string]string{"a": "fine\n"}); len(p) != 0 {
		t.Errorf("a kept agent with a clean log: %v, want no problem", p)
	}
}
