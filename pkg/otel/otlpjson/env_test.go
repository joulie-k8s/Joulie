package otlpjson

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func envTestGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// OJ04: ConfigFromEnv uses a per-signal endpoint as it is and appends
// v1/<signal> to a base endpoint; http/protobuf and grpc fail and leave the
// signal without an endpoint; headers, timeout, compression and the export
// interval parse, per signal before general; a bad general variable is
// reported once; enum values are case-insensitive, and an unsupported
// exporter is a warning, not an error, and is ignored; OTEL_SDK_DISABLED
// and the none exporter turn signals off; no endpoint means no push.
func TestOJ04ConfigFromEnv(t *testing.T) {
	c, warnings, err := ConfigFromEnv(envTestGetenv(nil))
	if err != nil || warnings != nil {
		t.Fatal(warnings, err)
	}
	if c.Metrics.Endpoint != "" || c.Logs.Endpoint != "" || c.ExportInterval != DefaultExportInterval {
		t.Errorf("empty environment gave %+v, want no endpoint and a 60 s interval", c)
	}

	for _, tc := range []struct{ base, metrics, logs string }{
		{"http://collector:4318", "http://collector:4318/v1/metrics", "http://collector:4318/v1/logs"},
		{"http://collector:4318/", "http://collector:4318/v1/metrics", "http://collector:4318/v1/logs"},
		{"http://prometheus:9090/api/v1/otlp", "http://prometheus:9090/api/v1/otlp/v1/metrics", "http://prometheus:9090/api/v1/otlp/v1/logs"},
	} {
		c, _, err := ConfigFromEnv(envTestGetenv(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": tc.base}))
		if err != nil {
			t.Fatal(err)
		}
		if c.Metrics.Endpoint != tc.metrics || c.Logs.Endpoint != tc.logs {
			t.Errorf("base %s gave metrics %s, logs %s; want %s, %s", tc.base, c.Metrics.Endpoint, c.Logs.Endpoint, tc.metrics, tc.logs)
		}
	}

	c, _, err = ConfigFromEnv(envTestGetenv(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":            "http://collector:4318",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT":    "http://prometheus:9090/api/v1/otlp/v1/metrics",
		"OTEL_EXPORTER_OTLP_HEADERS":             "authorization=Bearer%20abc, x-tenant = a",
		"OTEL_EXPORTER_OTLP_LOGS_HEADERS":        "x-tenant=logs",
		"OTEL_EXPORTER_OTLP_TIMEOUT":             "2500",
		"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT":        "500",
		"OTEL_EXPORTER_OTLP_METRICS_COMPRESSION": "gzip",
		"OTEL_EXPORTER_OTLP_COMPRESSION":         "none",
		"OTEL_EXPORTER_OTLP_PROTOCOL":            "http/json",
		"OTEL_METRIC_EXPORT_INTERVAL":            "5000",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := ExporterConfig{
		Metrics: SignalConfig{
			Endpoint: "http://prometheus:9090/api/v1/otlp/v1/metrics",
			Headers:  map[string]string{"authorization": "Bearer abc", "x-tenant": "a"},
			Timeout:  2500 * time.Millisecond, Compression: "gzip",
		},
		Logs: SignalConfig{
			Endpoint: "http://collector:4318/v1/logs",
			Headers:  map[string]string{"x-tenant": "logs"},
			Timeout:  500 * time.Millisecond,
		},
		ExportInterval: 5 * time.Second,
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("ConfigFromEnv =\n%+v\nwant\n%+v", c, want)
	}

	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf"}, "OTEL_EXPORTER_OTLP_PROTOCOL"},
		{map[string]string{"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "grpc"}, "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"},
		{map[string]string{"OTEL_EXPORTER_OTLP_TIMEOUT": "10s"}, "OTEL_EXPORTER_OTLP_TIMEOUT"},
		{map[string]string{"OTEL_EXPORTER_OTLP_LOGS_TIMEOUT": "-1"}, "OTEL_EXPORTER_OTLP_LOGS_TIMEOUT"},
		{map[string]string{"OTEL_EXPORTER_OTLP_COMPRESSION": "zstd"}, "OTEL_EXPORTER_OTLP_COMPRESSION"},
		{map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "novalue"}, "OTEL_EXPORTER_OTLP_HEADERS"},
		{map[string]string{"OTEL_EXPORTER_OTLP_METRICS_HEADERS": "k=%zz"}, "OTEL_EXPORTER_OTLP_METRICS_HEADERS"},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4318"}, "OTEL_EXPORTER_OTLP_ENDPOINT"},
		{map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "ftp://collector"}, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"},
		{map[string]string{"OTEL_METRIC_EXPORT_INTERVAL": "0"}, "OTEL_METRIC_EXPORT_INTERVAL"},
	} {
		_, warnings, err := ConfigFromEnv(envTestGetenv(tc.env))
		if err == nil || !strings.Contains(err.Error(), tc.want) || warnings != nil {
			t.Errorf("%v: err = %v, warnings %q; want an error naming %s and no warning", tc.env, err, warnings, tc.want)
		}
	}
	// A refused protocol leaves its signal without an endpoint, so OTLP JSON
	// never reaches a gRPC port, and a general variable that both signals
	// read is reported once.
	for _, tc := range []struct {
		env               map[string]string
		metricsOn, logsOn bool
	}{
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, false, false},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/protobuf"}, false, true},
		{map[string]string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://collector:4317/v1/logs", "OTEL_EXPORTER_OTLP_LOGS_PROTOCOL": "grpc"}, false, false},
	} {
		c, _, err := ConfigFromEnv(envTestGetenv(tc.env))
		if err == nil || strings.Count(err.Error(), "PROTOCOL") != 1 {
			t.Errorf("%v: err = %v, want the protocol reported once", tc.env, err)
		}
		if (c.Metrics.Endpoint != "") != tc.metricsOn || (c.Logs.Endpoint != "") != tc.logsOn {
			t.Errorf("%v gave metrics %q, logs %q; want metrics on %v, logs on %v", tc.env, c.Metrics.Endpoint, c.Logs.Endpoint, tc.metricsOn, tc.logsOn)
		}
	}
	for _, env := range []map[string]string{
		{"OTEL_EXPORTER_OTLP_ENDPOINT": "collector:4318"},
		{"OTEL_EXPORTER_OTLP_TIMEOUT": "10s"},
		{"OTEL_EXPORTER_OTLP_HEADERS": "novalue"},
		{"OTEL_EXPORTER_OTLP_COMPRESSION": "zstd"},
	} {
		if _, _, err := ConfigFromEnv(envTestGetenv(env)); err == nil || strings.Count(err.Error(), "otlpjson:") != 1 {
			t.Errorf("%v: err = %v, want one error for both signals", env, err)
		}
	}
	// Two bad variables are both reported.
	_, _, err = ConfigFromEnv(envTestGetenv(map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_METRIC_EXPORT_INTERVAL": "x"}))
	if err == nil || !strings.Contains(err.Error(), "PROTOCOL") || !strings.Contains(err.Error(), "INTERVAL") {
		t.Errorf("err = %v, want both variables named", err)
	}

	// Enum values are read without regard to case
	// (configuration/sdk-environment-variables.md, "Enum").
	c, warnings, err = ConfigFromEnv(envTestGetenv(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT":    "http://collector:4318",
		"OTEL_EXPORTER_OTLP_PROTOCOL":    "HTTP/JSON",
		"OTEL_EXPORTER_OTLP_COMPRESSION": "GZIP",
		"OTEL_METRICS_EXPORTER":          "OTLP",
		"OTEL_LOGS_EXPORTER":             "None",
	}))
	if err != nil || warnings != nil || c.Metrics.Endpoint != "http://collector:4318/v1/metrics" || c.Logs.Endpoint != "" || c.Metrics.Compression != "gzip" || c.Logs.Compression != "gzip" {
		t.Errorf("upper-case enums gave %+v, %q, %v; want metrics on with gzip, logs off, no warning or error", c, warnings, err)
	}
	// An exporter Joulie does not implement is a warning and is ignored, as
	// the specification asks of an enum value it does not recognise: the
	// signal keeps the otlp default unless another item says none. It is
	// not an error, so a caller that treats errors as fatal still starts.
	for _, tc := range []struct {
		exporter string
		on       bool
	}{{"prometheus", true}, {"console", true}, {"Otlp/Stdout", true}, {"console,none", false}, {"zipkin,OTLP", true}} {
		c, warnings, err := ConfigFromEnv(envTestGetenv(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_METRICS_EXPORTER": tc.exporter}))
		if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "OTEL_METRICS_EXPORTER") || !strings.Contains(warnings[0], "ignored") {
			t.Errorf("OTEL_METRICS_EXPORTER=%q: warnings %q, err = %v; want one warning that it is ignored and no error", tc.exporter, warnings, err)
		}
		if on := c.Metrics.Endpoint != ""; on != tc.on || c.Logs.Endpoint == "" {
			t.Errorf("OTEL_METRICS_EXPORTER=%q gave metrics %q, logs %q; want metrics on %v and logs on", tc.exporter, c.Metrics.Endpoint, c.Logs.Endpoint, tc.on)
		}
	}

	full := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}
	for _, v := range []string{"true", "TRUE", " True "} {
		full["OTEL_SDK_DISABLED"] = v
		c, _, err := ConfigFromEnv(envTestGetenv(full))
		if err != nil || c.Metrics.Endpoint != "" || c.Logs.Endpoint != "" {
			t.Errorf("OTEL_SDK_DISABLED=%q gave %+v, %v; want both signals off and no error", v, c, err)
		}
	}
	full["OTEL_SDK_DISABLED"] = "false"
	full["OTEL_EXPORTER_OTLP_PROTOCOL"] = ""
	full["OTEL_LOGS_EXPORTER"] = "none"
	full["OTEL_METRICS_EXPORTER"] = "otlp"
	c, _, err = ConfigFromEnv(envTestGetenv(full))
	if err != nil || c.Metrics.Endpoint == "" || c.Logs.Endpoint != "" {
		t.Errorf("OTEL_LOGS_EXPORTER=none gave %+v, %v; want metrics on and logs off", c, err)
	}
}

// ParseKeyValueList keeps order and repeats, trims, percent-decodes and
// skips empty items.
func TestParseKeyValueList(t *testing.T) {
	kvs, err := ParseKeyValueList(" a=1 ,, b = x%2Cy ,a=2,c=")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, kv := range kvs {
		got = append(got, kv.Key+"="+*kv.Value.String)
	}
	if want := []string{"a=1", "b=x,y", "a=2", "c="}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	for _, bad := range []string{"a", "=1", "a=%"} {
		if _, err := ParseKeyValueList(bad); err == nil {
			t.Errorf("ParseKeyValueList(%q) accepted it", bad)
		}
	}
}
