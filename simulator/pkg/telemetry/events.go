package telemetry

import (
	"fmt"
	"time"

	"github.com/matbun/joulie/pkg/otel/otlpjson"
	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/emulate"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// EventRecord is one event before encoding. Node is the emulated node whose
// resource the record goes on, empty for the simulator's own resource;
// Time is when it occurred. Attributes are those of Event.Attributes the
// record has values for. Body is a display string only: readers use the
// attributes.
type EventRecord struct {
	Node       string
	Event      semconv.Event
	Time       time.Time
	Attributes []otlpjson.KeyValue
	Body       string
}

// HardwareEvents are the events of snapshot s, node by node: node.ready on
// a node's first snapshot after the emulator started, then one record per
// file event of its last step, classified by Classify, at the time the step
// saw it.
func HardwareEvents(s *emulate.Snapshot) []EventRecord {
	if s == nil {
		return nil
	}
	var out []EventRecord
	for _, n := range s.Nodes {
		if n.Started {
			out = append(out, nodeReady(n, s.Time))
		}
		for _, e := range n.Events {
			if r, ok := hardwareEvent(n.Name, e); ok {
				out = append(out, r)
			}
		}
	}
	return out
}

// nodeReady says node n's tree is in place and its first step ran, and
// whether the tree was resumed and how many entries a resume put back.
func nodeReady(n emulate.NodeSnapshot, at time.Time) EventRecord {
	resumed := !n.Fresh
	how := "rendered"
	if resumed {
		how = fmt.Sprintf("resumed, %d entries put back", len(n.Added))
	}
	return EventRecord{
		Node:  n.Name,
		Event: semconv.EventSimNodeReady,
		Time:  at,
		Attributes: []otlpjson.KeyValue{
			otlpjson.String(semconv.AttrSimProfile, n.Profile),
			otlpjson.String(semconv.AttrSimPool, n.Pool),
			otlpjson.Bool(semconv.AttrSimTreeResumed, resumed),
			otlpjson.Int(semconv.AttrSimTreeAdded, int64(len(n.Added))),
		},
		Body: fmt.Sprintf("node %s ready, profile %s, tree %s", n.Name, n.Profile, how),
	}
}

// hardwareEvent is the record of file event e on node; ok is false for an
// event Classify does not know.
func hardwareEvent(node string, e hwemu.Event) (EventRecord, bool) {
	ev, errorType := Classify(e)
	if ev.Name == "" {
		return EventRecord{}, false
	}
	var attrs []otlpjson.KeyValue
	if e.Component != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrHwID, e.Component))
		if t := classifyHwType(e.Component); t != "" {
			attrs = append(attrs, otlpjson.String(semconv.AttrHwType, t))
		}
	}
	attrs = append(attrs,
		otlpjson.String(semconv.AttrSimFile, e.File),
		otlpjson.String(semconv.AttrSimOutcome, e.Kind),
		otlpjson.String(semconv.AttrSimValueWritten, e.Written),
		otlpjson.String(semconv.AttrSimValueEffective, e.Effective),
	)
	if errorType != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrErrorType, errorType))
	}
	return EventRecord{
		Node:       node,
		Event:      ev,
		Time:       e.Time,
		Attributes: attrs,
		Body:       fmt.Sprintf("%s %s: written %q, effective %q", e.Kind, e.File, e.Written, e.Effective),
	}, true
}

// NodeMismatch is node.mismatch for spec node node at time at: errorType is
// semconv.ErrorTypeNodeMissing when the Kubernetes Node does not exist, or
// semconv.ErrorTypeProfileMismatch when it names nodeProfile instead of the
// spec's profile. The caller emits it once per change, not once per poll.
func NodeMismatch(node, errorType, profile, nodeProfile string, at time.Time) EventRecord {
	attrs := []otlpjson.KeyValue{
		otlpjson.String(semconv.AttrErrorType, errorType),
		otlpjson.String(semconv.AttrSimProfile, profile),
	}
	body := fmt.Sprintf("node %s: %s, spec profile %s", node, errorType, profile)
	if nodeProfile != "" {
		attrs = append(attrs, otlpjson.String(semconv.AttrSimNodeProfile, nodeProfile))
		body += ", Node profile " + nodeProfile
	}
	return EventRecord{Node: node, Event: semconv.EventSimNodeMismatch, Time: at, Attributes: attrs, Body: body}
}

// logsRequest encodes records as one LogsData: one resource per node, in
// the order nodes first appear, plus the simulator's resource for records
// with no node. observed is the time of emission.
func logsRequest(recs []EventRecord, cluster string, observed time.Time, r Resource) otlpjson.LogsData {
	d := otlpjson.LogsData{ResourceLogs: []otlpjson.ResourceLogs{}}
	index := map[string]int{}
	for _, rec := range recs {
		i, ok := index[rec.Node]
		if !ok {
			res := r.Simulator(cluster)
			if rec.Node != "" {
				res = r.Node(rec.Node, cluster)
			}
			i = len(d.ResourceLogs)
			index[rec.Node] = i
			d.ResourceLogs = append(d.ResourceLogs, otlpjson.ResourceLogs{
				Resource: res,
				ScopeLogs: []otlpjson.ScopeLogs{{
					Scope:      otlpjson.Scope{Name: ScopeName, Version: r.ServiceVersion},
					LogRecords: []otlpjson.LogRecord{},
					SchemaURL:  semconv.SchemaURL,
				}},
				SchemaURL: semconv.SchemaURL,
			})
		}
		lr := otlpjson.LogRecord{
			TimeUnixNano:         metricsUnixNano(rec.Time),
			ObservedTimeUnixNano: metricsUnixNano(observed),
			SeverityNumber:       rec.Event.SeverityNumber,
			SeverityText:         rec.Event.SeverityText,
			Attributes:           rec.Attributes,
			EventName:            rec.Event.Name,
		}
		if rec.Body != "" {
			body := rec.Body
			lr.Body = &otlpjson.AnyValue{String: &body}
		}
		sl := &d.ResourceLogs[i].ScopeLogs[0]
		sl.LogRecords = append(sl.LogRecords, lr)
	}
	return d
}
