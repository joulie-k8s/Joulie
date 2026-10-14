package simcluster_test

// The manifests under scripts/simcluster/manifests, the pins in
// scripts/simcluster/versions.env they must agree with, and the Dagger
// pipeline and workflow that share the scheduler configuration. The
// package-level names of this file carry the prefix m.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	k8sjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Paths from the repository root, which is two levels above this package.
const (
	mVersionsFile    = "scripts/simcluster/versions.env"
	mManifestsDir    = "scripts/simcluster/manifests"
	mPrometheusFile  = mManifestsDir + "/prometheus.yaml"
	mOtelcolFile     = mManifestsDir + "/otelcol.yaml"
	mSchedulerConfig = mManifestsDir + "/scheduler-config.yaml"
	// The scheduler configuration of SIMCLUSTER_EXTENDER=service and the
	// Joulie values deploy.sh adds with it.
	mSchedulerConfigService = mManifestsDir + "/scheduler-config-service.yaml"
	mExtenderServiceValues  = "values/simcluster-extender-service.yaml"
	mOtelcolBridgeFile      = mManifestsDir + "/otelcol-bridge.yaml"
	mDaggerFile             = "ci/src/joulie_ci/__init__.py"
	mWorkflowFile           = ".github/workflows/integration-tests.yml"
)

// The names the scripts, the generated telemetry configuration and the
// Joulie chart (PROMETHEUS_ADDRESS) rely on.
const (
	mNamespace          = "monitoring"
	mPrometheusService  = "prometheus-operated"
	mPrometheusPort     = 9090
	mPrometheusConfig   = "prometheus-config"
	mPrometheusFileName = "prometheus.yml"
	mOtelcolService     = "otelcol"
	mOtelcolConfig      = "otelcol-config"
	mOtelcolFileName    = "config.yaml"
	mInfraLabel         = "joulie.io/infra"
	mKWOKImage          = "registry.k8s.io/kwok/kwok"
)

// The Dagger method that builds the k3s server, and the path inside its
// container that --kube-scheduler-arg=--config names.
const (
	mDaggerServer       = "_k3s_server_service"
	mK3sSchedulerConfig = "/etc/rancher/k3s/scheduler-config.yaml"
)

// mPrometheusArgs are the flags the harness Prometheus needs: the OTLP
// receiver, POST /-/reload for a regenerated configuration, and a retention
// that covers a nightly run.
var mPrometheusArgs = []string{"--web.enable-otlp-receiver", "--web.enable-lifecycle", "--storage.tsdb.retention.time=12h"}

// mOtelcolPorts are the Collector's OTLP/HTTP receiver, which the readiness
// probe checks, and its prometheus exporter.
var mOtelcolPorts = []int32{4318, 8889}

// mAPIVersions is the apiVersion of each kind a harness manifest may hold.
// A kind outside it fails, so a new kind is added here with its typed
// decoding in mDecodeTyped.
var mAPIVersions = map[string]string{
	"Namespace":          "v1",
	"ServiceAccount":     "v1",
	"Service":            "v1",
	"ClusterRole":        "rbac.authorization.k8s.io/v1",
	"ClusterRoleBinding": "rbac.authorization.k8s.io/v1",
	"Deployment":         "apps/v1",
}

// mDecoder reads a manifest document as the API server's strict field
// validation does, which kubectl apply requests by default: field names match
// case sensitively, and an unknown or repeated field is an error.
// yaml.UnmarshalStrict goes through encoding/json, which matches names
// without regard to case and so accepts imagePullpolicy.
var mDecoder = func() runtime.Decoder {
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(s); err != nil {
			panic(err)
		}
	}
	return k8sjson.NewSerializerWithOptions(k8sjson.DefaultMetaFactory, s, s, k8sjson.SerializerOptions{Yaml: true, Strict: true})
}()

func mPath(rel string) string {
	return filepath.Join("..", "..", filepath.FromSlash(rel))
}

