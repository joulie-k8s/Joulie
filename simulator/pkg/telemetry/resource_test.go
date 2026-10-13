package telemetry

import (
	"os"
	"testing"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

func testResourceAttrs(kv ...string) []otlpjson.KeyValue {
	var out []otlpjson.KeyValue
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, otlpjson.String(kv[i], kv[i+1]))
	}
	return out
}

func testGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TM08: OTEL_RESOURCE_ATTRIBUTES is merged into every resource but never
// sets service.instance.id, k8s.node.name, host.name or host.id; it may
// replace other attributes, such as k8s.cluster.name. service.name follows
// OTEL_SERVICE_NAME, then the attribute, then the default; a malformed list
// is discarded whole and reported.
func TestTM08ResourceAttributesCannotOverrideIdentity(t *testing.T) {
	env := map[string]string{
		"OTEL_RESOURCE_ATTRIBUTES": "service.instance.id=x, k8s.node.name=y,host.name=z,host.id=w," +
			"deployment.environment.name=lab,k8s.cluster.name=other,service.name=from-attrs,team=a%2Cb",
		EnvPodName: "joulie-simulator-7d9f",
	}
	r, err := ResourceFromEnv(testGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if r.ServiceName != "from-attrs" || r.Instance != "joulie-simulator-7d9f" {
		t.Errorf("ResourceFromEnv = %+v", r)
	}
	a := testAttrs(r.Node("intel-0", "smoke").Attributes)
	for k, v := range map[string]string{
		semconv.AttrServiceInstanceID: "intel-0", semconv.AttrK8sNodeName: "intel-0", semconv.AttrHostName: "intel-0",
		semconv.AttrHostID: HostID("intel-0"), semconv.AttrServiceName: "from-attrs", semconv.AttrServiceNamespace: ServiceNamespace,
		semconv.AttrK8sClusterName: "other", "deployment.environment.name": "lab", "team": "a,b",
	} {
		if a[k] != v {
			t.Errorf("node resource %s = %q, want %q", k, a[k], v)
		}
	}
	for _, kv := range r.Attributes {
		if telemetryIdentity[kv.Key] || kv.Key == semconv.AttrServiceName {
			t.Errorf("ResourceFromEnv kept %s in Attributes", kv.Key)
		}
	}
	// A Resource built by hand cannot set the identity either.
	hand := Resource{Attributes: testResourceAttrs(semconv.AttrServiceInstanceID, "x", semconv.AttrK8sNodeName, "y", semconv.AttrHostName, "z", semconv.AttrHostID, "w")}
	if a := testAttrs(hand.Node("intel-0", "smoke").Attributes); a[semconv.AttrServiceInstanceID] != "intel-0" || a[semconv.AttrK8sNodeName] != "intel-0" ||
		a[semconv.AttrHostName] != "intel-0" || a[semconv.AttrHostID] != HostID("intel-0") {
		t.Errorf("a hand-built Resource overrode the identity: %v", a)
	}
	fa := testAttrs(r.Facility("smoke").Attributes)
	if fa[semconv.AttrServiceInstanceID] != FacilityInstance || fa[semconv.AttrK8sNodeName] != "" || fa[semconv.AttrHostName] != "" || fa["team"] != "a,b" {
		t.Errorf("facility resource %v", fa)
	}
	if sa := testAttrs(r.Simulator("smoke").Attributes); sa[semconv.AttrServiceInstanceID] != "joulie-simulator-7d9f" {
		t.Errorf("simulator resource %v", sa)
	}

	env["OTEL_SERVICE_NAME"] = "sim-a"
	if r, _ := ResourceFromEnv(testGetenv(env)); r.ServiceName != "sim-a" {
		t.Errorf("OTEL_SERVICE_NAME gave service.name %q", r.ServiceName)
	}
	r, err = ResourceFromEnv(testGetenv(map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "team=a,broken"}))
	if err == nil || len(r.Attributes) != 0 || r.ServiceName != DefaultServiceName {
		t.Errorf("malformed attributes gave %+v, %v; want them discarded and an error", r, err)
	}
	r, _ = ResourceFromEnv(testGetenv(nil))
	if host, _ := os.Hostname(); host != "" && r.Instance != host {
		t.Errorf("Instance %q without POD_NAME, want the hostname %q", r.Instance, host)
	}
	if a := testAttrs(Resource{}.Node("n", "").Attributes); a[semconv.AttrServiceName] != DefaultServiceName || a[semconv.AttrK8sClusterName] != "" {
		t.Errorf("zero Resource gives %v", a)
	}
}

// ConfigFromEnv: the files default to the debug persistence directory, an
// explicitly empty SIM_OTEL_FILE_DIR turns them off, the OTLP variables
// reach the exporter configuration, and otlpjson's warnings come back
// apart from the errors.
func TestConfigFromEnvFiles(t *testing.T) {
	lookup := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	c, warnings, err := ConfigFromEnv(lookup(map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://p:9090/api/v1/otlp/v1/metrics"}), "/tmp/run")
	if err != nil || warnings != nil || c.FileDir != "/tmp/run" || c.FileMetrics || c.Exporter.Metrics.Endpoint == "" || c.Exporter.Logs.Endpoint != "" {
		t.Errorf("ConfigFromEnv = %+v, %v", c, err)
	}
	c, _, err = ConfigFromEnv(lookup(map[string]string{EnvFileDir: "", EnvFileMetrics: "true"}), "/tmp/run")
	if err != nil || c.FileDir != "" || !c.FileMetrics {
		t.Errorf("empty SIM_OTEL_FILE_DIR gave %+v, %v", c, err)
	}
	if _, _, err := ConfigFromEnv(lookup(map[string]string{EnvFileMetrics: "maybe", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}), ""); err == nil {
		t.Error("ConfigFromEnv accepted SIM_OTEL_FILE_METRICS=maybe and grpc")
	}
	c, warnings, err = ConfigFromEnv(lookup(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://p:4318", "OTEL_METRICS_EXPORTER": "prometheus"}), "")
	if err != nil || len(warnings) != 1 || c.Exporter.Metrics.Endpoint == "" {
		t.Errorf("OTEL_METRICS_EXPORTER=prometheus gave %+v, warnings %q, err %v; want metrics on, one warning, no error", c, warnings, err)
	}
}
