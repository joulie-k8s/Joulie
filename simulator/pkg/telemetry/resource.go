package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
)

// Resource identity of everything the simulator exports.
const (
	// DefaultServiceName is service.name without OTEL_SERVICE_NAME.
	DefaultServiceName = "joulie-simulator"
	// ServiceNamespace is service.namespace. With service.name it makes
	// Prometheus' job label: joulie/joulie-simulator.
	ServiceNamespace = "joulie"
	// FacilityInstance is the service.instance.id of the facility resource.
	FacilityInstance = "facility"
	// ScopeName is the instrumentation scope of every export.
	ScopeName = "github.com/matbun/joulie/simulator/pkg/telemetry"
	// HostIDPrefix starts the host.id of an emulated node, followed by the
	// first 32 hex digits of the sha256 of its name.
	HostIDPrefix = "hwemu-"
)

// Resource is what the simulator's resources share: the service name and
// version, the simulator's own instance id, and the attributes of
// OTEL_RESOURCE_ATTRIBUTES. Node, Facility and Simulator build the
// resources from it.
type Resource struct {
	ServiceName    string
	ServiceVersion string
	// Instance is the simulator's own service.instance.id: POD_NAME, else
	// the hostname.
	Instance string
	// Attributes come from OTEL_RESOURCE_ATTRIBUTES, sorted by key, with
	// service.name and the per-node identity attributes removed. They
	// replace the simulator's value of any other attribute.
	Attributes []otlpjson.KeyValue
}

// telemetryIdentity are the attributes OTEL_RESOURCE_ATTRIBUTES never sets:
// the identity of a node's resource. One value for every resource would
// merge every node into one series.
var telemetryIdentity = map[string]bool{
	semconv.AttrServiceInstanceID: true,
	semconv.AttrK8sNodeName:       true,
	semconv.AttrHostName:          true,
	semconv.AttrHostID:            true,
}

// ResourceFromEnv reads OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES and
// POD_NAME through getenv, normally os.Getenv. service.name is
// OTEL_SERVICE_NAME, else the service.name of OTEL_RESOURCE_ATTRIBUTES,
// else DefaultServiceName, as the OTel SDK specification orders them. A
// malformed OTEL_RESOURCE_ATTRIBUTES is discarded whole, as the
// specification asks, and reported in the error; the Resource is usable
// either way. service.version is the simulator's build version.
func ResourceFromEnv(getenv func(string) string) (Resource, error) {
	r := Resource{ServiceVersion: telemetryBuildVersion()}
	var err error
	envName := ""
	if v := strings.TrimSpace(getenv("OTEL_RESOURCE_ATTRIBUTES")); v != "" {
		kvs, perr := otlpjson.ParseKeyValueList(v)
		if perr != nil {
			err = fmt.Errorf("telemetry: OTEL_RESOURCE_ATTRIBUTES ignored: %w", perr)
		}
		last := map[string]string{}
		for _, kv := range kvs {
			last[kv.Key] = *kv.Value.String
		}
		for k, v := range last {
			switch {
			case k == semconv.AttrServiceName:
				envName = v
			case telemetryIdentity[k]:
			default:
				r.Attributes = append(r.Attributes, otlpjson.String(k, v))
			}
		}
		sort.Slice(r.Attributes, func(i, j int) bool { return r.Attributes[i].Key < r.Attributes[j].Key })
	}
	r.ServiceName = strings.TrimSpace(getenv("OTEL_SERVICE_NAME"))
	if r.ServiceName == "" {
		r.ServiceName = envName
	}
	if r.ServiceName == "" {
		r.ServiceName = DefaultServiceName
	}
	r.Instance = strings.TrimSpace(getenv(EnvPodName))
	if r.Instance == "" {
		r.Instance, _ = os.Hostname()
	}
	if r.Instance == "" {
		r.Instance = DefaultServiceName
	}
	return r, err
}

// Node is the resource of emulated node node in cluster: service.instance.id,
// k8s.node.name and host.name are the node name, and host.id is derived
// from it.
func (r Resource) Node(node, cluster string) otlpjson.Resource {
	return r.build(node, cluster,
		otlpjson.String(semconv.AttrK8sNodeName, node),
		otlpjson.String(semconv.AttrHostName, node),
		otlpjson.String(semconv.AttrHostID, HostID(node)),
	)
}

// Facility is the resource of the facility of cluster.
func (r Resource) Facility(cluster string) otlpjson.Resource {
	return r.build(FacilityInstance, cluster)
}

// Simulator is the simulator's own resource, for events about no node.
func (r Resource) Simulator(cluster string) otlpjson.Resource {
	return r.build(r.Instance, cluster)
}

// build returns the shared attributes, the instance id, cluster and extra
// identity attributes, then the OTEL_RESOURCE_ATTRIBUTES not already set,
// whose values replace the simulator's for the attributes they share.
func (r Resource) build(instance, cluster string, identity ...otlpjson.KeyValue) otlpjson.Resource {
	name := r.ServiceName
	if name == "" {
		name = DefaultServiceName
	}
	attrs := []otlpjson.KeyValue{
		otlpjson.String(semconv.AttrServiceName, name),
		otlpjson.String(semconv.AttrServiceNamespace, ServiceNamespace),
	}
	if r.ServiceVersion != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrServiceVersion, r.ServiceVersion))
	}
	attrs = append(attrs, otlpjson.String(semconv.AttrServiceInstanceID, instance))
	if cluster != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrK8sClusterName, cluster))
	}
	attrs = append(attrs, identity...)
	for _, kv := range r.Attributes {
		if telemetryIdentity[kv.Key] || kv.Key == semconv.AttrServiceName {
			continue
		}
		replaced := false
		for i := range attrs {
			if attrs[i].Key == kv.Key {
				attrs[i] = kv
				replaced = true
			}
		}
		if !replaced {
			attrs = append(attrs, kv)
		}
	}
	return otlpjson.Resource{Attributes: attrs}
}

// HostID is the host.id of emulated node node: HostIDPrefix and the first
// 32 hex digits of the sha256 of its name, stable across restarts and
// distinct per node.
func HostID(node string) string {
	sum := sha256.Sum256([]byte(node))
	return HostIDPrefix + hex.EncodeToString(sum[:])[:32]
}

// telemetryBuildVersion is the main module's version, else its VCS revision
// (12 digits, -dirty when modified), else empty.
func telemetryBuildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return ""
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
