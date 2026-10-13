// Package workload holds the parts of the simulator's workload engine that
// other components share: the Kubernetes objects a run creates, the labels
// and annotations on them, the rate formulas, and the run API.
//
// # Models
//
// SIM_WORKLOAD_MODEL selects one of two models. ModelLegacy is the
// behaviour experiments 01 to 03 were measured with: bare pods, a
// NotIn-eco affinity, completion by delete, a forced completion after a
// stall, and the GPU cap penalty applied twice. ModelV1 runs only in emulate
// mode, where it is the default: one batch/v1 Job per group of trace jobs,
// the workload class declared by annotation, completion through a KWOK Stage
// that the simulator triggers with an annotation, and stalls reported rather
// than forced. Legacy combined with emulate is an error, because legacy pods
// select type=kwok and NFD GPU labels in a form no emulated node carries.
//
// # Options
//
// Every v1 setting is an environment variable (the Env constants) that a run
// API request may override, and an invalid value is a startup error. The
// zero value of the engine's options equals the legacy defaults: "no cap"
// and "disabled" are the explicit sentinel Unlimited, never 0, so an engine
// built without options keeps the legacy limits.
//
// # Run directory
//
// A v1 run reads its trace from and writes its results to
// <SIM_RUNS_DIR>/<runId>/, by default <SIM_HWEMU_ROOT>/_runs/<runId>/. A node
// name cannot contain an underscore, so the directory never collides with a
// node's tree. The results are OTLP JSON Lines under otlp/, one MetricsData
// or LogsData object per line.
//
// keys.go is the day-0 freeze shared by the simulator, workloadgen and the
// scenario runner; its names change only by an amendment every user of them
// agrees to.
package workload
