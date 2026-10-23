//go:build simcluster

package simcluster_test

// The end-to-end test's harness: the cluster under test (e2eEnv), how each
// check is run and recorded into the run's summary.json and OTLP files, and
// the reads the checks share. The package-level names of this file, of
// e2e_test.go and of known_defects_test.go carry the prefix e2e.
//
// Every cluster read goes through the API server: Kubernetes objects
// through a controller-runtime client with Joulie's typed CRDs, and HTTP to
// Prometheus, the simulator and the scheduler extender through the
// service proxy (/api/v1/namespaces/<ns>/services/<svc>:<port>/proxy/...):
// the cluster publishes no port but the API server's.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/clusterspec"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/simcluster"
)

// Settings smoke.sh passes to the test. SIMCLUSTER_DIR is the cluster's
// state directory (lib.sh), SIMCLUSTER_RUN_DIR the run directory the test
// writes summary.json and its OTLP files into, and SIMCLUSTER_PROFILES_DIR
// the local profiles the host tools read.
const (
	e2eEnvDir         = "SIMCLUSTER_DIR"
	e2eEnvRunDir      = "SIMCLUSTER_RUN_DIR"
	e2eEnvProfilesDir = "SIMCLUSTER_PROFILES_DIR"
)

// Names the scripts and the charts give the cluster's objects
// (scripts/simcluster/lib.sh, charts/joulie/templates/agent-emulated.yaml).
const (
	e2eReleaseJoulie     = "joulie"
	e2eLabelInfra        = "joulie.io/infra"
	e2eLabelManaged      = "joulie.io/managed"
	e2eLabelComponent    = "app.kubernetes.io/component"
	e2eComponentEmulated = "emulated"
	e2eAnnotationNode    = "sim.joulie.io/node"
	e2eExtenderMetrics   = "joulie-scheduler-extender:9877"
	e2eDeployController  = "joulie-controller-manager"
	e2eDeployExtender    = "joulie-scheduler-extender"
)

// The smoke policy (tests/simcluster/smoke-values.yaml): CPU and GPU
// performance caps at 80 % of the maximum, lowered to 50 % in one upgrade.
const (
	e2eCapPctBefore = 80.0
	e2eCapPctAfter  = 50.0
)

// e2eAssertionType is the assertion type of the smoke test's checks in
// summary.json; the scenario runner's types are scenario.AssertionTypes.
const e2eAssertionType = scenario.AssertionType("e2e")

// e2ePollInterval is how often a check polls the cluster while it waits;
// e2eCallTimeout bounds one API call.
const (
	e2ePollInterval = 2 * time.Second
	e2eCallTimeout  = 30 * time.Second
)

// e2eMessageMax bounds the message of one check in summary.json.
const e2eMessageMax = 4000

// e2eEnv is the simulated cluster under test, as deploy.sh left it.
type e2eEnv struct {
	// Repo is the repository root, Dir the cluster's state directory and
	// RunDir the run directory ("" when the test runs outside smoke.sh).
	Repo, Dir, RunDir string
	// SpecPath is the cluster spec up.sh recorded, Spec its content and
	// Members its Nodes (clusterspec.Expand), sorted by name.
	SpecPath string
	Spec     *clusterspec.Spec
	Members  []clusterspec.Member
	Harness  simcluster.Harness
	// Install is the tool delivery of the Joulie install deploy.sh ran
	// (simcluster.Placement.ToolDelivery of its agent DaemonSet render),
	// the delivery of every pool the spec leaves to it; Extender is how
	// kube-scheduler reaches the extender (SIMCLUSTER_EXTENDER); Hwemu how
	// the agents reach their Nodes' files (SIMCLUSTER_HWEMU, files when
	// deploy.env records none).
	Install, Extender, Hwemu string
	// Mode, Scope and Timing are the settings deploy.sh installed with
	// (deploy.env); Parts are their telemetry parts, with Wait.Nodes set.
	Mode   simcluster.Mode
	Scope  simcluster.Scope
	Timing simcluster.Timing
	Parts  simcluster.TelemetryParts
	// Images is images.env.
	Images map[string]string
	// Prom queries the harness Prometheus through the service proxy.
	Prom *scenario.PromClient

	Config    *rest.Config
	Client    client.Client
	Clientset kubernetes.Interface

	start   time.Time
	summary scenario.Summary
}

