package telemetry

import (
	"path"
	"strings"

	"github.com/matbun/joulie/pkg/otel/semconv"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// Classify maps what a step did with a file to the event it is reported
// as, and the error.type a rejected write carries:
//
//   - a write that took effect, as written or quantized, resolved or
//     clamped, is cap.applied on a power limit file and setting.applied on
//     any other file (a frequency bound, a governor, a time window);
//   - reverted-out-of-range, parse-error and restored are write.rejected
//     with error.type reverted_out_of_range, parse_error and restored;
//   - reset-persistence-off is cap.reset.
//
// An unknown kind gives the zero Event, which callers skip.
func Classify(e hwemu.Event) (semconv.Event, string) {
	switch e.Kind {
	case hwemu.EventAccepted, hwemu.EventQuantized, hwemu.EventResolved, hwemu.EventClamped:
		if classifyLimitFile(e.File) {
			return semconv.EventSimCapApplied, ""
		}
		return semconv.EventSimSettingApplied, ""
	case hwemu.EventRevertedOutOfRange:
		return semconv.EventSimWriteRejected, semconv.ErrorTypeRevertedOutOfRange
	case hwemu.EventParseError:
		return semconv.EventSimWriteRejected, semconv.ErrorTypeParseError
	case hwemu.EventRestored:
		return semconv.EventSimWriteRejected, semconv.ErrorTypeRestored
	case hwemu.EventResetPersistenceOff:
		return semconv.EventSimCapReset, ""
	}
	return semconv.Event{}, ""
}

// classifyLimitFile reports whether file sets a power limit: a powercap
// constraint_<n>_power_limit_uw (powercap.rst), an hwmon power1_cap (amdgpu
// and the HSMP socket limit, sysfs-interface.rst) or the NVML limit the fake
// nvidia-smi writes, power_limit_mw.
func classifyLimitFile(file string) bool {
	base := path.Base(file)
	return strings.HasSuffix(base, "_power_limit_uw") || base == "power1_cap" || base == "power_limit_mw"
}

// classifyHwType is the hw.type of component id: cpu for cpu<n>, memory for
// memory<n>, gpu for gpu<n>, and empty for anything else.
func classifyHwType(id string) string {
	t := strings.TrimRight(id, "0123456789")
	if t == id {
		return ""
	}
	switch t {
	case semconv.HwTypeCPU, semconv.HwTypeMemory, semconv.HwTypeGPU:
		return t
	}
	return ""
}