func mRead(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(mPath(rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// mVersions parses versions.env the way lib.sh sources it: KEY=VALUE lines
// without quotes or spaces, comments and blank lines. Any other line, or a
// key set twice, fails.
func mVersions(t *testing.T) map[string]string {
	t.Helper()
	line := regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=(\S*)$`)
	out := map[string]string{}
	for i, l := range strings.Split(string(mRead(t, mVersionsFile)), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		m := line.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("%s:%d: %q is not KEY=VALUE", mVersionsFile, i+1, l)
		}
		if _, ok := out[m[1]]; ok {
			t.Fatalf("%s:%d: %s is set twice", mVersionsFile, i+1, m[1])
		}
		out[m[1]] = m[2]
	}
	return out
}

// mVersion returns one pin of versions.env and fails when it is missing or
// empty.
func mVersion(t *testing.T, versions map[string]string, key string) string {
	t.Helper()
	v := versions[key]
	if v == "" {
		t.Fatalf("%s sets no %s", mVersionsFile, key)
	}
	return v
}

// mDoc is one document of a manifest file.
type mDoc struct {
	APIVersion, Kind, Namespace, Name string
	YAML                              []byte
}

func (d mDoc) String() string {
	if d.Namespace == "" {
		return d.Kind + " " + d.Name
	}
	return d.Kind + " " + d.Namespace + "/" + d.Name
}

// mDocs splits a multi-document YAML file and reads each document's kind and
// name. Documents holding only comments are dropped.
func mDocs(t *testing.T, rel string) []mDoc {
	t.Helper()
	r := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(mRead(t, rel))))
	var out []mDoc
	for {
		doc, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		var head struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &head); err != nil {
			t.Fatalf("%s: document %d: %v", rel, len(out)+1, err)
		}
		if head.Kind == "" && head.APIVersion == "" {
			continue
		}
		out = append(out, mDoc{head.APIVersion, head.Kind, head.Metadata.Namespace, head.Metadata.Name, doc})
	}
}

// mManifest is a harness manifest decoded into the typed Kubernetes API.
type mManifest struct {
	File                string
	Docs                []mDoc
	Namespaces          []corev1.Namespace
	ServiceAccounts     []corev1.ServiceAccount
	Services            []corev1.Service
	ClusterRoles        []rbacv1.ClusterRole
	ClusterRoleBindings []rbacv1.ClusterRoleBinding
	Deployments         []appsv1.Deployment
}

// mDecodeTyped decodes every document of a harness manifest strictly into
// its typed API object with mDecoder, so a misspelt or misplaced field, or
// one whose case is wrong, fails here rather than when up.sh applies the
// file. Each kind must be in mAPIVersions
// with its apiVersion, and each namespaced object must name its namespace,
// since the scripts apply these files without -n.
func mDecodeTyped(t *testing.T, rel string) *mManifest {
	t.Helper()
	m := &mManifest{File: rel, Docs: mDocs(t, rel)}
	seen := map[string]bool{}
	for _, d := range m.Docs {
		want, ok := mAPIVersions[d.Kind]
		if !ok {
			t.Errorf("%s: %s: kind %q is not one of mAPIVersions", rel, d, d.Kind)
			continue
		}
		if d.APIVersion != want {
			t.Errorf("%s: %s: apiVersion %q, want %q", rel, d, d.APIVersion, want)
		}
		if seen[d.String()] {
			t.Errorf("%s: %s appears twice", rel, d)
		}
		seen[d.String()] = true
		var into runtime.Object
		switch d.Kind {
		case "Namespace":
			m.Namespaces = append(m.Namespaces, corev1.Namespace{})
			into = &m.Namespaces[len(m.Namespaces)-1]
		case "ServiceAccount":
			m.ServiceAccounts = append(m.ServiceAccounts, corev1.ServiceAccount{})
			into = &m.ServiceAccounts[len(m.ServiceAccounts)-1]
		case "Service":
			m.Services = append(m.Services, corev1.Service{})
			into = &m.Services[len(m.Services)-1]
		case "ClusterRole":
			m.ClusterRoles = append(m.ClusterRoles, rbacv1.ClusterRole{})
			into = &m.ClusterRoles[len(m.ClusterRoles)-1]
		case "ClusterRoleBinding":
			m.ClusterRoleBindings = append(m.ClusterRoleBindings, rbacv1.ClusterRoleBinding{})
			into = &m.ClusterRoleBindings[len(m.ClusterRoleBindings)-1]
		case "Deployment":
			m.Deployments = append(m.Deployments, appsv1.Deployment{})
			into = &m.Deployments[len(m.Deployments)-1]
		}
		if obj, _, err := mDecoder.Decode(d.YAML, nil, into); err != nil {
			t.Errorf("%s: %s: %v", rel, d, err)
		} else if obj != into {
			t.Errorf("%s: %s decodes to %T, not %T", rel, d, obj, into)
		}
		clusterScoped := d.Kind == "Namespace" || d.Kind == "ClusterRole" || d.Kind == "ClusterRoleBinding"
		if !clusterScoped && d.Namespace == "" {
			t.Errorf("%s: %s names no namespace", rel, d)
		}
	}
	return m
}

func (m *mManifest) deployment(t *testing.T, name string) *appsv1.Deployment {
	t.Helper()
	for i := range m.Deployments {
		if d := &m.Deployments[i]; d.Name == name && d.Namespace == mNamespace {
			return d
		}
	}
	t.Fatalf("%s has no Deployment %s/%s", m.File, mNamespace, name)
	return nil
}

func (m *mManifest) service(t *testing.T, name string) *corev1.Service {
	t.Helper()
	for i := range m.Services {
		if s := &m.Services[i]; s.Name == name && s.Namespace == mNamespace {
			return s
		}
	}
	t.Fatalf("%s has no Service %s/%s", m.File, mNamespace, name)
	return nil
}

// mCheckWorkload checks what the Prometheus and Collector Deployments share:
// one replica replaced with Recreate, the infra node selector, one container
// running image by tag with IfNotPresent (images.sh imports it by tag, and a
// digest reference does not survive the import), a Service whose selector
// and target ports reach that container, and the generated configuration
// mounted from ConfigMap configMap. It returns the container and the path
// the ConfigMap is mounted at.
func mCheckWorkload(t *testing.T, file string, d *appsv1.Deployment, svc *corev1.Service, image, configMap string) (*corev1.Container, string) {
	t.Helper()
	at := file + ": Deployment " + d.Name
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
		t.Errorf("%s: replicas %v, want 1", at, d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("%s: strategy %q, want %s", at, d.Spec.Strategy.Type, appsv1.RecreateDeploymentStrategyType)
	}
	pod := d.Spec.Template
	if got := pod.Spec.NodeSelector; len(got) != 1 || got[mInfraLabel] != "true" {
		t.Errorf("%s: nodeSelector %v, want only %s: \"true\"", at, got, mInfraLabel)
	}
	if d.Spec.Selector == nil || len(d.Spec.Selector.MatchLabels) == 0 || len(d.Spec.Selector.MatchExpressions) != 0 {
		t.Fatalf("%s: selector %v, want matchLabels only", at, d.Spec.Selector)
	}
	if !labels.SelectorFromSet(d.Spec.Selector.MatchLabels).Matches(labels.Set(pod.Labels)) {
		t.Errorf("%s: selector %v does not match the pod labels %v", at, d.Spec.Selector.MatchLabels, pod.Labels)
	}
	if len(svc.Spec.Selector) == 0 || !labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(pod.Labels)) {
		t.Errorf("%s: Service %s selector %v does not match the pod labels %v", file, svc.Name, svc.Spec.Selector, pod.Labels)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("%s: %d containers, want 1", at, len(pod.Spec.Containers))
	}
	c := &pod.Spec.Containers[0]
	if c.Image != image {
		t.Errorf("%s: image %q, want %q from %s", at, c.Image, image, mVersionsFile)
	}
	if strings.Contains(c.Image, "@") {
		t.Errorf("%s: image %q is a digest reference; the manifests use tags", at, c.Image)
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("%s: imagePullPolicy %q, want %s", at, c.ImagePullPolicy, corev1.PullIfNotPresent)
	}
	for _, p := range svc.Spec.Ports {
		if _, ok := mTarget(c, p); !ok {
			t.Errorf("%s: Service %s port %d targets %s, which container %s does not expose", file, svc.Name, p.Port, p.TargetPort.String(), c.Name)
		}
	}
	if c.ReadinessProbe == nil {
		t.Errorf("%s: container %s has no readiness probe; up.sh waits on it", at, c.Name)
	}
	volume := ""
	for _, v := range pod.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == configMap {
			volume = v.Name
		}
	}
	if volume == "" {
		t.Fatalf("%s: no volume from ConfigMap %s, which up.sh generates", at, configMap)
	}
	return c, mMountPath(t, at, c, volume)
}

// mPort returns the container port a name or a number refers to, as a
// Service targetPort or a probe port does.
func mPort(c *corev1.Container, ref intstr.IntOrString) (int32, bool) {
	for _, p := range c.Ports {
		if (ref.Type == intstr.String && p.Name == ref.StrVal) || (ref.Type == intstr.Int && p.ContainerPort == ref.IntVal) {
			return p.ContainerPort, true
		}
	}
	return 0, false
}

// mTarget returns the container port a Service port reaches. An unset
// targetPort defaults to the port itself, as the API server does.
func mTarget(c *corev1.Container, sp corev1.ServicePort) (int32, bool) {
	ref := sp.TargetPort
	if ref.Type == intstr.Int && ref.IntVal == 0 {
		ref = intstr.FromInt32(sp.Port)
	}
	return mPort(c, ref)
}

// mMountPath returns where the container mounts a volume.
func mMountPath(t *testing.T, at string, c *corev1.Container, volume string) string {
	t.Helper()
	for _, m := range c.VolumeMounts {
		if m.Name == volume {
			return m.MountPath
		}
	}
	t.Fatalf("%s: container %s does not mount volume %s", at, c.Name, volume)
	return ""
}

// mServicePort returns the Service port with that number.
func mServicePort(svc *corev1.Service, port int32) (corev1.ServicePort, bool) {
	for _, p := range svc.Spec.Ports {
		if p.Port == port {
			return p, true
		}
	}
	return corev1.ServicePort{}, false
}

// mRuleAllows reports whether some rule grants verb on resource in group.
func mRuleAllows(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	has := func(list []string, v string) bool {
		for _, x := range list {
			if x == v || x == "*" {
				return true
			}
		}
		return false
	}
	for _, r := range rules {
		if has(r.APIGroups, group) && has(r.Resources, resource) && has(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// mDaggerConstant returns the string a top-level Python assignment
// NAME = "..." in the Dagger module holds, and fails unless there is exactly
// one.
func mDaggerConstant(t *testing.T, src []byte, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*=\s*"([^"\n]*)"\s*$`)
	m := re.FindAllSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("%s: %d assignments %s = \"...\", want 1", mDaggerFile, len(m), name)
	}
	return string(m[0][1])
}

// mPythonKeyword matches a keyword argument name=value.
var mPythonKeyword = regexp.MustCompile(`(?s)^([A-Za-z_][A-Za-z0-9_]*)\s*=([^=].*)$`)

// mPythonArgs splits the Python argument or parameter list that src starts
// inside (just after the opening parenthesis) at its top-level commas. It
// returns the trimmed entries, comments dropped, and the offset just past the
// closing parenthesis. what names the call in a failure.
func mPythonArgs(t *testing.T, src []byte, what string) ([]string, int) {
	t.Helper()
	var args []string
	var cur []byte
	depth := 0
	var quote byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		if quote == 0 && c == '#' {
			for i+1 < len(src) && src[i+1] != '\n' {
				i++
			}
			continue
		}
		if quote == 0 && depth == 0 && (c == ',' || c == ')') {
			if a := strings.TrimSpace(string(cur)); a != "" {
				args = append(args, a)
			}
			cur = cur[:0]
			if c == ')' {
				return args, i + 1
			}
			continue
		}
		cur = append(cur, c)
		switch {
		case quote != 0 && c == '\\' && i+1 < len(src):
			i++
			cur = append(cur, src[i])
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
		case c == '"' || c == '\'':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		}
	}
	t.Fatalf("%s: the arguments of %s never close", mDaggerFile, what)
	return nil, 0
}