// e2eNewEnv reads the cluster smoke.sh deployed. It skips the test when
// SIMCLUSTER_DIR is not set: the test needs a cluster, which make
// simcluster-smoke creates. The summary is written by finish, which the
// caller defers right after, so a failure here still leaves a summary.
func e2eNewEnv(t *testing.T) *e2eEnv {
	t.Helper()
	dir := os.Getenv(e2eEnvDir)
	if dir == "" {
		t.Skipf("%s is not set: this test runs against a simulated cluster, through make simcluster-smoke", e2eEnvDir)
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return &e2eEnv{Repo: repo, Dir: dir, RunDir: os.Getenv(e2eEnvRunDir), start: time.Now()}
}

// load reads the spec, the settings and the images of the cluster and
// connects to it.
func (e *e2eEnv) load(t *testing.T) {
	t.Helper()
	deploy, err := simcluster.ReadEnvFile(filepath.Join(e.Dir, "deploy.env"))
	if err != nil {
		t.Fatalf("%v; run scripts/simcluster/deploy.sh first", err)
	}
	if e.Mode, err = simcluster.ParseMode(deploy["TELEMETRY"]); err != nil {
		t.Fatalf("deploy.env: %v", err)
	}
	if e.Scope, err = simcluster.ParseScope(deploy["POWER_SCOPE"]); err != nil {
		t.Fatalf("deploy.env: %v", err)
	}
	if e.Timing, err = simcluster.ReadTiming(e.Repo, deploy["TIMING"]); err != nil {
		t.Fatalf("deploy.env: %v", err)
	}
	e.summary.TelemetryMode, e.summary.PowerScope, e.summary.Timing = string(e.Mode), string(e.Scope), e.Timing.Name
	e.summary.InputPreset = string(e.Mode.InputPreset())
	if e.Extender = deploy["EXTENDER"]; e.Extender == "" {
		e.Extender = e2eExtenderHostNet
	}
	if e.Hwemu = deploy["HWEMU"]; e.Hwemu == "" {
		e.Hwemu = "files"
	}

	e.SpecPath = filepath.Join(e.Dir, "spec.yaml")
	if e.Spec, err = clusterspec.LoadFile(e.SpecPath); err != nil {
		t.Fatalf("the spec up.sh recorded: %v", err)
	}
	e.summary.SuiteName = scenario.SuiteName(e.Spec.Metadata.Name, string(e.Mode))
	if spec := os.Getenv("SIMCLUSTER_SPEC"); spec != "" && os.Getenv("SIMCLUSTER_NODES") == "" {
		e.summary.Spec = spec
	} else {
		e.summary.Spec = e.Spec.Metadata.Name
	}
	if e.Members, err = e.Spec.Expand(clusterspec.Profiles(os.Getenv(e2eEnvProfilesDir))); err != nil {
		t.Fatalf("expand %s: %v", e.SpecPath, err)
	}
	if e.Harness, err = simcluster.SpecHarness(e.Spec); err != nil {
		t.Fatalf("%s: %v", e.SpecPath, err)
	}
	render, err := os.ReadFile(filepath.Join(e.Dir, "render", "agent-daemonset.yaml"))
	if err != nil {
		t.Fatalf("the agent DaemonSet render deploy.sh placed the agents with: %v", err)
	}
	placement, err := simcluster.PlacementFromRender(render, e2eImageArch)
	if err != nil {
		t.Fatalf("render/agent-daemonset.yaml: %v", err)
	}
	e.Install = placement.ToolDelivery()
	if e.Parts, err = simcluster.Telemetry(e.Mode, e.Scope, e.Timing); err != nil {
		t.Fatalf("telemetry parts: %v", err)
	}
	e.Parts.Wait.Nodes = simcluster.WaitNodes(e.Members)

	if e.Images, err = simcluster.ReadEnvFile(filepath.Join(e.Dir, "images.env")); err != nil {
		t.Fatal(err)
	}
	e.summary.Images = map[string]string{}
	for k, v := range e.Images {
		if strings.HasPrefix(k, "IMAGE_") && !strings.HasPrefix(k, "IMAGE_IDS_") {
			e.summary.Images[k] = v
		}
	}

	if e.Config, err = clientcmd.BuildConfigFromFlags("", filepath.Join(e.Dir, "kubeconfig")); err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	e.Config.QPS, e.Config.Burst = 50, 100
	scheme, err := kube.NewScheme(v1alpha1.AddToScheme, appsv1.AddToScheme)
	if err != nil {
		t.Fatal(err)
	}
	if e.Client, err = client.New(e.Config, client.Options{Scheme: scheme}); err != nil {
		t.Fatal(err)
	}
	if e.Clientset, err = kubernetes.NewForConfig(e.Config); err != nil {
		t.Fatal(err)
	}
	if e.Prom, err = scenario.NewProxyPromClient(e.Config); err != nil {
		t.Fatal(err)
	}
	t.Logf("cluster %s: spec %s (%d Nodes), telemetry %s, scope %s, timing %s", filepath.Base(e.Dir), e.Spec.Metadata.Name,
		len(e.Members), e.Mode, e.Scope, e.Timing.Name)
}

// The values of SIMCLUSTER_EXTENDER (scripts/simcluster/lib.sh), as
// deploy.env records them; hostnet when it records none.
const (
	e2eExtenderHostNet = "hostnet"
	e2eExtenderService = "service"
)

// e2eImageArch is the architecture the scripts build the images for, and
// e2eHwemuRoot where the hwemu trees live on the infra Node
// (SIM_IMAGE_ARCH and SIM_HWEMU_ROOT in scripts/simcluster/lib.sh).
const (
	e2eImageArch = "amd64"
	e2eHwemuRoot = "/var/lib/joulie-hwemu"
)

// e2eCase is one check: a subtest whose failures are also kept for
// summary.json. Errorf and Fatalf record the message; a check that knows
// it reproduces a registered defect calls knownDefect.
type e2eCase struct {
	*testing.T
	env      *e2eEnv
	name     string
	msgs     []string
	observed *float64
	expected string
	defects  []scenario.KnownDefect
}

func (c *e2eCase) Errorf(format string, args ...any) {
	c.Helper()
	msg := fmt.Sprintf(format, args...)
	c.msgs = append(c.msgs, msg)
	c.T.Error(msg)
}

func (c *e2eCase) Fatalf(format string, args ...any) {
	c.Helper()
	msg := fmt.Sprintf(format, args...)
	c.msgs = append(c.msgs, msg)
	c.T.Fatal(msg)
}

// observe records the check's observed value and its expected comparator
// (such as ">= 0.8"), which summary.json and the harness metrics carry.
func (c *e2eCase) observe(v float64, expected string) {
	c.observed, c.expected = &v, expected
}

// check runs fn as the subtest name and records its result, with claim as
// its expectation unless the check observes a number against a comparator.
// A skipped check is logged and left out of the summary, and so is one the
// -run filter leaves out (go test -run 'TestSimclusterE2E/E11' against a
// kept cluster): t.Run then returns without running fn, so c.T stays nil.
func (e *e2eEnv) check(t *testing.T, name, claim string, fn func(c *e2eCase)) bool {
	t.Helper()
	c := &e2eCase{env: e, name: name, expected: claim}
	ok := t.Run(name, func(t *testing.T) {
		c.T = t
		fn(c)
	})
	if c.T == nil || c.T.Skipped() {
		return ok
	}
	status := semconv.TestCaseResultStatusPass
	if !ok || len(c.msgs) > 0 {
		status = semconv.TestCaseResultStatusFail
	}
	msg := strings.Join(c.msgs, "; ")
	if len(msg) > e2eMessageMax {
		msg = msg[:e2eMessageMax] + " ..."
	}
	if status == semconv.TestCaseResultStatusPass && len(c.defects) > 0 {
		ids := make([]string, 0, len(c.defects))
		for _, d := range c.defects {
			ids = append(ids, d.ID)
		}
		msg = "known defects reproduce: " + strings.Join(ids, ", ")
	}
	e.summary.AddAssertions(scenario.AssertionResult{Name: name, Status: status, Type: e2eAssertionType,
		Severity: scenario.SeverityFail, Observed: c.observed, Expected: c.expected, Message: msg})
	e.summary.KnownDefects = append(e.summary.KnownDefects, c.defects...)
	return ok
}

// warn adds a warning line to the summary.
func (e *e2eEnv) warn(t *testing.T, format string, args ...any) {
	t.Helper()
	line := fmt.Sprintf(format, args...)
	t.Log("warning: " + line)
	e.summary.Warnings = append(e.summary.Warnings, line)
}

// finish writes summary.json, and the joulie.scenario.assertion.evaluated
// records and joulie.scenario.assertion.value points of the checks into
// the run directory's otlp/ files, as a scenario run does. A test that failed outside its
// checks, such as while reading the cluster, is a run error: exit code
// ExitError, status aborted.
func (e *e2eEnv) finish(t *testing.T) {
	t.Helper()
	if e.RunDir == "" {
		t.Logf("%s is not set: no summary.json written", e2eEnvRunDir)
		return
	}
	s := e.summary
	s.Schema, s.SemconvSchemaURL = scenario.NewSummary("").Schema, scenario.NewSummary("").SemconvSchemaURL
	if s.SuiteName == "" {
		s.SuiteName = filepath.Base(e.Dir)
	}
	s.Git = scenario.ReadGit(e.Repo)
	s.DurationsSeconds = map[string]float64{"test": math.Round(time.Since(e.start).Seconds()*10) / 10}
	var runErr error
	if t.Failed() && !e2eAnyFailed(s.Assertions) {
		runErr = errors.New("the test failed outside its checks; see smoke.log")
	}
	s.Finish(runErr)
	if err := os.MkdirAll(e.RunDir, 0o755); err != nil {
		t.Errorf("run directory: %v", err)
		return
	}
	path := filepath.Join(e.RunDir, scenario.SummaryFile)
	if err := scenario.WriteSummary(path, s); err != nil {
		t.Errorf("write %s: %v", path, err)
		return
	}
	h := scenario.Recorder{Dir: e.RunDir}
	if e.Spec != nil {
		h.ClusterName = e.Spec.Metadata.Name
	}
	recs, metric := scenario.ResultRecords(s.Assertions, time.Now(), nil)
	if err := h.WriteLogs(recs...); err != nil {
		t.Errorf("harness logs: %v", err)
	}
	if metric != nil {
		if err := h.WriteMetrics(*metric); err != nil {
			t.Errorf("harness metrics: %v", err)
		}
	}
	t.Logf("summary: %s, %s (exit %d), %d known defects", path, s.Status, s.ExitCode, len(s.KnownDefects))
}

func e2eAnyFailed(rs []scenario.AssertionResult) bool {
	for _, r := range rs {
		if r.Status != semconv.TestCaseResultStatusPass {
			return true
		}
	}
	return false
}

// ctx returns a context for one API call.
func (e *e2eEnv) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), e2eCallTimeout)
}

