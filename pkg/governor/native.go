package governor

import (
	"strconv"
	"strings"

	joulie "github.com/matbun/joulie/pkg/api"
)

// ParseNative reads back the State that Plan.Native spells for backend (a
// pkg/api GovernorBackend* value), as the controlStatus applied field holds
// it: the agent reads what an earlier run of its own applied, and a test
// reads what the agent reports, through it, never through the text. A word
// it does not know is skipped. A cpufreq or amdgpu state that names no
// single governor or level ("mixed") reads as that word.
func ParseNative(backend, native string) State {
	var s State
	words := strings.Fields(native)
	switch backend {
	case joulie.GovernorBackendCPUFreq:
		if len(words) == 0 {
			return s
		}
		s.Governor = words[0]
		for _, w := range words[1:] {
			key, value, _ := strings.Cut(w, "=")
			switch key {
			case "epp":
				s.EPP = value
			case "boost", "no_turbo":
				on := value == "1"
				if key == "no_turbo" {
					on = !on
				}
				s.Boost = joulie.CPUGovernorBoostOff
				if on {
					s.Boost = joulie.CPUGovernorBoostOn
				}
			}
		}
	case joulie.GovernorBackendAMDGPUSysfs:
		if len(words) == 0 {
			return s
		}
		s.Level = words[0]
		for _, w := range words[1:] {
			if v, ok := strings.CutPrefix(w, "sclk_max="); ok {
				s.SclkMaxMHz, _ = strconv.Atoi(v)
			}
		}
	case joulie.GovernorBackendNvidiaSMI:
		for i := 0; i+1 < len(words); i++ {
			switch words[i] {
			case "-lgc":
				s.GPULockMHz = parseLockArg(words[i+1])
			case "-lmc":
				s.MemLockMHz = parseLockArg(words[i+1])
			}
		}
	}
	return s
}

// parseLockArg reads "<min>,<max>" as nvidia-smi is told a lock; nil when it
// is not one.
func parseLockArg(v string) *[2]int {
	lo, hi, ok := strings.Cut(v, ",")
	a, errA := strconv.Atoi(strings.TrimSpace(lo))
	b, errB := strconv.Atoi(strings.TrimSpace(hi))
	if !ok || errA != nil || errB != nil {
		return nil
	}
	return &[2]int{a, b}
}
