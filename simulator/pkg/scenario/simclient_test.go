package scenario_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/matbun/joulie/simulator/pkg/scenario"
	"github.com/matbun/joulie/simulator/pkg/workload"
)

// The client speaks the frozen run API: a run request body, the status,
// finish with cleanup, and a 4xx answer as a SimError carrying the
// simulator's RunError.
func TestSimClientRunAPI(t *testing.T) {
	var got workload.RunRequest
	var finishQuery string
	active := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == workload.PathRuns:
			if active {
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(workload.RunError{Message: "a run is active"})
				return
			}
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &got)
			active = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(workload.RunStarted{RunID: got.RunID, Jobs: 6, Groups: 6})
		case r.Method == http.MethodGet && r.URL.Path == workload.PathRunCurrent:
			json.NewEncoder(w).Encode(workload.RunStatus{RunID: got.RunID, Phase: workload.PhaseRunning})
		case r.Method == http.MethodPost && r.URL.Path == workload.PathRunFinish:
			finishQuery = r.URL.RawQuery
			json.NewEncoder(w).Encode(workload.RunStatus{RunID: got.RunID, Phase: workload.PhaseAborted})
		default:
			w.WriteHeader(http.StatusPreconditionFailed)
			w.Write([]byte("not in v1 emulate mode"))
		}
	}))
	defer srv.Close()
	c := &scenario.SimClient{HTTP: srv.Client(), BaseURL: srv.URL}
	ctx := context.Background()
	s := fxDecode(t, fxScenario)
	s.Workload.Placement = &scenario.Placement{NodeSelector: map[string]string{}}
	req := scenario.RunRequestFor(s, fxEnv())
	started, err := c.Start(ctx, req)
	if err != nil || started.Jobs != 6 {
		t.Fatalf("start: %+v %v", started, err)
	}
	if got.RunID != fxEnv().RunID || got.ClusterName != "smoke" || got.Options.BaseSpeedPerCore == nil || *got.Options.BaseSpeedPerCore != 1 ||
		got.Options.NodeSelector == nil || len(got.Options.NodeSelector) != 0 || got.Options.Tolerations != nil {
		t.Errorf("request %+v options %+v", got, got.Options)
	}
	var se *scenario.SimError
	if _, err := c.Start(ctx, req); !errors.As(err, &se) || se.StatusCode != http.StatusConflict || se.Run == nil || se.Run.Message != "a run is active" {
		t.Errorf("second start: %v", err)
	}
	if st, err := c.Current(ctx); err != nil || st.Phase != workload.PhaseRunning {
		t.Errorf("current %+v %v", st, err)
	}
	if st, err := c.Finish(ctx, true); err != nil || st.Phase != workload.PhaseAborted || finishQuery != "cleanup=true" {
		t.Errorf("finish %+v %v query %q", st, err, finishQuery)
	}
	bad := &scenario.SimClient{HTTP: srv.Client(), BaseURL: srv.URL + "/elsewhere"}
	if _, err := bad.Current(ctx); !errors.As(err, &se) || se.StatusCode != http.StatusPreconditionFailed || se.Body != "not in v1 emulate mode" {
		t.Errorf("412: %v", err)
	}
}