// proxyGet GETs path of a Service through the API server's service proxy.
func (e *e2eEnv) proxyGet(namespace, service, path string, params map[string]string) ([]byte, error) {
	ctx, cancel := e.ctx()
	defer cancel()
	req := e.Clientset.CoreV1().RESTClient().Get().
		AbsPath("/api/v1/namespaces", namespace, "services", service, "proxy", path)
	for k, v := range params {
		req = req.Param(k, v)
	}
	b, err := req.DoRaw(ctx)
	if err != nil {
		return b, fmt.Errorf("GET %s/%s %s: %w (%s)", namespace, service, path, err, bytes.TrimSpace(b))
	}
	return b, nil
}

// prom runs an instant query in the harness Prometheus at time at.
func (e *e2eEnv) prom(query string, at time.Time) (scenario.PromResult, error) {
	ctx, cancel := e.ctx()
	defer cancel()
	r, err := e.Prom.Query(ctx, query, at)
	if err != nil {
		return r, fmt.Errorf("query %s: %w", query, err)
	}
	return r, nil
}

// promByLabel runs query at time at and returns each series' value by its
// label label (e2eByLabel).
func (e *e2eEnv) promByLabel(query, label string, at time.Time) (map[string]float64, error) {
	r, err := e.prom(query, at)
	if err != nil {
		return nil, err
	}
	return e2eByLabel(r, query, label)
}

