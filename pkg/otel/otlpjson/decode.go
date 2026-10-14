package otlpjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// DecodeMetrics reads one MetricsData object, an Export request body or a
// line of a metrics .jsonl file. It is lenient, as the OTLP specification
// asks of receivers: unknown fields are ignored, including fields of the
// protocol Joulie does not model (exponentialHistogram, summary, kvlistValue,
// bytesValue, flags) and keys in a form OTLP JSON does not allow, such as
// snake_case.
func DecodeMetrics(b []byte) (MetricsData, error) {
	var d MetricsData
	if err := json.Unmarshal(b, &d); err != nil {
		return MetricsData{}, fmt.Errorf("otlpjson: decode metrics: %w", err)
	}
	return d, nil
}

// DecodeMetricsStrict reads one MetricsData object and fails on any key that
// is not, exactly, the lowerCamelCase name of a field Joulie models: an
// unknown field, a snake_case proto name (opentelemetry-proto v1.11.1
// docs/specification.md: "Original field names are not valid to use as
// keys"), or a key in another case. It also fails on data after the
// object. Tests use it to hold what Joulie writes to the letter.
func DecodeMetricsStrict(b []byte) (MetricsData, error) {
	var d MetricsData
	if err := decodeStrict(b, &d); err != nil {
		return MetricsData{}, fmt.Errorf("otlpjson: decode metrics strictly: %w", err)
	}
	return d, nil
}

// DecodeLogsStrict reads one LogsData object as strictly as
// DecodeMetricsStrict.
func DecodeLogsStrict(b []byte) (LogsData, error) {
	var d LogsData
	if err := decodeStrict(b, &d); err != nil {
		return LogsData{}, fmt.Errorf("otlpjson: decode logs strictly: %w", err)
	}
	return d, nil
}

// IsMetricsData reports whether b is a JSON object with a top-level
// resourceMetrics key, which marks an OTLP metrics body. A reader uses it to
// tell OTLP from an older JSON format on the same endpoint.
func IsMetricsData(b []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(b, &top) != nil {
		return false
	}
	_, ok := top["resourceMetrics"]
	return ok
}

// decodeKeys holds every JSON key of the model, the tag names of every field
// reachable from MetricsData and LogsData.
var decodeKeys = decodeTagNames(reflect.TypeOf(MetricsData{}), reflect.TypeOf(LogsData{}))

// decodeStrict decodes b into v with unknown fields refused and every key
// checked against decodeKeys. encoding/json matches field names without
// regard to case, so DisallowUnknownFields alone would accept
// "ResourceMetrics"; the key check closes that gap, and DisallowUnknownFields
// refuses a known key in the wrong place, such as count on a gauge point.
func decodeStrict(b []byte, v any) error {
	if err := decodeCheckKeys(b); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the object")
	}
	return nil
}

// decodeCheckKeys walks the tokens of b and fails on the first object key
// that is not in decodeKeys.
func decodeCheckKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	// stack has one entry per open container, true for an object; key says
	// the next token of the innermost object is a key.
	var stack []bool
	key := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				stack = append(stack, d == '{')
				key = d == '{'
				continue
			}
			stack = stack[:len(stack)-1]
		} else if key {
			if k, _ := tok.(string); !decodeKeys[k] {
				return fmt.Errorf("key %q is not an OTLP JSON field name", k)
			}
			key = false
			continue
		}
		// A value ended; inside an object a key comes next.
		key = len(stack) > 0 && stack[len(stack)-1]
	}
}

// decodeTagNames collects the JSON tag names of the fields of ts and of
// every struct reachable from them.
func decodeTagNames(ts ...reflect.Type) map[string]bool {
	names := map[string]bool{}
	seen := map[reflect.Type]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || seen[t] {
			return
		}
		seen[t] = true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name != "" && name != "-" {
				names[name] = true
			}
			walk(f.Type)
		}
	}
	for _, t := range ts {
		walk(t)
	}
	return names
}