// mPythonCompact drops the whitespace of a Python expression, so that
// expressions compare whatever their layout.
func mPythonCompact(expr string) string {
	return strings.Join(strings.Fields(expr), "")
}

// mPythonDef returns the parameter names, self excluded, and the body of the
// one Python function or method def name(...) in src. The body ends at the
// first non-blank line indented no deeper than the def.
func mPythonDef(t *testing.T, src []byte, name string) ([]string, []byte) {
	t.Helper()
	m := regexp.MustCompile(`(?m)^([ \t]*)def `+regexp.QuoteMeta(name)+`\(`).FindAllSubmatchIndex(src, -1)
	if len(m) != 1 {
		t.Fatalf("%s: %d definitions of %s, want 1", mDaggerFile, len(m), name)
	}
	indent := m[0][3] - m[0][2]
	args, n := mPythonArgs(t, src[m[0][1]:], name)
	var params []string
	for _, a := range args {
		if i := strings.IndexAny(a, ":="); i >= 0 {
			a = a[:i]
		}
		if a = strings.TrimSpace(a); a != "self" {
			params = append(params, a)
		}
	}
	rest := src[m[0][1]+n:]
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	} else {
		rest = nil
	}
	for off := 0; off < len(rest); {
		line := rest[off:]
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if code := bytes.TrimLeft(line, " \t"); len(code) > 0 && len(line)-len(code) <= indent {
			return params, rest[:off]
		}
		off += len(line) + 1
	}
	return params, rest
}

