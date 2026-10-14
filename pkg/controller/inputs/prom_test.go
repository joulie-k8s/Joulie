package inputs

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// promServer answers every query with status and body and records the
// last request.
func promServer(t *testing.T, status int, contentType, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

// TestClientParsesVectorAndScalar: a vector keeps each series' labels,
// value and time, to the millisecond, a scalar is one sample without
// labels, warnings come through, the query reaches GET /api/v1/query
// intact, and a matrix is refused.
func TestClientParsesVectorAndScalar(t *testing.T) {
	const vector = `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"k8s_node_name":"a"},"value":[1759600000.123,"1050"]},
		{"metric":{"k8s_node_name":"b"},"value":[1759600000.123,"350.5"]}]},
		"warnings":["PromQL warning: encountered a mix of histograms and floats"]}`
	srv, last := promServer(t, 200, "application/json", vector)
	q := `sum by (k8s_node_name) (rate(hw_energy_joules_total{hw_type=~"cpu|gpu"}[5m]))`
	res, err := Client{Address: srv.URL + "/"}.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if last.Method != http.MethodGet || last.URL.Path != "/api/v1/query" || last.URL.Query().Get("query") != q {
		t.Errorf("request %s %s query %q, want GET /api/v1/query with the query", last.Method, last.URL.Path, last.URL.Query().Get("query"))
	}
	want := Result{
		Type: "vector",
		Samples: []Sample{
			{Labels: map[string]string{"k8s_node_name": "a"}, Value: 1050, Time: time.UnixMilli(1759600000123).UTC()},
			{Labels: map[string]string{"k8s_node_name": "b"}, Value: 350.5, Time: time.UnixMilli(1759600000123).UTC()},
		},
		Warnings: []string{"PromQL warning: encountered a mix of histograms and floats"},
	}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("vector: got %+v, want %+v", res, want)
	}

	srv, _ = promServer(t, 200, "application/json", `{"status":"success","data":{"resultType":"scalar","result":[1759600000,"-3"]}}`)
	res, err = Client{Address: srv.URL}.Query(context.Background(), "scalar(x)")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := res.Single(); err != nil || v != -3 || res.Type != "scalar" || len(res.Samples[0].Labels) != 0 || !res.Samples[0].Time.Equal(time.Unix(1759600000, 0)) {
		t.Errorf("scalar: %+v, Single %v %v; want one sample -3 without labels", res, v, err)
	}

	srv, _ = promServer(t, 200, "application/json", `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	res, err = Client{Address: srv.URL}.Query(context.Background(), "x")
	if err != nil || res.Type != "vector" || len(res.Samples) != 0 {
		t.Errorf("empty vector: %+v, %v", res, err)
	}
	if _, err := res.Single(); !errors.Is(err, ErrNoData) {
		t.Errorf("Single of an empty vector: %v, want ErrNoData", err)
	}

	srv, _ = promServer(t, 200, "application/json", `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1,"1"]]}]}}`)
	if _, err := (Client{Address: srv.URL}).Query(context.Background(), "x[5m]"); err == nil || !strings.Contains(err.Error(), "matrix") {
		t.Errorf("matrix: %v, want an error naming the result type", err)
	}
	if _, err := (Client{}).Query(context.Background(), "x"); err == nil {
		t.Error("no address: no error")
	}
}

// TestClientReportsPrometheusError: every failure is an *Error carrying the
// HTTP status, Prometheus' errorType and error, or the start of a body that
// is not JSON, as a proxy's HTML page. A status other than 2xx fails even
// when the body is a success envelope, as a gateway that answers with its
// own status passes it through.
func TestClientReportsPrometheusError(t *testing.T) {
	// The 422 body Prometheus v3.15.0 returned for a plain target_info join
	// after a producer restarted with a new service.version, verbatim.
	const manyToMany = `{"status":"error","errorType":"execution","error":"found duplicate series for the match group {instance=\"a\", job=\"joulie/joulie-simulator\"} on the right hand-side of the operation: [{__name__=\"target_info\", host_name=\"a\", instance=\"a\", job=\"joulie/joulie-simulator\", k8s_node_name=\"a\", service_version=\"1\"}, {__name__=\"target_info\", host_name=\"a\", instance=\"a\", job=\"joulie/joulie-simulator\", k8s_node_name=\"a\", service_version=\"2\"}];many-to-many matching not allowed: matching labels must be unique on one side"}`
	html := "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\r\n<center><h1>502 Bad Gateway</h1></center>\r\n<hr><center>nginx</center>\r\n" + strings.Repeat("<!-- padding -->\r\n", 20) + "</body>\r\n</html>\r\n"
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
		want        Error
	}{
		{"400 bad_data", 400, "application/json",
			`{"status":"error","errorType":"bad_data","error":"invalid parameter \"query\": 1:5: parse error: unexpected <by>"}`,
			Error{Status: 400, Type: "bad_data", Message: `invalid parameter "query": 1:5: parse error: unexpected <by>`}},
		{"422 many-to-many", 422, "application/json", manyToMany,
			Error{Status: 422, Type: "execution", Message: `found duplicate series for the match group {instance="a", job="joulie/joulie-simulator"} on the right hand-side of the operation: [{__name__="target_info", host_name="a", instance="a", job="joulie/joulie-simulator", k8s_node_name="a", service_version="1"}, {__name__="target_info", host_name="a", instance="a", job="joulie/joulie-simulator", k8s_node_name="a", service_version="2"}];many-to-many matching not allowed: matching labels must be unique on one side`}},
		{"503 timeout", 503, "application/json",
			`{"status":"error","errorType":"timeout","error":"query timed out in expression evaluation"}`,
			Error{Status: 503, Type: "timeout", Message: "query timed out in expression evaluation"}},
		{"502 HTML", 502, "text/html", html,
			Error{Status: 502, Message: strings.TrimSpace(html[:errorExcerptBytes])}},
		{"200 not JSON", 200, "text/plain", "ok",
			Error{Status: 200, Message: "ok"}},
		{"504 success envelope", 504, "application/json", `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			Error{Status: 504, Message: `{"status":"success","data":{"resultType":"vector","result":[]}}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := promServer(t, c.status, c.contentType, c.body)
			_, err := Client{Address: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}.Query(context.Background(), "x")
			var pe *Error
			if !errors.As(err, &pe) {
				t.Fatalf("error %v (%T), want *Error", err, err)
			}
			if *pe != c.want {
				t.Errorf("got  %+v\nwant %+v", *pe, c.want)
			}
			if len(pe.Message) > errorExcerptBytes && pe.Type == "" {
				t.Errorf("message of a body that is not JSON has %d bytes, want at most %d", len(pe.Message), errorExcerptBytes)
			}
			if !strings.Contains(err.Error(), "HTTP "+strings.Fields(c.name)[0]) {
				t.Errorf("Error() = %q does not name the status", err.Error())
			}
		})
	}
}

// TestByLabelReportsMissingAndDuplicates: ByLabel keys each sample by the
// label, skips and counts a sample without it or with it empty, and fails
// on two samples with one key, counting each repeat.
func TestByLabelReportsMissingAndDuplicates(t *testing.T) {
	r := Result{Type: "vector", Samples: []Sample{
		{Labels: map[string]string{"k8s_node_name": "a"}, Value: 1050},
		{Labels: map[string]string{"k8s_node_name": "b", "hw_type": "cpu"}, Value: 350},
		{Labels: map[string]string{}, Value: 597.69},
		{Labels: map[string]string{"node": "c"}, Value: 1},
		{Labels: map[string]string{"k8s_node_name": ""}, Value: 5},
	}}
	got, iss, err := r.ByLabel("k8s_node_name")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]float64{"a": 1050, "b": 350}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if want := (Issues{Series: 5, MissingLabel: 3}); iss != want {
		t.Errorf("issues %+v, want %+v", iss, want)
	}

	r.Samples = append(r.Samples,
		Sample{Labels: map[string]string{"k8s_node_name": "a"}, Value: 1},
		Sample{Labels: map[string]string{"k8s_node_name": "a"}, Value: 2},
		Sample{Labels: map[string]string{"k8s_node_name": "b"}, Value: 3},
	)
	got, iss, err = r.ByLabel("k8s_node_name")
	if err == nil || got != nil {
		t.Fatalf("duplicates: map %v, err %v; want nil and an error", got, err)
	}
	if want := (Issues{Series: 8, MissingLabel: 3, Duplicates: 3}); iss != want {
		t.Errorf("issues %+v, want %+v", iss, want)
	}
	if !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "k8s_node_name") {
		t.Errorf("error %q does not name the label and the first repeated key", err)
	}

	got, iss, err = Result{Type: "vector"}.ByLabel("k8s_node_name")
	if err != nil || len(got) != 0 || iss != (Issues{}) {
		t.Errorf("empty result: %v %+v %v", got, iss, err)
	}
}

// TestNonFiniteRejected: NaN and infinities reach Result as Prometheus
// returned them, and no reader passes them on: ByLabel skips and counts
// them, Single fails, Sum leaves them out.
func TestNonFiniteRejected(t *testing.T) {
	srv, _ := promServer(t, 200, "application/json", `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"k8s_node_name":"a"},"value":[1,"NaN"]},
		{"metric":{"k8s_node_name":"b"},"value":[1,"+Inf"]},
		{"metric":{"k8s_node_name":"c"},"value":[1,"-Inf"]},
		{"metric":{"k8s_node_name":"d"},"value":[1,"42"]}]}}`)
	res, err := Client{Address: srv.URL}.Query(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Samples) != 4 || !math.IsNaN(res.Samples[0].Value) || !math.IsInf(res.Samples[1].Value, 1) || !math.IsInf(res.Samples[2].Value, -1) {
		t.Fatalf("samples %+v, want NaN, +Inf, -Inf and 42 as returned", res.Samples)
	}
	got, iss, err := res.ByLabel("k8s_node_name")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]float64{"d": 42}; !reflect.DeepEqual(got, want) {
		t.Errorf("ByLabel %v, want %v", got, want)
	}
	if want := (Issues{Series: 4, NonFinite: 3}); iss != want {
		t.Errorf("issues %+v, want %+v", iss, want)
	}
	if s := res.Sum(); s != 42 {
		t.Errorf("Sum %v, want 42", s)
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := (Result{Samples: []Sample{{Value: v}}}).Single(); err == nil {
			t.Errorf("Single(%v): no error", v)
		}
	}
	if _, err := (Result{Samples: []Sample{{Value: 1}, {Value: 2}}}).Single(); err == nil || errors.Is(err, ErrNoData) {
		t.Errorf("Single of two samples: %v, want an error other than ErrNoData", err)
	}
	if s := (Result{}).Sum(); s != 0 {
		t.Errorf("Sum of nothing %v, want 0", s)
	}
}

// TestPromDuration: whole minutes as Nm, other whole seconds as Ns, and
// windows under 10 s or with a fraction of a second rejected.
func TestPromDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		5 * time.Minute:  "5m",
		time.Minute:      "1m",
		2 * time.Hour:    "120m",
		90 * time.Second: "90s",
		10 * time.Second: "10s",
		61 * time.Second: "61s",
	} {
		if got, err := PromDuration(d); err != nil || got != want {
			t.Errorf("PromDuration(%s) = %q, %v; want %q", d, got, err, want)
		}
	}
	for _, d := range []time.Duration{0, -time.Minute, 9 * time.Second, 9999 * time.Millisecond, 10500 * time.Millisecond} {
		if got, err := PromDuration(d); err == nil {
			t.Errorf("PromDuration(%s) = %q, want an error", d, got)
		}
	}
}