// e2eByLabel returns each series' last value by its label label. A series
// without the label or a value, or two series with one value of it, is an
// error: a query keyed by a Node returns one series per Node, and keeping
// either of two would hide the other.
func e2eByLabel(r scenario.PromResult, query, label string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, s := range r.Series {
		v := s.Labels[label]
		if v == "" || len(s.Samples) == 0 {
			return nil, fmt.Errorf("query %s: a series without %s or a value: %v", query, label, s.Labels)
		}
		if _, dup := out[v]; dup {
			return nil, fmt.Errorf("query %s: two series with %s=%s", query, label, v)
		}
		out[v] = s.Samples[len(s.Samples)-1].Value
	}
	return out, nil
}

// snapshot is the simulator's truth, GET /debug/emulate.
func (e *e2eEnv) snapshot() (*emulate.Snapshot, error) {
	b, err := e.proxyGet(scenario.NamespaceSimulator, scenario.SimulatorService, "debug/emulate", nil)
	if err != nil {
		return nil, err
	}
	var s emulate.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode /debug/emulate: %w", err)
	}
	return &s, nil
}

// e2eSnapshotNode returns the reading of node in s.
func e2eSnapshotNode(s *emulate.Snapshot, node string) (emulate.NodeSnapshot, bool) {
	for _, n := range s.Nodes {
		if n.Name == node {
			return n, true
		}
	}
	return emulate.NodeSnapshot{}, false
}