// TestM01VendoredKWOKMatchesVersions checks that kwok.yaml and
// stage-fast.yaml are the KWOK release assets byte for byte, under a
// directory named for KWOK_VERSION, and that nothing else sits beside them.
// Harness changes to the KWOK stages go in scripts/simcluster/manifests/
// stage-*.yaml, which up.sh applies after the vendored files, so a Stage of
// the same name replaces the vendored one.
func TestM01VendoredKWOKMatchesVersions(t *testing.T) {
	v := mVersions(t)
	dir := path.Join(mManifestsDir, "kwok-"+mVersion(t, v, "KWOK_VERSION"))
	want := map[string]string{
		"kwok.yaml":       mVersion(t, v, "KWOK_YAML_SHA256"),
		"stage-fast.yaml": mVersion(t, v, "KWOK_STAGE_FAST_SHA256"),
	}
	entries, err := os.ReadDir(mPath(dir))
	if err != nil {
		t.Fatalf("%v; vendor the KWOK %s release assets there", err, v["KWOK_VERSION"])
	}
	for _, e := range entries {
		if _, ok := want[e.Name()]; !ok {
			t.Errorf("%s/%s is not a vendored release asset; harness stages go in %s/stage-*.yaml", dir, e.Name(), mManifestsDir)
		}
	}
	for _, name := range spSortedKeys(want) {
		sum := sha256.Sum256(mRead(t, path.Join(dir, name)))
		if got := hex.EncodeToString(sum[:]); got != want[name] {
			t.Errorf("%s/%s has sha256 %s, but %s pins %s; vendor the release asset unmodified", dir, name, got, mVersionsFile, want[name])
		}
	}
}

