package governor

import (
	"path"
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

// InForce spells, as Plan.Native does, the part of a cpufreq or amdgpu plan
// that taken, the writes read back as written (an overdrive command once
// the table printed again), put in force: what the controlStatus applied
// field names after a write failed. A field of the plan is in force when
// every write of the plan that sets it was taken. The governor or level
// leads: taken on some policies or devices only, it reads mixed, and taken
// on none, nothing is in force and InForce is empty.
func InForce(p Plan, taken []Write) string {
	got := map[Write]bool{}
	for _, w := range taken {
		got[w] = true
	}
	all, done := map[string]int{}, map[string]int{}
	for _, w := range p.Writes {
		f := writeField(w)
		all[f]++
		if got[w] {
			done[f]++
		}
	}
	words := strings.Fields(p.Native)
	head := "governor"
	if all["level"] > 0 {
		head = "level"
	}
	if len(words) == 0 || done[head] == 0 {
		return ""
	}
	out := []string{words[0]}
	if done[head] < all[head] {
		out[0] = joulie.GovernorCurrentMixed
	}
	for _, w := range words[1:] {
		f, _, _ := strings.Cut(w, "=")
		if f == "no_turbo" {
			f = "boost"
		}
		if all[f] > 0 && done[f] == all[f] {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// writeField names the field of State a write sets, by the file it writes,
// with the key Plan.Native spells it by: governor, epp, boost, level or
// sclk_max; empty for any other file.
func writeField(w Write) string {
	switch path.Base(w.RelPath) {
	case PolicyGovernorFile:
		return "governor"
	case PolicyEPPFile:
		return "epp"
	case PolicyBoostFile, path.Base(NoTurboRelPath):
		return "boost"
	case AMDLevelFile:
		return "level"
	case AMDODFile:
		return "sclk_max"
	}
	return ""
}