// e2eComponents returns the components of type typ (hw.type: cpu, gpu).
func e2eComponents(n emulate.NodeSnapshot, typ string) []emulate.ComponentReading {
	var out []emulate.ComponentReading
	for _, c := range n.Components {
		if c.Type == typ {
			out = append(out, c)
		}
	}
	return out
}

// joulieTruth is the power Joulie's measured node power must equal for each
// node in s, under the mode's truth (CheckSet.JoulieTruth). ok is false
// when the mode leaves Joulie without data.
func (e *e2eEnv) joulieTruth(s *emulate.Snapshot) (map[string]float64, bool, error) {
	truth := e.Parts.Checks.JoulieTruth
	if truth == "" {
		return nil, false, nil
	}
	truths, err := e.withKepler07Excess(simcluster.NodeTruths(s), s.Time)
	if err != nil {
		return nil, false, err
	}
	out := map[string]float64{}
	for name, nt := range truths {
		v, err := simcluster.EvalTruth(truth, nt)
		if err != nil {
			return nil, false, err
		}
		out[name] = v
	}
	return out, true, nil
}

// withKepler07Excess sets, in mode kepler-legacy, the idle power the
// emulated Kepler 0.7 reports beyond the measured one on each Node's truth,
// over the rate window ending at at (CheckSet.Kepler07Excess). Other modes
// get truth back as it is.
func (e *e2eEnv) withKepler07Excess(truth map[string]simcluster.NodeTruth, at time.Time) (map[string]simcluster.NodeTruth, error) {
	q := e.Parts.Checks.Kepler07Excess
	if q == nil {
		return truth, nil
	}
	components, err := e.promByLabel(q.Components, e.Parts.Checks.NodeLabel, at)
	if err != nil {
		return nil, fmt.Errorf("Kepler 0.7 idle excess: %w", err)
	}
	platform, err := e.promByLabel(q.Platform, e.Parts.Checks.NodeLabel, at)
	if err != nil {
		return nil, fmt.Errorf("Kepler 0.7 platform idle excess: %w", err)
	}
	return simcluster.WithKepler07Excess(truth, components, platform), nil
}

// twins returns the NodeTwins by spec.nodeName.
func (e *e2eEnv) twins() (map[string]v1alpha1.NodeTwin, error) {
	ctx, cancel := e.ctx()
	defer cancel()
	var l v1alpha1.NodeTwinList
	if err := e.Client.List(ctx, &l); err != nil {
		return nil, err
	}
	out := map[string]v1alpha1.NodeTwin{}
	for _, tw := range l.Items {
		out[tw.Spec.NodeName] = tw
	}
	return out, nil
}

// hardware returns the NodeHardware objects by spec.nodeName; names are
// never derived here (pkg/api.ObjectNameForNode owns them).
func (e *e2eEnv) hardware() (map[string]v1alpha1.NodeHardware, error) {
	ctx, cancel := e.ctx()
	defer cancel()
	var l v1alpha1.NodeHardwareList
	if err := e.Client.List(ctx, &l); err != nil {
		return nil, err
	}
	out := map[string]v1alpha1.NodeHardware{}
	for _, h := range l.Items {
		out[h.Spec.NodeName] = h
	}
	return out, nil
}

// pods lists the pods of namespace ("" for every namespace).
func (e *e2eEnv) pods(namespace string, opts ...client.ListOption) ([]corev1.Pod, error) {
	ctx, cancel := e.ctx()
	defer cancel()
	var l corev1.PodList
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := e.Client.List(ctx, &l, opts...); err != nil {
		return nil, err
	}
	return l.Items, nil
}

// agentDeployments returns the emulated agent Deployments by the Node their
// sim.joulie.io/node annotation names.
func (e *e2eEnv) agentDeployments() (map[string]appsv1.Deployment, error) {
	ctx, cancel := e.ctx()
	defer cancel()
	var l appsv1.DeploymentList
	if err := e.Client.List(ctx, &l, client.InNamespace(scenario.NamespaceJoulie),
		client.MatchingLabels{e2eLabelComponent: e2eComponentEmulated}); err != nil {
		return nil, err
	}
	out := map[string]appsv1.Deployment{}
	for _, d := range l.Items {
		node := d.Annotations[e2eAnnotationNode]
		if node == "" {
			return nil, fmt.Errorf("deployment %s has no %s annotation", d.Name, e2eAnnotationNode)
		}
		if _, dup := out[node]; dup {
			return nil, fmt.Errorf("two emulated agent Deployments for node %s", node)
		}
		out[node] = d
	}
	return out, nil
}

