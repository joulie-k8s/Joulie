// Package scenario reads, validates, plans and evaluates simulated-cluster
// scenarios, and writes the run results of both scenarios and the smoke test:
// one run directory layout and one summary.json schema.
//
// # Scenario files
//
// A scenario is tests/simcluster/scenarios/<name>.yaml, decoded strictly
// into Scenario: an unknown key anywhere is an error, including inside an
// assertion, which is decoded into the struct its type selects. A scenario
// names a cluster spec and a timing profile, one workload source (inline
// jobs, a workloadgen invocation, or a v3 trace), the Joulie and simulator
// Helm settings, up to four variants, the captures to record and the
// assertions to evaluate. Each variant is the top level with its Joulie and
// Simulator settings merged in key by key (Variant). When variants are
// listed only they run; without variants the top level runs once as the
// unnamed variant "".
//
// # Run directory
//
// Every run writes one directory under <SIMCLUSTER_DIR>/RunsDir, named by
// RunDirName and pointed to by the LatestLink symlink:
//
//	summary.json  scenario.effective.yaml
//	<variant>/  summary.json  trace.jsonl  values.effective.yaml  images.env  variant.env  run-status.json
//	            otlp/{simulator,harness}.{metrics,logs}.jsonl
//	            k8s/{nodetwins,nodehardwares,nodes}-{start,end}.yaml  {jobs,pods,events}-end.yaml
//	            logs/{controller-manager,scheduler,simulator}.log  logs/agent-<node>.log
//
// The otlp files are OTLP JSON Lines, one MetricsData or LogsData object per
// line, with the names of pkg/otel/semconv. The simulator writes its two
// files into its own run directory (package workload) and the runner copies
// them out; the runner writes the harness files itself. A smoke run and a
// scenario without variants have no <variant>/ directory: they write the
// variant files at the top level, next to their one summary.json.
//
// # Results
//
// The evaluator reads a variant directory into a RunData and evaluates each
// check of each assertion against it (Assertion.Checks, AssertionResult).
// summary.json (Summary) is keyed by the semantic
// conventions' test.* attributes; its exit code is ExitPass, ExitFail when
// an assertion of severity fail failed, or ExitError when the run could not
// be carried out.
//
// schema.go and rundata.go are the day-0 freeze that the simulator, the
// scenario runner and the smoke test share; their names change only by an
// amendment every user of them agrees to.
package scenario
