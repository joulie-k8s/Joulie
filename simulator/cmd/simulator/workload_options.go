package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/matbun/joulie/simulator/pkg/workload"
)

// workloadOptions are the settings of the workload engine, read from the
// SIM_WORKLOAD_* variables, SIM_RUNS_DIR, SIM_RUN_ID and
// SIM_OTLP_FILE_INTERVAL by loadWorkloadOptions (names in
// simulator/pkg/workload/keys.go).
//
// The zero value is the legacy engine as it ran before these became
// settings: 20 creates and 10 deletes per tick, a 120 s force-complete, and
// a client limited to 30 QPS with a burst of 50. An engine a test builds
// without options therefore behaves as before; the accessors below read the
// fields with that fallback.
type workloadOptions struct {
	// Model is the workload model; empty is workload.ModelLegacy.
	Model workload.Model

	// Read by both models.
	//
	// MaxCreatesPerTick caps the creates of one tick: 0 is
	// workload.LegacyMaxCreatesPerTick and workload.Unlimited removes the
	// cap.
	MaxCreatesPerTick int
	// KubeQPS and KubeBurst are the workload client's rate limits; 0 is
	// workload.LegacyKubeQPS and workload.LegacyKubeBurst.
	KubeQPS   float32
	KubeBurst int

	// Read by the legacy model only.
	//
	// MaxDeletesPerTick caps the deletes of completed pods in one tick: 0 is
	// workload.LegacyMaxDeletesPerTick and workload.Unlimited removes the
	// cap.
	MaxDeletesPerTick int
	// StallForceComplete is how long a submitted job may make no progress
	// before it is completed by force: 0 is
	// workload.LegacyStallForceComplete and a negative value disables it.
	StallForceComplete time.Duration

	// Read by the v1 model only; loadWorkloadOptions fills the v1 defaults.
	Owner      workload.Owner
	Intent     workload.Intent
	Completion workload.Completion
	// Namespace replaces the trace's namespace when set.
	Namespace string
	// NodeSelector is added to every pod's placement. nil means
	// workload.DefaultNodeSelector of the cluster spec, which the run
	// resolves once the spec is loaded.
	NodeSelector map[string]string
	// Tolerations are added to every pod's placement; unset, they are
	// workload.KWOKToleration, and an empty list clears them.
	Tolerations      []corev1.Toleration
	JobTTL           time.Duration
	StallReport      time.Duration
	RunsDir          string
	RunID            string
	OTLPFileInterval time.Duration
}

// workloadV1OnlyEnv and workloadLegacyOnlyEnv are the variables one model
// ignores; each one set logs a warning in the other model.
var (
	workloadV1OnlyEnv = []string{
		workload.EnvOwner, workload.EnvIntent, workload.EnvCompletion, workload.EnvNamespace,
		workload.EnvNodeSelector, workload.EnvTolerations, workload.EnvJobTTL, workload.EnvStallReport,
		workload.EnvRunsDir, workload.EnvRunID, workload.EnvOTLPFileInterval,
	}
	// EnvTick is the legacy workload loop's period; a v1 run ticks on the
	// emulator's tick.
	workloadLegacyOnlyEnv = []string{workload.EnvTick, workload.EnvMaxDeletesPerTick, workload.EnvStallForceComplete}
)