// e2eRunning reports whether a pod is Running and not being deleted.
func e2eRunning(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil
}

// e2eNode is one spec Node with what the run gave it.
type e2eNode struct {
	clusterspec.Member
	// Agent: an emulated agent stands in for the Node. Managed: the Node
	// carries joulie.io/managed=true. ToolDelivery is its pool's: the
	// spec's harness setting, or the install's.
	Agent, Managed bool
	ToolDelivery   string
}

// Families of spec Nodes, by what the run must show on them.
const (
	e2eFamilyRAPL      = "rapl"       // CPU-only, RAPL package caps (E09)
	e2eFamilyDVFS      = "dvfs"       // CPU-only, caps through DVFS (E10)
	e2eFamilyNoCPUCap  = "no-cpu-cap" // CPU-only, no power control (E11)
	e2eFamilyNVIDIA    = "nvidia"     // NVIDIA GPUs, whole (E10, E11)
	e2eFamilyAMDGPU    = "amd-gpu"    // AMD GPUs, capped through sysfs with or without tools (E10, E11)
	e2eFamilyIntelGPU  = "intel-gpu"  // Intel GPUs, capped through sysfs (E10, E11)
	e2eFamilyGPUShared = "gpu-shared" // NVIDIA GPUs, MIG or renamed time-slicing (MD01, E10, E11)
	e2eFamilyNoNFD     = "no-nfd"     // NFD absent (MD02)
	e2eFamilyNoTools   = "no-tools"   // NVIDIA GPUs, nvidia-smi not delivered (MD03)
	e2eFamilyUnmanaged = "unmanaged"  // joulie.io/managed is not true (MD04)
	e2eFamilyUnreached = "unreached"  // managed, no agent (E12)
	e2eFamilyOther     = "other"      // a profile the checks have no expectation for
)

// e2eCPUFamily is the CPU control path of the CPU-only profiles the checks
// know: the agent caps intel-xeon-2s-rapl through RAPL, the two EPYC
// profiles through DVFS (their RAPL only counts energy), and has nothing to
// control on the VM.
var e2eCPUFamily = map[string]string{
	"intel-xeon-2s-rapl":      e2eFamilyRAPL,
	"amd-epyc-2s-energy-only": e2eFamilyDVFS,
	"amd-epyc-2s-hsmp":        e2eFamilyDVFS,
	"vm-no-powercap":          e2eFamilyNoCPUCap,
}

// e2eSharedExposures are the GPU exposures that advertise no <vendor>/gpu
// resource: MIG devices of the mixed strategy and renamed replicas. MD01
// checks that they are planned and capped as GPU Nodes all the same.
var e2eSharedExposures = map[string]bool{"mig-mixed": true, "timeslice-rename": true}

// family is the family of n.
func (n e2eNode) family() string {
	p := n.Profile
	switch {
	case !n.Managed:
		return e2eFamilyUnmanaged
	case !n.Agent:
		return e2eFamilyUnreached
	case p.GPUs != nil && e2eSharedExposures[p.GPUs.Exposure.V]:
		return e2eFamilyGPUShared
	case p.GPUs != nil && p.GPUs.Vendor == "nvidia" && n.ToolDelivery == simcluster.ToolDeliveryNone:
		return e2eFamilyNoTools
	case p.GPUs != nil && p.GPUs.Vendor == "nvidia":
		return e2eFamilyNVIDIA
	case p.GPUs != nil && p.GPUs.Vendor == "amd":
		return e2eFamilyAMDGPU
	case p.GPUs != nil && p.GPUs.Vendor == "intel":
		return e2eFamilyIntelGPU
	case p.GPUs != nil:
		return e2eFamilyOther
	case p.Node.NFD == "absent":
		return e2eFamilyNoNFD
	}
	if f, ok := e2eCPUFamily[p.Name]; ok {
		return f
	}
	return e2eFamilyOther
}

// nodes returns the spec Nodes with their agent, managed label and tool
// delivery, sorted by name.
func (e *e2eEnv) nodes() ([]e2eNode, error) {
	agents, err := e.agentDeployments()
	if err != nil {
		return nil, err
	}
	out := make([]e2eNode, 0, len(e.Members))
	for _, m := range e.Members {
		_, agent := agents[m.Name]
		out = append(out, e2eNode{Member: m, Agent: agent, Managed: m.Node.Labels[e2eLabelManaged] == "true",
			ToolDelivery: e.Harness.ToolDelivery(m.Pool, e.Install)})
	}
	return out, nil
}

