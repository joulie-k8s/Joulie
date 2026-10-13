package scenario

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/matbun/joulie/api/v1alpha1"
	"github.com/matbun/joulie/pkg/kube"
	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// Namespaces and Services of the simulated cluster harness, reached through
// the API server's service proxy.
const (
	NamespaceJoulie     = "joulie-system"
	NamespaceSimulator  = "joulie-sim"
	NamespaceMonitoring = "monitoring"
	// PrometheusService is the Prometheus Service and port, as the proxy
	// path names it.
	PrometheusService = "prometheus-operated:9090"
	// SimulatorService is the simulator chart's Service and port name.
	SimulatorService = "joulie-telemetry-sim:http"
)

// ServiceProxyURL is the base URL of a Service through the API server's
// proxy: requests to it reach the Service as if sent from inside the
// cluster, with the kubeconfig's credentials.
func ServiceProxyURL(cfg *rest.Config, namespace, service string) string {
	return strings.TrimSuffix(cfg.Host, "/") + "/api/v1/namespaces/" + namespace + "/services/" + service + "/proxy"
}

// NewKubeClient returns a client whose scheme holds the core types, batch
// Jobs and Joulie's CRDs, read through their typed structs.
func NewKubeClient(cfg *rest.Config) (client.Client, error) {
	scheme, err := kube.NewScheme(v1alpha1.AddToScheme, batchv1.AddToScheme)
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

// snapshotTimePrefix starts the first line of a snapshot file, a comment
// carrying the time the snapshot was taken.
const snapshotTimePrefix = "# snapshot time: "

// SnapshotKinds are the kinds taken at each phase: Joulie's objects and the
// nodes at both, the workload's objects at the end only.
func SnapshotKinds(phase string) []string {
	if phase == SnapshotStart {
		return []string{KindNodeTwins, KindNodeHardwares, KindNodes}
	}
	return []string{KindNodeTwins, KindNodeHardwares, KindNodes, KindJobs, KindPods, KindEvents}
}

// TakeKubeSnapshot lists the kinds of phase: every NodeTwin, NodeHardware
// and Node, and the workload's Jobs and pods (label
// app.kubernetes.io/part-of=joulie-sim-workload) with the events of the
// namespaces they are in.
func TakeKubeSnapshot(ctx context.Context, c client.Reader, phase string) (KubeSnapshot, error) {
	snap := KubeSnapshot{Time: time.Now().UTC()}
	var twins v1alpha1.NodeTwinList
	var hws v1alpha1.NodeHardwareList
	var nodes corev1.NodeList
	for _, l := range []client.ObjectList{&twins, &hws, &nodes} {
		if err := c.List(ctx, l); err != nil {
			return snap, err
		}
	}
	snap.NodeTwins, snap.NodeHardwares, snap.Nodes = twins.Items, hws.Items, nodes.Items
	if phase == SnapshotStart {
		return snap, nil
	}
	sel := client.MatchingLabels{workload.LabelPartOf: workload.PartOfWorkload}
	var jobs batchv1.JobList
	var pods corev1.PodList
	if err := c.List(ctx, &jobs, sel); err != nil {
		return snap, err
	}
	if err := c.List(ctx, &pods, sel); err != nil {
		return snap, err
	}
	snap.Jobs, snap.Pods = jobs.Items, pods.Items
	namespaces := map[string]bool{}
	for _, j := range jobs.Items {
		namespaces[j.Namespace] = true
	}
	for _, p := range pods.Items {
		namespaces[p.Namespace] = true
	}
	for _, ns := range sortedKeys(namespaces) {
		var events corev1.EventList
		if err := c.List(ctx, &events, client.InNamespace(ns)); err != nil {
			return snap, err
		}
		snap.Events = append(snap.Events, events.Items...)
	}
	return snap, nil
}

// kindItems returns the objects of one kind, with their apiVersion and kind
// set, as kubectl writes them.
func (s KubeSnapshot) kindItems(kind string) []any {
	var out []any
	add := func(o any) { out = append(out, o) }
	switch kind {
	case KindNodeTwins:
		for _, x := range s.NodeTwins {
			x.APIVersion, x.Kind = v1alpha1.GroupVersion.String(), "NodeTwin"
			add(x)
		}
	case KindNodeHardwares:
		for _, x := range s.NodeHardwares {
			x.APIVersion, x.Kind = v1alpha1.GroupVersion.String(), "NodeHardware"
			add(x)
		}
	case KindNodes:
		for _, x := range s.Nodes {
			x.APIVersion, x.Kind = "v1", "Node"
			add(x)
		}
	case KindJobs:
		for _, x := range s.Jobs {
			x.APIVersion, x.Kind = batchv1.SchemeGroupVersion.String(), "Job"
			add(x)
		}
	case KindPods:
		for _, x := range s.Pods {
			x.APIVersion, x.Kind = "v1", "Pod"
			add(x)
		}
	case KindEvents:
		for _, x := range s.Events {
			x.APIVersion, x.Kind = "v1", "Event"
			add(x)
		}
	}
	return out
}

// WriteKubeSnapshot writes the kinds of phase from s under dir, one
// KubeSnapshotFile per kind: a v1 List, as kubectl get -o yaml writes,
// after a comment line with the snapshot time.
func WriteKubeSnapshot(dir, phase string, s KubeSnapshot) error {
	if err := os.MkdirAll(filepath.Join(dir, KubeDir), 0o755); err != nil {
		return err
	}
	for _, kind := range SnapshotKinds(phase) {
		items := s.kindItems(kind)
		if items == nil {
			items = []any{}
		}
		b, err := yaml.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
		if err != nil {
			return err
		}
		head := snapshotTimePrefix + s.Time.UTC().Format(time.RFC3339Nano) + "\n"
		if err := os.WriteFile(filepath.Join(dir, KubeSnapshotFile(kind, phase)), append([]byte(head), b...), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ReadKubeSnapshot reads the snapshot files of phase under dir into the
// typed structs. A missing file leaves its kind empty; with no file at all
// the snapshot's Time is zero. The time is the first file's comment line,
// or its modification time when it has none.
func ReadKubeSnapshot(dir, phase string) (KubeSnapshot, error) {
	var s KubeSnapshot
	for _, kind := range SnapshotKinds(phase) {
		path := filepath.Join(dir, KubeSnapshotFile(kind, phase))
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return s, err
		}
		if s.Time.IsZero() {
			s.Time = snapshotTime(b, path)
		}
		var dst any
		switch kind {
		case KindNodeTwins:
			dst = &s.NodeTwins
		case KindNodeHardwares:
			dst = &s.NodeHardwares
		case KindNodes:
			dst = &s.Nodes
		case KindJobs:
			dst = &s.Jobs
		case KindPods:
			dst = &s.Pods
		case KindEvents:
			dst = &s.Events
		}
		if err := decodeList(b, dst); err != nil {
			return s, fmt.Errorf("%s: %w", path, err)
		}
	}
	return s, nil
}

// decodeList decodes the items of a v1 List into dst, a pointer to a slice
// of typed objects. Unknown fields are kept out, not refused, so a
// snapshot taken by a newer kubectl still reads.
func decodeList(b []byte, dst any) error {
	j, err := yaml.YAMLToJSON(b)
	if err != nil {
		return err
	}
	var list struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(j, &list); err != nil {
		return err
	}
	if len(list.Items) == 0 || bytes.Equal(list.Items, []byte("null")) {
		return nil
	}
	return json.Unmarshal(list.Items, dst)
}

func snapshotTime(b []byte, path string) time.Time {
	line, _, _ := bufio.NewReader(bytes.NewReader(b)).ReadLine()
	if rest, ok := strings.CutPrefix(string(line), snapshotTimePrefix); ok {
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(rest)); err == nil {
			return t
		}
	}
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime().UTC()
	}
	return time.Time{}
}

// CaptureNodeTwins returns the NodeTwin samples at one capture as the
// runner's metrics: each twin's measured power, when it has a
// measurement, and the state sets of its schedulable class and of each
// component's control result, 1 for the state in effect. start is the
// run's start, the start time of the up-down counters.
func CaptureNodeTwins(twins []v1alpha1.NodeTwin, at, start time.Time) []otlpjson.Metric {
	twins = append([]v1alpha1.NodeTwin(nil), twins...)
	sort.Slice(twins, func(i, j int) bool { return twins[i].Spec.NodeName < twins[j].Spec.NodeName })
	power := &otlpjson.Gauge{}
	class := &otlpjson.Sum{AggregationTemporality: otlpjson.AggregationTemporalityCumulative}
	control := &otlpjson.Sum{AggregationTemporality: otlpjson.AggregationTemporalityCumulative}
	ts, st := otlpjson.Uint64String(at.UnixNano()), otlpjson.Uint64String(start.UnixNano())
	one := otlpjson.Int64String(1)
	for _, t := range twins {
		node := otlpjson.String(semconv.AttrK8sNodeName, t.Spec.NodeName)
		if m := t.Status.PowerMeasurement; m != nil {
			w := m.MeasuredNodePowerW
			power.DataPoints = append(power.DataPoints, otlpjson.NumberDataPoint{
				Attributes:   []otlpjson.KeyValue{node, otlpjson.String(semconv.AttrJoulieNodeTwinPowerSource, m.Source)},
				TimeUnixNano: ts, AsDouble: &w,
			})
		}
		if c := t.Status.SchedulableClass; c != "" {
			class.DataPoints = append(class.DataPoints, otlpjson.NumberDataPoint{
				Attributes:        []otlpjson.KeyValue{node, otlpjson.String(semconv.AttrJoulieNodeSchedulableClass, c)},
				StartTimeUnixNano: st, TimeUnixNano: ts, AsInt: &one,
			})
		}
		if cs := t.Status.ControlStatus; cs != nil {
			for _, comp := range []struct {
				hw string
				r  *v1alpha1.ControlResult
			}{{semconv.HwTypeCPU, cs.CPU}, {semconv.HwTypeGPU, cs.GPU}} {
				if comp.r == nil {
					continue
				}
				control.DataPoints = append(control.DataPoints, otlpjson.NumberDataPoint{
					Attributes: []otlpjson.KeyValue{node, otlpjson.String(semconv.AttrHwType, comp.hw),
						otlpjson.String(semconv.AttrJoulieNodeTwinControlBackend, comp.r.Backend),
						otlpjson.String(semconv.AttrJoulieNodeTwinControlResult, comp.r.Result)},
					StartTimeUnixNano: st, TimeUnixNano: ts, AsInt: &one,
				})
			}
		}
	}
	var out []otlpjson.Metric
	if len(power.DataPoints) > 0 {
		out = append(out, otlpjson.Metric{Name: semconv.JoulieCaptureNodeTwinMeasuredPower.Name, Unit: semconv.JoulieCaptureNodeTwinMeasuredPower.Unit, Gauge: power})
	}
	if len(class.DataPoints) > 0 {
		out = append(out, otlpjson.Metric{Name: semconv.JoulieCaptureNodeTwinSchedulableClass.Name, Unit: semconv.JoulieCaptureNodeTwinSchedulableClass.Unit, Sum: class})
	}
	if len(control.DataPoints) > 0 {
		out = append(out, otlpjson.Metric{Name: semconv.JoulieCaptureNodeTwinControlResult.Name, Unit: semconv.JoulieCaptureNodeTwinControlResult.Unit, Sum: control})
	}
	return out
}

// Component logs collected into LogsDir: the controller manager, the
// scheduler extender and the simulator whole, each agent's last lines.
const agentLogLines = 500

// agentNodeLabel names the Node an emulated agent stands in for.
const agentNodeLabel = "sim.joulie.io/node"

// CollectLogs writes the logs of the Joulie and simulator pods under
// dir/LogsDir: controller-manager.log, scheduler.log, simulator.log and
// agent-<node>.log (last 500 lines). It returns the pods it could not read,
// which do not stop the others.
func CollectLogs(ctx context.Context, cs kubernetes.Interface, dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, LogsDir), 0o755); err != nil {
		return err
	}
	var errs []error
	for _, ns := range []string{NamespaceJoulie, NamespaceSimulator} {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range pods.Items {
			name, tail := logFileName(p)
			if name == "" {
				continue
			}
			opts := &corev1.PodLogOptions{}
			if tail > 0 {
				opts.TailLines = &tail
			}
			if err := copyLog(ctx, cs, p, opts, filepath.Join(dir, LogsDir, name)); err != nil {
				errs = append(errs, fmt.Errorf("pod %s/%s: %w", p.Namespace, p.Name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// logFileName maps a pod to its log file and line limit; "" skips it.
func logFileName(p corev1.Pod) (string, int64) {
	switch {
	case p.Namespace == NamespaceSimulator:
		return "simulator.log", 0
	case strings.Contains(p.Name, "controller-manager"):
		return "controller-manager.log", 0
	case strings.Contains(p.Name, "scheduler"):
		return "scheduler.log", 0
	case strings.Contains(p.Name, "agent"):
		node := p.Labels[agentNodeLabel]
		if node == "" {
			node = p.Spec.NodeName
		}
		return "agent-" + node + ".log", agentLogLines
	}
	return "", 0
}

func copyLog(ctx context.Context, cs kubernetes.Interface, p corev1.Pod, opts *corev1.PodLogOptions, path string) error {
	rc, err := cs.CoreV1().Pods(p.Namespace).GetLogs(p.Name, opts).Stream(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// The KWOK Stage the simulator's completion relies on.
const (
	stageGroupVersion  = "kwok.x-k8s.io/v1alpha1"
	stageKind          = "Stage"
	ownerReferencesKey = "ownerReferences"
)

// CheckPodCompleteStage checks the KWOK Stage workload.StagePodComplete,
// as JSON: its selector must name the completion annotation, or the
// annotated pods never succeed, and must not select by owner, or KWOK
// completes Job pods on its own timer, before their work is done.
func CheckPodCompleteStage(stage []byte) error {
	var st struct {
		Spec struct {
			Selector json.RawMessage `json:"selector"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(stage, &st); err != nil {
		return fmt.Errorf("stage %s: %w", workload.StagePodComplete, err)
	}
	sel := string(st.Spec.Selector)
	switch {
	case !strings.Contains(sel, workload.AnnotationComplete):
		return fmt.Errorf("stage %s: the selector does not select the annotation %s; apply scripts/simcluster/manifests/stage-pod-complete.yaml",
			workload.StagePodComplete, workload.AnnotationComplete)
	case strings.Contains(sel, ownerReferencesKey):
		return fmt.Errorf("stage %s: the selector selects by %s, so KWOK would complete Job pods on its own timer; apply scripts/simcluster/manifests/stage-pod-complete.yaml",
			workload.StagePodComplete, ownerReferencesKey)
	}
	return nil
}

// GetPodCompleteStage reads the KWOK Stage workload.StagePodComplete as
// JSON.
func GetPodCompleteStage(ctx context.Context, c client.Reader) ([]byte, error) {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(stageGroupVersion)
	u.SetKind(stageKind)
	if err := c.Get(ctx, client.ObjectKey{Name: workload.StagePodComplete}, u); err != nil {
		return nil, fmt.Errorf("stage %s: %w", workload.StagePodComplete, err)
	}
	return u.MarshalJSON()
}
