package telemetry

import (
	"testing"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// EV01: Classify follows the event table: a PL1 accept is cap.applied, as
// is any limit that took effect (powercap, hwmon power1_cap, NVML); a
// governor, frequency bound, time window or persistence mode change is
// setting.applied; reverted-out-of-range, parse-error and restored are
// write.rejected with their error.type; reset-persistence-off is cap.reset.
func TestEV01ClassifyFollowsTheEventTable(t *testing.T) {
	const (
		pl1      = "sys/devices/virtual/powercap/intel-rapl/intel-rapl:1/constraint_0_power_limit_uw"
		pl2      = "sys/devices/virtual/powercap/intel-rapl/intel-rapl:1/constraint_1_power_limit_uw"
		window   = "sys/devices/virtual/powercap/intel-rapl/intel-rapl:1/constraint_0_time_window_us"
		governor = "sys/devices/system/cpu/cpufreq/policy0/scaling_governor"
		maxFreq  = "sys/devices/system/cpu/cpufreq/policy0/scaling_max_freq"
		hsmp     = "sys/devices/platform/amd_hsmp/hwmon/hwmon0/power1_cap"
		amdgpu   = "sys/devices/pci0000:c1/0000:c1:00.0/hwmon/hwmon3/power1_cap"
		nvml     = "state/nvml/gpu3/power_limit_mw"
		persist  = "state/nvml/gpu3/persistence_mode"
		cpuinfo  = "cpuinfo"
	)
	for _, tc := range []struct {
		file, kind string
		want       semconv.Event
		errorType  string
	}{
		{pl1, hwemu.EventAccepted, semconv.EventSimCapApplied, ""},
		{pl2, hwemu.EventQuantized, semconv.EventSimCapApplied, ""},
		{hsmp, hwemu.EventClamped, semconv.EventSimCapApplied, ""},
		{amdgpu, hwemu.EventAccepted, semconv.EventSimCapApplied, ""},
		{nvml, hwemu.EventResolved, semconv.EventSimCapApplied, ""},
		{governor, hwemu.EventAccepted, semconv.EventSimSettingApplied, ""},
		{maxFreq, hwemu.EventClamped, semconv.EventSimSettingApplied, ""},
		{window, hwemu.EventQuantized, semconv.EventSimSettingApplied, ""},
		{persist, hwemu.EventAccepted, semconv.EventSimSettingApplied, ""},
		{pl1, hwemu.EventRevertedOutOfRange, semconv.EventSimWriteRejected, semconv.ErrorTypeRevertedOutOfRange},
		{governor, hwemu.EventParseError, semconv.EventSimWriteRejected, semconv.ErrorTypeParseError},
		{cpuinfo, hwemu.EventRestored, semconv.EventSimWriteRejected, semconv.ErrorTypeRestored},
		{nvml, hwemu.EventResetPersistenceOff, semconv.EventSimCapReset, ""},
		{pl1, "no-such-kind", semconv.Event{}, ""},
	} {
		got, errorType := Classify(hwemu.Event{File: tc.file, Kind: tc.kind})
		if got.Name != tc.want.Name || errorType != tc.errorType {
			t.Errorf("Classify(%s, %s) = %q, %q; want %q, %q", tc.kind, tc.file, got.Name, errorType, tc.want.Name, tc.errorType)
		}
	}
	for id, want := range map[string]string{"cpu1": "cpu", "memory0": "memory", "gpu12": "gpu", "host": "", "cpu": "", "": "", "nic0": ""} {
		if got := classifyHwType(id); got != want {
			t.Errorf("classifyHwType(%q) = %q, want %q", id, got, want)
		}
	}
}