// nodesOf returns the spec Nodes of the families given.
func (e *e2eEnv) nodesOf(c *e2eCase, families ...string) []e2eNode {
	c.Helper()
	all, err := e.nodes()
	if err != nil {
		c.Fatalf("spec Nodes: %v", err)
	}
	want := map[string]bool{}
	for _, f := range families {
		want[f] = true
	}
	var out []e2eNode
	for _, n := range all {
		if want[n.family()] {
			out = append(out, n)
		}
	}
	return out
}

// e2eDefaultPackageLimitW is the long-term limit of the first package zone
// of p in watts, the limit the node starts with; 0 when it has none.
func e2eDefaultPackageLimitW(p *hwemu.Profile) float64 {
	if p.Powercap == nil {
		return 0
	}
	for _, ct := range p.Powercap.ControlTypes {
		for _, z := range ct.Zones {
			if !strings.HasPrefix(z.NameFormat, "package-") {
				continue
			}
			for _, cs := range z.Constraints {
				if cs.Name == "long_term" {
					return float64(cs.PowerLimitUW.V) / 1e6
				}
			}
		}
	}
	return 0
}

// e2eDefaultGPULimitW is the GPU power limit p starts with: the current
// limit when the profile sets one, else the default.
func e2eDefaultGPULimitW(p *hwemu.Profile) float64 {
	if p.GPUs == nil {
		return 0
	}
	if p.GPUs.Limits.CurrentW.Src != "" && p.GPUs.Limits.CurrentW.V > 0 {
		return p.GPUs.Limits.CurrentW.V
	}
	return p.GPUs.Limits.DefaultW.V
}

// e2eNear reports whether got is want within tol watts.
func e2eNear(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// e2eLimitTolW is how far an enforced limit may be from the planned one:
// the powercap power unit and float rounding, far below one watt.
const e2eLimitTolW = 0.5

// e2eEventually calls fn every e2ePollInterval until it returns nil, for at
// most within, and returns its last error.
func e2eEventually(within time.Duration, fn func() error) error {
	deadline := time.Now().Add(within)
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still after %s: %w", within.Round(time.Second), err)
		}
		time.Sleep(e2ePollInterval)
	}
}

// Deadlines derived from the timing.
//
// agentSettle: a NodeTwin intent reaches the hardware. The controller writes
// the intent within a reconcile, the agent applies it within its own, and a
// restarted pod needs about a minute to be scheduled and read its first
// state.
func (e *e2eEnv) agentSettle() time.Duration {
	return 2*e.Timing.ControllerReconcile + 3*e.Timing.AgentReconcile + time.Minute
}

// twinFollow: a change of power reaches the NodeTwin. rate() over the
// window needs the window to pass, and the controller to reconcile.
func (e *e2eEnv) twinFollow() time.Duration {
	return 3*e.Timing.RateWindow + 2*e.Timing.ControllerReconcile
}

// waitSeriesAge waits until the simulator has been pushing for a rate
// window plus two export intervals and a scrape interval. On a younger
// series rate() reads low: Prometheus divides the increase it saw by the
// whole window. The series cannot be older than the Running simulator pod.
func (e *e2eEnv) waitSeriesAge(t *testing.T) error {
	t.Helper()
	pods, err := e.pods(scenario.NamespaceSimulator)
	if err != nil {
		return err
	}
	var start time.Time
	for i := range pods {
		if p := &pods[i]; e2eRunning(p) && p.Status.StartTime != nil && (start.IsZero() || p.Status.StartTime.Time.Before(start)) {
			start = p.Status.StartTime.Time
		}
	}
	if start.IsZero() {
		return fmt.Errorf("no Running pod in %s", scenario.NamespaceSimulator)
	}
	ready := start.Add(e.Timing.RateWindow + 2*e.Timing.OTLPExportInterval + e.Timing.ScrapeInterval)
	if wait := time.Until(ready); wait > 0 {
		t.Logf("waiting %s for the series to be older than the rate window %s", wait.Round(time.Second), e.Timing.RateWindow)
		time.Sleep(wait)
	}
	return nil
}

// rollout waits until Deployment name in namespace has rolled out its
// latest generation.
func (e *e2eEnv) rollout(namespace, name string, within time.Duration) error {
	return e2eEventually(within, func() error {
		ctx, cancel := e.ctx()
		defer cancel()
		var d appsv1.Deployment
		if err := e.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &d); err != nil {
			return err
		}
		want := int32(1)
		if d.Spec.Replicas != nil {
			want = *d.Spec.Replicas
		}
		st := d.Status
		if st.ObservedGeneration < d.Generation || st.UpdatedReplicas != want || st.Replicas != want || st.AvailableReplicas != want {
			return fmt.Errorf("deployment %s/%s: generation %d observed %d, replicas %d updated %d available %d, want %d",
				namespace, name, d.Generation, st.ObservedGeneration, st.Replicas, st.UpdatedReplicas, st.AvailableReplicas, want)
		}
		return nil
	})
}

