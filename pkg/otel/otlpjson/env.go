package otlpjson

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Signal is an OTLP signal Joulie exports. Its value is the last element of
// the OTLP/HTTP path, v1/<signal>.
type Signal string

const (
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
)

// Defaults of the OTel specification v1.61.0: OTEL_METRIC_EXPORT_INTERVAL
// (configuration/sdk-environment-variables.md) and OTEL_EXPORTER_OTLP_TIMEOUT
// (protocol/exporter.md).
const (
	DefaultExportInterval = 60 * time.Second
	DefaultTimeout        = 10 * time.Second
)

// SignalConfig is where and how one signal is exported. An empty Endpoint
// means the signal is not pushed. Timeout bounds one export, retries
// included; zero means DefaultTimeout. Compression is "gzip" or empty.
type SignalConfig struct {
	Endpoint    string
	Headers     map[string]string
	Timeout     time.Duration
	Compression string
}

// ExporterConfig configures an Exporter. ExportInterval is how often a
// periodic reader exports metrics; the Exporter itself does not use it.
// A nil HTTPClient means a client of the Exporter's own.
type ExporterConfig struct {
	Metrics, Logs  SignalConfig
	ExportInterval time.Duration
	HTTPClient     *http.Client
}

// Signal returns the configuration of signal s.
func (c ExporterConfig) Signal(s Signal) SignalConfig {
	if s == SignalLogs {
		return c.Logs
	}
	return c.Metrics
}

// ConfigFromEnv reads the OTLP exporter environment variables of the OTel
// specification v1.61.0 (protocol/exporter.md:26-27 and the SDK variables)
// through getenv, normally os.Getenv:
//
//   - OTEL_EXPORTER_OTLP_<SIGNAL>_ENDPOINT is used as it is;
//     OTEL_EXPORTER_OTLP_ENDPOINT is a base URL that gets v1/metrics or
//     v1/logs appended to its path.
//   - _HEADERS (key=value pairs, percent-decoded), _TIMEOUT (milliseconds),
//     _COMPRESSION (gzip or none) and _PROTOCOL, each with a per-signal form
//     that takes precedence over the general one.
//   - OTEL_METRIC_EXPORT_INTERVAL in milliseconds.
//   - OTEL_METRICS_EXPORTER and OTEL_LOGS_EXPORTER: otlp (the default) or
//     none, which turns the signal off.
//   - OTEL_SDK_DISABLED=true turns both signals off; nothing else is read.
//
// Enum values (protocol, compression, exporter) are read without regard to
// case. An exporter Joulie does not implement, such as prometheus or
// console, is a warning and is ignored, so the signal keeps the otlp
// default (configuration/sdk-environment-variables.md, "Enum"). Joulie
// deviates from the specification in two documented ways: the only
// protocol is http/json, so http/protobuf and grpc are errors rather than
// the default, and a signal with no endpoint is not pushed rather than sent
// to http://localhost:4318.
//
// Every other invalid variable is an error, and all of them are joined in
// err. The configuration returned with an error leaves each invalid value
// out, and a signal whose protocol is refused has no endpoint, so nothing
// is sent to a receiver that cannot read it. A general variable is read for
// both signals but reported once. Callers log the warnings and treat a
// non-nil err as fatal.
func ConfigFromEnv(getenv func(string) string) (c ExporterConfig, warnings []string, err error) {
	c = ExporterConfig{ExportInterval: DefaultExportInterval}
	if strings.EqualFold(strings.TrimSpace(getenv("OTEL_SDK_DISABLED")), "true") {
		return c, nil, nil
	}
	var r envReport
	if v := strings.TrimSpace(getenv("OTEL_METRIC_EXPORT_INTERVAL")); v != "" {
		d, err := envMillis("OTEL_METRIC_EXPORT_INTERVAL", v)
		if err != nil {
			r.fail(err)
		} else {
			c.ExportInterval = d
		}
	}
	c.Metrics = envSignal(getenv, SignalMetrics, &r)
	c.Logs = envSignal(getenv, SignalLogs, &r)
	return c, r.warnings, errors.Join(r.errs...)
}

// envReport collects the warnings and errors of ConfigFromEnv. A message
// already reported is dropped: the general variables are read once per
// signal and would otherwise be reported twice.
type envReport struct {
	seen     map[string]bool
	warnings []string
	errs     []error
}

func (r *envReport) first(msg string) bool {
	if r.seen[msg] {
		return false
	}
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	r.seen[msg] = true
	return true
}

func (r *envReport) fail(err error) {
	if r.first(err.Error()) {
		r.errs = append(r.errs, err)
	}
}

func (r *envReport) warn(format string, a ...any) {
	if msg := fmt.Sprintf(format, a...); r.first(msg) {
		r.warnings = append(r.warnings, msg)
	}
}