// loadWorkloadOptions reads the workload settings from getenv. emulate is
// SIM_EMULATE: SIM_WORKLOAD_MODEL defaults to v1 with it and to legacy
// without it, and the other pairing is an error either way, since the v1
// engine needs the emulated nodes and a legacy trace cannot drive them
// (X10). An invalid value is an error naming its variable; all of them are
// reported together. The warnings name each variable set that the chosen
// model does not read.
func loadWorkloadOptions(getenv func(string) string, emulate bool) (workloadOptions, []string, error) {
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	model := workload.ModelLegacy
	if emulate {
		model = workload.ModelV1
	}
	if s := get(workload.EnvModel); s != "" {
		m, err := workload.ParseModel(s)
		if err != nil {
			return workloadOptions{}, nil, err
		}
		model = m
	}
	switch {
	case model == workload.ModelV1 && !emulate:
		return workloadOptions{}, nil, fmt.Errorf("%s=%s needs %s=true: the v1 engine drives emulated nodes", workload.EnvModel, model, envEmulate)
	case model == workload.ModelLegacy && emulate:
		return workloadOptions{}, nil, fmt.Errorf("%s=%s cannot run with %s=true: a legacy trace cannot drive emulated nodes", workload.EnvModel, model, envEmulate)
	}

	o := workloadOptions{Model: model}
	var errs []error
	var warnings []string
	ignored := func(keys []string, other workload.Model) {
		for _, k := range keys {
			if get(k) != "" {
				warnings = append(warnings, fmt.Sprintf("%s is read with %s=%s only; ignored", k, workload.EnvModel, other))
			}
		}
	}

	creates, qps, burst := workload.LegacyMaxCreatesPerTick, float32(workload.LegacyKubeQPS), workload.LegacyKubeBurst
	if model == workload.ModelV1 {
		creates, qps, burst = workload.DefaultMaxCreatesPerTick, workload.DefaultKubeQPS, workload.DefaultKubeBurst
	}
	o.MaxCreatesPerTick = perTickSetting(get, workload.EnvMaxCreatesPerTick, creates, &errs)
	o.KubeQPS = qpsSetting(get, workload.EnvKubeQPS, qps, &errs)
	o.KubeBurst = positiveIntSetting(get, workload.EnvKubeBurst, burst, &errs)

	if model == workload.ModelLegacy {
		o.MaxDeletesPerTick = perTickSetting(get, workload.EnvMaxDeletesPerTick, workload.LegacyMaxDeletesPerTick, &errs)
		o.StallForceComplete = forceCompleteSetting(get, workload.EnvStallForceComplete, workload.LegacyStallForceComplete, &errs)
		ignored(workloadV1OnlyEnv, workload.ModelV1)
		return o, warnings, errors.Join(errs...)
	}

	ignored(workloadLegacyOnlyEnv, workload.ModelLegacy)
	o.Owner = enumSetting(get, workload.EnvOwner, workload.DefaultOwner, workload.ParseOwner, &errs)
	o.Intent = enumSetting(get, workload.EnvIntent, workload.DefaultIntent, workload.ParseIntent, &errs)
	o.Completion = enumSetting(get, workload.EnvCompletion, workload.DefaultCompletion, workload.ParseCompletion, &errs)
	if ns := get(workload.EnvNamespace); ns != "" {
		if msgs := validation.IsDNS1123Label(ns); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("%s=%q: %s", workload.EnvNamespace, ns, strings.Join(msgs, "; ")))
		}
		o.Namespace = ns
	}
	if s := get(workload.EnvNodeSelector); s != "" {
		set, err := labels.ConvertSelectorToLabelsMap(s)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: want key=value pairs, comma separated: %w", workload.EnvNodeSelector, s, err))
		}
		o.NodeSelector = map[string]string(set)
	}
	o.Tolerations = []corev1.Toleration{workload.KWOKToleration()}
	if s := get(workload.EnvTolerations); s != "" {
		dec := json.NewDecoder(bytes.NewReader([]byte(s)))
		dec.DisallowUnknownFields()
		var list []corev1.Toleration
		if err := dec.Decode(&list); err != nil {
			errs = append(errs, fmt.Errorf("%s: want a JSON list of tolerations: %w", workload.EnvTolerations, err))
		}
		o.Tolerations = list
	}
	o.JobTTL = durationSetting(get, workload.EnvJobTTL, workload.DefaultJobTTL, true, &errs)
	o.StallReport = durationSetting(get, workload.EnvStallReport, workload.DefaultStallReport, false, &errs)
	o.OTLPFileInterval = durationSetting(get, workload.EnvOTLPFileInterval, workload.DefaultOTLPFileInterval, false, &errs)
	o.RunsDir = get(workload.EnvRunsDir)
	if o.RunsDir == "" {
		root := get(envHwemuRoot)
		if root == "" {
			root = defaultHwemuRoot
		}
		o.RunsDir = filepath.Join(root, workload.RunsDirName)
	}
	if id := get(workload.EnvRunID); id != "" {
		if id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
			errs = append(errs, fmt.Errorf("%s=%q: a run id names a directory under %s, so it may not contain a slash or be . or ..", workload.EnvRunID, id, workload.EnvRunsDir))
		}
		o.RunID = id
	}
	return o, warnings, errors.Join(errs...)
}