// script runs scripts/simcluster/<name>.sh with args against this cluster
// and returns its combined output. The settings come from the test's
// environment, which smoke.sh set, with SIMCLUSTER_DIR pinned to the
// cluster's; extra adds or overrides variables.
func (e *e2eEnv) script(name string, extra map[string]string, args ...string) (string, error) {
	cmd := exec.Command("bash", append([]string{filepath.Join(e.Repo, "scripts", "simcluster", name+".sh")}, args...)...)
	cmd.Dir = e.Repo
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	env[e2eEnvDir] = e.Dir
	env["KUBECONFIG"] = filepath.Join(e.Dir, "kubeconfig")
	for k, v := range extra {
		env[k] = v
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cmd.Env = make([]string, 0, len(keys))
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+env[k])
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// helm runs helm with args against this cluster.
func (e *e2eEnv) helm(args ...string) (string, error) {
	cmd := exec.Command("helm", args...)
	cmd.Dir = e.Repo
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(e.Dir, "kubeconfig"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// e2eMetricSum is the sum of every sample of metric name in a Prometheus
// text exposition: 0 when the metric has no sample yet, as a counter vector
// before its first increment.
func e2eMetricSum(text []byte, name string) (float64, error) {
	var sum float64
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		if rest == "" || (rest[0] != '{' && rest[0] != ' ') {
			continue
		}
		if i := strings.LastIndexByte(rest, '}'); i >= 0 {
			rest = rest[i+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, fmt.Errorf("%s: no value in %q", name, line)
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %q: %w", name, line, err)
		}
		sum += v
	}
	return sum, sc.Err()
}

// TestE2EByLabel: series by their node label; a missing label or two
// series of one node are errors, never one of them kept.
func TestE2EByLabel(t *testing.T) {
	s := func(node string, v float64) scenario.PromSeries {
		return scenario.PromSeries{Labels: map[string]string{"k8s_node_name": node}, Samples: []scenario.PromSample{{Value: v}}}
	}
	got, err := e2eByLabel(scenario.PromResult{Series: []scenario.PromSeries{s("a", 1), s("b", 2)}}, "q", "k8s_node_name")
	if err != nil || len(got) != 2 || got["a"] != 1 || got["b"] != 2 {
		t.Errorf("got %v %v", got, err)
	}
	if got, err := e2eByLabel(scenario.PromResult{Series: []scenario.PromSeries{s("a", 1), s("a", 2)}}, "q", "k8s_node_name"); err == nil {
		t.Errorf("two series of one node: got %v, want an error", got)
	}
	if _, err := e2eByLabel(scenario.PromResult{Series: []scenario.PromSeries{s("", 1)}}, "q", "k8s_node_name"); err == nil {
		t.Error("a series without the label: want an error")
	}
}

// e2eEnvFilterChild marks the run of this test binary that
// TestE2ECheckOutsideTheRunFilter starts with a -run filter.
const e2eEnvFilterChild = "JOULIE_E2E_FILTER_CHILD"

// TestE2ECheckOutsideTheRunFilter runs one check of two under a subtest
// -run filter, as go test -run 'TestSimclusterE2E/E11' does against a kept
// cluster: the check the filter leaves out neither runs nor lands in the
// summary, and the run does not panic. It needs no cluster: the filtered
// run is this test binary, started again.
func TestE2ECheckOutsideTheRunFilter(t *testing.T) {
	if os.Getenv(e2eEnvFilterChild) == "1" {
		e := &e2eEnv{}
		ran := map[string]bool{}
		for _, name := range []string{"E01", "E02"} {
			e.check(t, name, "a claim", func(c *e2eCase) { ran[name] = true })
		}
		if ran["E01"] || !ran["E02"] || len(e.summary.Assertions) != 1 || e.summary.Assertions[0].Name != "E02" {
			t.Errorf("under -run .../E02: ran %v, summary %+v; want only E02 run and recorded", ran, e.summary.Assertions)
		}
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestE2ECheckOutsideTheRunFilter$/^E02$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), e2eEnvFilterChild+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the test binary with -run TestE2ECheckOutsideTheRunFilter/E02: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestE2ECheckOutsideTheRunFilter/E02") {
		t.Errorf("the filtered run did not run E02:\n%s", out)
	}
}