// envSignal reads the variables of one signal into r. It returns what it
// could read even with an error, so that one bad variable is reported with
// the others.
func envSignal(getenv func(string) string, s Signal, r *envReport) SignalConfig {
	upper := strings.ToUpper(string(s))
	// lookup returns the per-signal variable when set, else the general one,
	// and the name it read.
	lookup := func(key string) (name, value string) {
		name = "OTEL_EXPORTER_OTLP_" + upper + "_" + key
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return name, v
		}
		name = "OTEL_EXPORTER_OTLP_" + key
		return name, strings.TrimSpace(getenv(name))
	}
	var c SignalConfig
	name, v := lookup("PROTOCOL")
	protocolOK := v == "" || strings.EqualFold(v, "http/json")
	if !protocolOK {
		r.fail(fmt.Errorf("otlpjson: %s=%q is not supported: Joulie writes OTLP JSON only, set http/json", name, v))
	}
	if name, v := lookup("HEADERS"); v != "" {
		kvs, err := ParseKeyValueList(v)
		if err != nil {
			r.fail(fmt.Errorf("otlpjson: %s: %w", name, err))
		}
		if len(kvs) > 0 {
			c.Headers = make(map[string]string, len(kvs))
			for _, kv := range kvs {
				c.Headers[kv.Key] = *kv.Value.String
			}
		}
	}
	if name, v := lookup("TIMEOUT"); v != "" {
		d, err := envMillis(name, v)
		if err != nil {
			r.fail(err)
		}
		c.Timeout = d
	}
	if name, v := lookup("COMPRESSION"); v != "" {
		switch strings.ToLower(v) {
		case "gzip":
			c.Compression = "gzip"
		case "none":
		default:
			r.fail(fmt.Errorf("otlpjson: %s=%q, want gzip or none", name, v))
		}
	}

	perSignal := "OTEL_EXPORTER_OTLP_" + upper + "_ENDPOINT"
	if v := strings.TrimSpace(getenv(perSignal)); v != "" {
		if err := envCheckURL(perSignal, v); err != nil {
			r.fail(err)
		} else {
			c.Endpoint = v
		}
	} else if v := strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")); v != "" {
		u, err := envSignalURL(v, s)
		if err != nil {
			r.fail(err)
		} else {
			c.Endpoint = u
		}
	}

	// An item other than otlp and none is ignored, so the signal is off
	// only when an item says none and none says otlp.
	exporterVar := "OTEL_" + upper + "_EXPORTER"
	if v := strings.TrimSpace(getenv(exporterVar)); v != "" {
		otlp, none := false, false
		for _, e := range strings.Split(v, ",") {
			switch e = strings.ToLower(strings.TrimSpace(e)); e {
			case "otlp":
				otlp = true
			case "none":
				none = true
			case "":
			default:
				r.warn("otlpjson: %s=%q: %s is not supported and is ignored; only otlp and none are", exporterVar, v, e)
			}
		}
		if none && !otlp {
			c.Endpoint = ""
		}
	}
	if !protocolOK {
		c.Endpoint = ""
	}
	return c
}

// envMillis parses a positive number of milliseconds.
func envMillis(name, v string) (time.Duration, error) {
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil || ms <= 0 {
		return 0, fmt.Errorf("otlpjson: %s=%q is not a positive number of milliseconds", name, v)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// envCheckURL accepts an absolute http or https URL.
func envCheckURL(name, v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("otlpjson: %s=%q is not an http or https URL", name, v)
	}
	return nil
}

// envSignalURL appends v1/<signal> to the path of base, with exactly one
// slash before it: http://h:4318 and http://h:4318/ both give
// http://h:4318/v1/metrics, and a base with a path keeps it, as Prometheus'
// http://h:9090/api/v1/otlp gives http://h:9090/api/v1/otlp/v1/metrics.
func envSignalURL(base string, s Signal) (string, error) {
	if err := envCheckURL("OTEL_EXPORTER_OTLP_ENDPOINT", base); err != nil {
		return "", err
	}
	u, _ := url.Parse(base)
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/" + string(s)
	u.RawPath = ""
	return u.String(), nil
}

// ParseKeyValueList parses the list format of OTEL_EXPORTER_OTLP_HEADERS and
// OTEL_RESOURCE_ATTRIBUTES: key=value pairs separated by commas, with
// spaces around each part trimmed and the value percent-decoded. Empty
// items are skipped. The pairs come back in order as string attributes; a
// key may repeat, and the reader decides which one wins.
func ParseKeyValueList(s string) ([]KeyValue, error) {
	var out []KeyValue
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k, v, ok := strings.Cut(item, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("item %q is not key=value", item)
		}
		dv, err := url.PathUnescape(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("value of %q: %w", k, err)
		}
		out = append(out, String(k, dv))
	}
	return out, nil
}