// perTickSetting reads a per-tick cap: a positive count or
// workload.Unlimited. 0 is refused because it would never act.
func perTickSetting(get func(string) string, key string, def int, errs *[]error) int {
	s := get(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || (n < 1 && n != workload.Unlimited) {
		*errs = append(*errs, fmt.Errorf("%s=%q: want a positive count, or %d for no cap", key, s, workload.Unlimited))
		return def
	}
	return n
}

// forceCompleteSetting reads a timeout that workload.Unlimited disables.
func forceCompleteSetting(get func(string) string, key string, def time.Duration, errs *[]error) time.Duration {
	s := get(key)
	if s == strconv.Itoa(workload.Unlimited) {
		return time.Duration(workload.Unlimited)
	}
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s=%q: want a positive duration such as 120s, or %d to disable it", key, s, workload.Unlimited))
		return def
	}
	return d
}

func durationSetting(get func(string) string, key string, def time.Duration, zeroOK bool, errs *[]error) time.Duration {
	s := get(key)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 || (d == 0 && !zeroOK) {
		want := "a positive duration"
		if zeroOK {
			want = "a duration of 0 or more"
		}
		*errs = append(*errs, fmt.Errorf("%s=%q: want %s, such as 10m", key, s, want))
		return def
	}
	return d
}

func qpsSetting(get func(string) string, key string, def float32, errs *[]error) float32 {
	s := get(key)
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(s, 32)
	// ParseFloat reads "NaN", which compares false with everything, so a
	// v <= 0 check alone would let it through as a rate limit.
	if err != nil || math.IsNaN(v) || v <= 0 || math.IsInf(v, 0) {
		*errs = append(*errs, fmt.Errorf("%s=%q: want a positive number", key, s))
		return def
	}
	return float32(v)
}

func positiveIntSetting(get func(string) string, key string, def int, errs *[]error) int {
	s := get(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		*errs = append(*errs, fmt.Errorf("%s=%q: want a positive integer", key, s))
		return def
	}
	return n
}

func enumSetting[T ~string](get func(string) string, key string, def T, parse func(string) (T, error), errs *[]error) T {
	s := get(key)
	if s == "" {
		return def
	}
	v, err := parse(s)
	if err != nil {
		*errs = append(*errs, err)
		return def
	}
	return v
}

// model is the workload model, legacy for the zero value.
func (o workloadOptions) model() workload.Model {
	if o.Model == "" {
		return workload.ModelLegacy
	}
	return o.Model
}

// maxCreatesPerTick is the creates cap of one tick, workload.Unlimited for
// none.
func (o workloadOptions) maxCreatesPerTick() int {
	if o.MaxCreatesPerTick == 0 {
		return workload.LegacyMaxCreatesPerTick
	}
	return o.MaxCreatesPerTick
}

// maxDeletesPerTick is the legacy deletes cap of one tick,
// workload.Unlimited for none.
func (o workloadOptions) maxDeletesPerTick() int {
	if o.MaxDeletesPerTick == 0 {
		return workload.LegacyMaxDeletesPerTick
	}
	return o.MaxDeletesPerTick
}

// stallForceComplete is the legacy force-complete timeout, and false when it
// is disabled.
func (o workloadOptions) stallForceComplete() (time.Duration, bool) {
	switch {
	case o.StallForceComplete == 0:
		return workload.LegacyStallForceComplete, true
	case o.StallForceComplete < 0:
		return 0, false
	}
	return o.StallForceComplete, true
}

// kubeQPS and kubeBurst are the workload client's rate limits.
func (o workloadOptions) kubeQPS() float32 {
	if o.KubeQPS == 0 {
		return workload.LegacyKubeQPS
	}
	return o.KubeQPS
}

func (o workloadOptions) kubeBurst() int {
	if o.KubeBurst == 0 {
		return workload.LegacyKubeBurst
	}
	return o.KubeBurst
}

// underCap reports whether n actions leave room under limit, which
// workload.Unlimited never reaches.
func underCap(n, limit int) bool {
	return limit == workload.Unlimited || n < limit
}