// TestM02KWOKImageTagIsKWOKVersion checks that the vendored kwok.yaml runs
// the KWOK controller image of KWOK_VERSION, and no image of another tag.
func TestM02KWOKImageTagIsKWOKVersion(t *testing.T) {
	v := mVersions(t)
	version := mVersion(t, v, "KWOK_VERSION")
	file := path.Join(mManifestsDir, "kwok-"+version, "kwok.yaml")
	var images []string
	for _, d := range mDocs(t, file) {
		var w struct {
			Spec struct {
				Template struct {
					Spec corev1.PodSpec `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(d.YAML, &w); err != nil {
			t.Fatalf("%s: %s: %v", file, d, err)
		}
		pod := w.Spec.Template.Spec
		for _, c := range append(append([]corev1.Container(nil), pod.InitContainers...), pod.Containers...) {
			images = append(images, c.Image)
		}
	}
	controller := false
	for _, image := range images {
		i := strings.LastIndex(image, ":")
		if i < 0 || strings.Contains(image[i:], "/") {
			t.Errorf("%s: image %q has no tag", file, image)
			continue
		}
		if tag := image[i+1:]; tag != version {
			t.Errorf("%s: image %q has tag %s, but KWOK_VERSION is %s", file, image, tag, version)
		}
		if image[:i] == mKWOKImage {
			controller = true
		}
	}
	if !controller {
		t.Errorf("%s runs no %s image; images: %v", file, mKWOKImage, images)
	}
	// images.sh imports KWOK_IMAGE, so the cluster never pulls the image
	// kwok.yaml runs.
	if want := mVersion(t, v, "KWOK_IMAGE"); !mContainsString(images, want) {
		t.Errorf("KWOK_IMAGE %s is not the image %s runs (%v)", want, file, images)
	}
	if d := mVersion(t, v, "KWOK_DIGEST"); !strings.HasPrefix(d, "sha256:") || len(d) != len("sha256:")+64 {
		t.Errorf("KWOK_DIGEST %q is not a sha256 digest", d)
	}
}

func mContainsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestM03K3sImageMatchesDagger checks that the harness and the Dagger
// integration pipeline run the same k3s.
func TestM03K3sImageMatchesDagger(t *testing.T) {
	want := mVersion(t, mVersions(t), "K3S_IMAGE")
	if got := mDaggerConstant(t, mRead(t, mDaggerFile), "_K3S_IMAGE"); got != want {
		t.Errorf("%s: _K3S_IMAGE is %q, but %s pins K3S_IMAGE=%s", mDaggerFile, got, mVersionsFile, want)
	}
}

// TestM04DaggerReadsTheSharedSchedulerConfig checks that the Dagger pipeline
// gives its k3s server the repository's scheduler configuration, read from
// the source directory, and keeps no copy of its own that could drift from
// the harness's. Every call of _k3s_server_service passes the file through
// the same parameter, and the method puts that parameter at the path
// kube-scheduler is pointed at, directly or through a cp.
func TestM04DaggerReadsTheSharedSchedulerConfig(t *testing.T) {
	src := mRead(t, mDaggerFile)
	if bytes.Contains(src, []byte("KubeSchedulerConfiguration")) {
		t.Errorf("%s holds a KubeSchedulerConfiguration literal; read %s instead", mDaggerFile, mSchedulerConfig)
	}
	if _, err := os.Stat(mPath(mSchedulerConfig)); err != nil {
		t.Error(err)
	}
	re := regexp.MustCompile(`(?m)^(_[A-Z0-9_]+)\s*=\s*"` + regexp.QuoteMeta(mSchedulerConfig) + `"\s*$`)
	m := re.FindAllSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("%s: %d constants hold %q, want 1", mDaggerFile, len(m), mSchedulerConfig)
	}
	read := "source.file(" + string(m[0][1]) + ")"

	params, body := mPythonDef(t, src, mDaggerServer)
	calls := regexp.MustCompile(`self\.`+regexp.QuoteMeta(mDaggerServer)+`\(`).FindAllIndex(src, -1)
	if len(calls) == 0 {
		t.Fatalf("%s never calls self.%s", mDaggerFile, mDaggerServer)
	}
	param := ""
	for _, c := range calls {
		args, _ := mPythonArgs(t, src[c[1]:], mDaggerServer)
		got := ""
		for i, a := range args {
			if kw := mPythonKeyword.FindStringSubmatch(a); kw != nil {
				if mPythonCompact(kw[2]) == read {
					got = kw[1]
				}
			} else if mPythonCompact(a) == read && i < len(params) {
				got = params[i]
			}
		}
		line := 1 + bytes.Count(src[:c[0]], []byte("\n"))
		switch {
		case got == "":
			t.Errorf("%s:%d: %s gets no %s; every k3s server gets the shared file", mDaggerFile, line, mDaggerServer, read)
		case param == "":
			param = got
		case got != param:
			t.Errorf("%s:%d: %s gets %s as %s, but another call passes it as %s", mDaggerFile, line, mDaggerServer, read, got, param)
		}
	}
	if param == "" {
		t.FailNow()
	}

	put := regexp.MustCompile(`\.with_file\(\s*"([^"]+)"\s*,\s*`+regexp.QuoteMeta(param)+`\s*\)`).FindAllSubmatchIndex(body, -1)
	if len(put) != 1 {
		t.Fatalf("%s: %s puts its %s parameter into the container %d times, want 1", mDaggerFile, mDaggerServer, param, len(put))
	}
	if dest := string(body[put[0][2]:put[0][3]]); dest != mK3sSchedulerConfig {
		cp := fmt.Sprintf(`.with_exec(["cp", %q, %q])`, dest, mK3sSchedulerConfig)
		if !bytes.Contains(body[put[0][1]:], []byte(cp)) {
			t.Errorf("%s: %s puts %s at %s but never copies it to %s", mDaggerFile, mDaggerServer, param, dest, mK3sSchedulerConfig)
		}
	}
	if !bytes.Contains(body, []byte("--kube-scheduler-arg=--config="+mK3sSchedulerConfig)) {
		t.Errorf("%s: %s no longer points kube-scheduler at %s", mDaggerFile, mDaggerServer, mK3sSchedulerConfig)
	}
}

// TestM05PrometheusManifest checks prometheus.yaml: the monitoring
// namespace; Prometheus by the PROMETHEUS_IMAGE tag with the OTLP receiver
// and the other mPrometheusArgs, its configuration from the generated
// ConfigMap and its TSDB on an emptyDir; readiness on /-/ready; the Service
// prometheus-operated:9090 that the Joulie chart's PROMETHEUS_ADDRESS
// names; and read access for service discovery.
func TestM05PrometheusManifest(t *testing.T) {
	m := mDecodeTyped(t, mPrometheusFile)
	if len(m.Namespaces) != 1 || m.Namespaces[0].Name != mNamespace {
		t.Errorf("%s: %d Namespaces, want only %s, which otelcol.yaml relies on", m.File, len(m.Namespaces), mNamespace)
	}
	for _, d := range m.Docs {
		if d.Kind == "ConfigMap" {
			t.Errorf("%s: %s: up.sh applies the generated configuration; a ConfigMap here overwrites it", m.File, d)
		}
	}
	d := m.deployment(t, "prometheus")
	svc := m.service(t, mPrometheusService)
	c, configDir := mCheckWorkload(t, m.File, d, svc, mVersion(t, mVersions(t), "PROMETHEUS_IMAGE"), mPrometheusConfig)

	args := map[string]bool{}
	for _, a := range c.Args {
		args[a] = true
	}
	if len(c.Command) != 0 {
		t.Errorf("%s: command %v replaces the image's /bin/prometheus", m.File, c.Command)
	}
	for _, a := range mPrometheusArgs {
		if !args[a] {
			t.Errorf("%s: args %v lack %s", m.File, c.Args, a)
		}
	}
	if a := "--config.file=" + path.Join(configDir, mPrometheusFileName); !args[a] {
		t.Errorf("%s: args %v lack %s, the key of ConfigMap %s", m.File, c.Args, a, mPrometheusConfig)
	}
	tsdb := ""
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.EmptyDir != nil {
			tsdb = mMountPath(t, m.File, c, v.Name)
		}
	}
	if a := "--storage.tsdb.path=" + tsdb; tsdb == "" || !args[a] {
		t.Errorf("%s: args %v do not keep the TSDB on the emptyDir (mounted at %q)", m.File, c.Args, tsdb)
	}
	if p := c.ReadinessProbe; p == nil || p.HTTPGet == nil || p.HTTPGet.Path != "/-/ready" {
		t.Errorf("%s: readiness probe %+v, want HTTP GET /-/ready", m.File, p)
	} else if port, ok := mPort(c, p.HTTPGet.Port); !ok || port != mPrometheusPort {
		t.Errorf("%s: readiness probe on %s, want container port %d", m.File, p.HTTPGet.Port.String(), mPrometheusPort)
	}
	if sp, ok := mServicePort(svc, mPrometheusPort); !ok {
		t.Errorf("%s: Service %s has no port %d", m.File, mPrometheusService, mPrometheusPort)
	} else if port, _ := mTarget(c, sp); port != mPrometheusPort {
		t.Errorf("%s: Service %s port %d reaches container port %d, want %d", m.File, mPrometheusService, mPrometheusPort, port, mPrometheusPort)
	}

	sa := d.Spec.Template.Spec.ServiceAccountName
	if sa == "" {
		t.Fatalf("%s: the Deployment runs as the default ServiceAccount", m.File)
	}
	found := false
	for _, a := range m.ServiceAccounts {
		found = found || (a.Name == sa && a.Namespace == mNamespace)
	}
	if !found {
		t.Errorf("%s: ServiceAccount %s/%s is not in the file", m.File, mNamespace, sa)
	}
	var rules []rbacv1.PolicyRule
	for _, b := range m.ClusterRoleBindings {
		bound := false
		for _, s := range b.Subjects {
			bound = bound || (s.Kind == rbacv1.ServiceAccountKind && s.Name == sa && s.Namespace == mNamespace)
		}
		if !bound || b.RoleRef.Kind != "ClusterRole" {
			continue
		}
		for _, r := range m.ClusterRoles {
			if r.Name == b.RoleRef.Name {
				rules = append(rules, r.Rules...)
			}
		}
	}
	for _, need := range []struct{ group, resource string }{
		{"", "nodes"}, {"", "services"}, {"", "endpoints"}, {"", "pods"}, {"discovery.k8s.io", "endpointslices"},
	} {
		for _, verb := range []string{"get", "list", "watch"} {
			if !mRuleAllows(rules, need.group, need.resource, verb) {
				t.Errorf("%s: ServiceAccount %s may not %s %s in group %q", m.File, sa, verb, need.resource, need.group)
			}
		}
	}
}

// TestM06OtelcolManifest checks otelcol.yaml: the Collector by the
// OTELCOL_IMAGE tag in the monitoring namespace, its configuration from the
// generated ConfigMap, and a Service on the OTLP/HTTP and prometheus
// exporter ports. The file creates no Namespace, so deleting it when leaving
// the otel-collector mode leaves Prometheus in place.
func TestM06OtelcolManifest(t *testing.T) {
	m := mDecodeTyped(t, mOtelcolFile)
	for _, d := range m.Docs {
		if d.Kind == "Namespace" || d.Kind == "ConfigMap" {
			t.Errorf("%s: %s belongs to prometheus.yaml or to the generated configuration", m.File, d)
		}
	}
	d := m.deployment(t, "otelcol")
	svc := m.service(t, mOtelcolService)
	c, configDir := mCheckWorkload(t, m.File, d, svc, mVersion(t, mVersions(t), "OTELCOL_IMAGE"), mOtelcolConfig)
	if a := "--config=" + path.Join(configDir, mOtelcolFileName); len(c.Args) != 1 || c.Args[0] != a {
		t.Errorf("%s: args %v, want [%s], the key of ConfigMap %s", m.File, c.Args, a, mOtelcolConfig)
	}
	var ports []int32
	for _, p := range svc.Spec.Ports {
		ports = append(ports, p.Port)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	if fmt.Sprint(ports) != fmt.Sprint(mOtelcolPorts) {
		t.Errorf("%s: Service %s ports %v, want %v", m.File, mOtelcolService, ports, mOtelcolPorts)
	}
	for _, want := range mOtelcolPorts {
		if sp, ok := mServicePort(svc, want); ok {
			if port, _ := mTarget(c, sp); port != want {
				t.Errorf("%s: Service %s port %d reaches container port %d, want %d", m.File, mOtelcolService, want, port, want)
			}
		}
	}
	if p := c.ReadinessProbe; p == nil || p.TCPSocket == nil {
		t.Errorf("%s: readiness probe %+v, want a TCP check of the OTLP/HTTP receiver", m.File, p)
	} else if port, ok := mPort(c, p.TCPSocket.Port); !ok || port != mOtelcolPorts[0] {
		t.Errorf("%s: readiness probe on %s, want container port %d", m.File, p.TCPSocket.Port.String(), mOtelcolPorts[0])
	}
}

// TestM07WorkflowPathsListTheSchedulerConfig checks that a change to the
// shared scheduler configuration runs the Dagger integration workflow, on
// pull requests and on pushes alike.
func TestM07WorkflowPathsListTheSchedulerConfig(t *testing.T) {
	var wf map[string]interface{}
	if err := yaml.Unmarshal(mRead(t, mWorkflowFile), &wf); err != nil {
		t.Fatalf("%s: %v", mWorkflowFile, err)
	}
	// YAML 1.1, which sigs.k8s.io/yaml follows, reads the key on as true.
	on, ok := wf["on"].(map[string]interface{})
	if !ok {
		on, ok = wf["true"].(map[string]interface{})
	}
	if !ok {
		t.Fatalf("%s has no on: section", mWorkflowFile)
	}
	for _, event := range []string{"pull_request", "push"} {
		trigger, _ := on[event].(map[string]interface{})
		paths, _ := trigger["paths"].([]interface{})
		listed := false
		for _, p := range paths {
			listed = listed || p == mSchedulerConfig
		}
		if !listed {
			t.Errorf("%s: on.%s.paths %v lack %s", mWorkflowFile, event, paths, mSchedulerConfig)
		}
	}
}

// TestM08SchedulerConfigIsIgnorable checks that every extender in the
// scheduler configurations is ignorable: the shared one, whose extender
// listens on a host port and rolls out with Recreate (D17), so for a moment
// no extender answers, and the one of SIMCLUSTER_EXTENDER=service, whose
// extender rolls over between pods. With ignorable false, kube-scheduler
// fails every pod in that window instead of scheduling without it.
func TestM08SchedulerConfigIsIgnorable(t *testing.T) {
	for _, file := range []string{mSchedulerConfig, mSchedulerConfigService} {
		cfg := mSchedulerConfiguration(t, file)
		if len(cfg.Extenders) == 0 {
			t.Fatalf("%s registers no extender", file)
		}
		for _, e := range cfg.Extenders {
			if e.Ignorable == nil || !*e.Ignorable {
				t.Errorf("%s: extender %s is not ignorable", file, e.URLPrefix)
			}
		}
	}
}

// mSchedulerConfigDoc is the part of a KubeSchedulerConfiguration the tests
// read.
type mSchedulerConfigDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Extenders  []struct {
		URLPrefix string `json:"urlPrefix"`
		Ignorable *bool  `json:"ignorable"`
	} `json:"extenders"`
}

// mSchedulerConfiguration reads a scheduler configuration, which contains
// ignorable: true literally so a grep finds it, and checks its kind.
func mSchedulerConfiguration(t *testing.T, file string) mSchedulerConfigDoc {
	t.Helper()
	b := mRead(t, file)
	if !bytes.Contains(b, []byte("ignorable: true")) {
		t.Errorf("%s does not contain ignorable: true", file)
	}
	var cfg mSchedulerConfigDoc
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if cfg.APIVersion != "kubescheduler.config.k8s.io/v1" || cfg.Kind != "KubeSchedulerConfiguration" {
		t.Errorf("%s: apiVersion %q kind %q, want kubescheduler.config.k8s.io/v1 KubeSchedulerConfiguration", file, cfg.APIVersion, cfg.Kind)
	}
	return cfg
}

// TestM09OtelcolBridgeManifest checks otelcol-bridge.yaml: one replica of
// the OTELCOL_CONTRIB_IMAGE tag in the monitoring namespace, replaced with
// Recreate on the infra Node, reading the example's collector.yaml and then
// the generated config.yaml from ConfigMap otelcol-bridge-config, ready
// once its health check answers. No Service and no Namespace: nothing
// connects to the bridge, and deleting the file when leaving the mode must
// leave Prometheus in place.
func TestM09OtelcolBridgeManifest(t *testing.T) {
	m := mDecodeTyped(t, mOtelcolBridgeFile)
	for _, d := range m.Docs {
		if d.Kind != "Deployment" {
			t.Errorf("%s: %s; the file holds the bridge Deployment only", m.File, d)
		}
	}
	d := m.deployment(t, "otelcol-bridge")
	at := m.File + ": Deployment " + d.Name
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Errorf("%s: replicas %v strategy %q, want 1 and Recreate: every replica scrapes every exporter", at, d.Spec.Replicas, d.Spec.Strategy.Type)
	}
	pod := d.Spec.Template.Spec
	if got := pod.NodeSelector; len(got) != 1 || got[mInfraLabel] != "true" {
		t.Errorf("%s: nodeSelector %v, want only %s: \"true\"", at, got, mInfraLabel)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("%s: %d containers, want 1", at, len(pod.Containers))
	}
	c := &pod.Containers[0]
	if want := mVersion(t, mVersions(t), "OTELCOL_CONTRIB_IMAGE"); c.Image != want || c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("%s: image %q pull policy %q, want %q from %s and IfNotPresent", at, c.Image, c.ImagePullPolicy, want, mVersionsFile)
	}
	volume := ""
	for _, v := range pod.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name == "otelcol-bridge-config" {
			volume = v.Name
		}
	}
	if volume == "" {
		t.Fatalf("%s: no volume from ConfigMap otelcol-bridge-config", at)
	}
	dir := mMountPath(t, at, c, volume)
	if want := []string{"--config=" + path.Join(dir, "collector.yaml"), "--config=" + path.Join(dir, mOtelcolFileName)}; fmt.Sprint(c.Args) != fmt.Sprint(want) {
		t.Errorf("%s: args %v, want %v: the example first, then the generated file", at, c.Args, want)
	}
	if p := c.ReadinessProbe; p == nil || p.HTTPGet == nil {
		t.Errorf("%s: readiness probe %+v, want an HTTP check of the health_check extension", at, p)
	} else if port, ok := mPort(c, p.HTTPGet.Port); !ok || port != 13133 {
		t.Errorf("%s: readiness probe on %s, want container port 13133", at, p.HTTPGet.Port.String())
	}
}

