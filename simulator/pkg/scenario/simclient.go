package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"k8s.io/client-go/rest"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// SimClient drives the simulator's run API (workload.PathRuns and the
// paths below it) under BaseURL, such as the simulator Service through the
// API server's proxy.
type SimClient struct {
	HTTP    *http.Client
	BaseURL string
}

// NewProxySimClient returns a client of the harness simulator,
// joulie-telemetry-sim in joulie-sim, through the API server's proxy.
func NewProxySimClient(cfg *rest.Config) (*SimClient, error) {
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &SimClient{HTTP: hc, BaseURL: ServiceProxyURL(cfg, NamespaceSimulator, SimulatorService)}, nil
}

// SimError is an answer of the run API outside 2xx. Run is the decoded
// RunError body when the simulator wrote one.
type SimError struct {
	Method, Path string
	StatusCode   int
	Run          *workload.RunError
	Body         string
}

func (e *SimError) Error() string {
	msg := e.Body
	if e.Run != nil {
		msg = e.Run.Error()
	}
	return fmt.Sprintf("simulator %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, msg)
}

// Start posts req: the simulator answers 201 with the run, 409 while
// another run is active, 400 for a bad trace and 412 outside v1 and
// emulate mode.
func (c *SimClient) Start(ctx context.Context, req workload.RunRequest) (workload.RunStarted, error) {
	var out workload.RunStarted
	return out, c.do(ctx, http.MethodPost, workload.PathRuns, req, &out)
}

// Current returns the status of the active or last run.
func (c *SimClient) Current(ctx context.Context) (workload.RunStatus, error) {
	var out workload.RunStatus
	return out, c.do(ctx, http.MethodGet, workload.PathRunCurrent, nil, &out)
}

// Finish ends the run; the remaining jobs end incomplete. With cleanup the
// run's Job objects are deleted in the background. It returns the final
// status once the run's files are closed.
func (c *SimClient) Finish(ctx context.Context, cleanup bool) (workload.RunStatus, error) {
	var out workload.RunStatus
	path := workload.PathRunFinish
	if cleanup {
		path += "?" + url.Values{workload.QueryCleanup: {"true"}}.Encode()
	}
	return out, c.do(ctx, http.MethodPost, path, nil, &out)
}

func (c *SimClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &SimError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
		var re workload.RunError
		if json.Unmarshal(b, &re) == nil && re.Message != "" {
			e.Run = &re
		}
		return e
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("simulator %s %s: %w", method, path, err)
	}
	return nil
}

// RunRequestFor is the run request of a variant: its run id and cluster,
// and the run options its workload sets. The trace is the simulator's
// default, <runs dir>/<runId>/trace.jsonl, where the harness copies the
// variant's trace.
func RunRequestFor(s Scenario, env VariantEnv) workload.RunRequest {
	w := s.Workload
	o := workload.RunOptions{
		Owner: w.Owner, Intent: w.Intent, Completion: w.Completion, Namespace: w.Namespace,
		JobTTL: w.JobTTL, StallReport: w.StallReport,
	}
	base := s.baseSpeed()
	o.BaseSpeedPerCore = &base
	if p := w.Placement; p != nil {
		o.NodeSelector, o.Tolerations = p.NodeSelector, p.Tolerations
	}
	return workload.RunRequest{RunID: env.RunID, ClusterName: env.ClusterName, Options: o}
}