// TestM10ExtenderServiceVariant checks SIMCLUSTER_EXTENDER=service: its
// values take the extender off the host network and give its Service a
// fixed ClusterIP inside k3s' default service CIDR, 10.43.0.0/16, and the
// scheduler configuration of the variant calls that address on the
// extender's port, the chart's schedulerExtender.port.
func TestM10ExtenderServiceVariant(t *testing.T) {
	var values struct {
		SchedulerExtender struct {
			HostNetwork *bool  `json:"hostNetwork"`
			ClusterIP   string `json:"clusterIP"`
		} `json:"schedulerExtender"`
	}
	if err := yaml.UnmarshalStrict(mRead(t, mExtenderServiceValues), &values); err != nil {
		t.Fatalf("%s: %v", mExtenderServiceValues, err)
	}
	var chart struct {
		SchedulerExtender struct {
			Port int `json:"port"`
		} `json:"schedulerExtender"`
	}
	if err := yaml.Unmarshal(mRead(t, "charts/joulie/values.yaml"), &chart); err != nil {
		t.Fatal(err)
	}
	ext := values.SchedulerExtender
	if ext.HostNetwork == nil || *ext.HostNetwork {
		t.Errorf("%s: schedulerExtender.hostNetwork %v, want false", mExtenderServiceValues, ext.HostNetwork)
	}
	if !strings.HasPrefix(ext.ClusterIP, "10.43.") {
		t.Errorf("%s: schedulerExtender.clusterIP %q is not in k3s' service CIDR 10.43.0.0/16", mExtenderServiceValues, ext.ClusterIP)
	}
	cfg := mSchedulerConfiguration(t, mSchedulerConfigService)
	want := fmt.Sprintf("http://%s:%d", ext.ClusterIP, chart.SchedulerExtender.Port)
	if len(cfg.Extenders) != 1 || cfg.Extenders[0].URLPrefix != want {
		t.Errorf("%s: extenders %+v, want one at %s", mSchedulerConfigService, cfg.Extenders, want)
	}
}
