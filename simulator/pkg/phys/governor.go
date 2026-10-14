package phys

import "math"

// The cpufreq governors as pure functions: the frequency each one requests
// for one policy at one step. The rules are read from Linux v6.12 and quoted
// as file:line, under drivers/cpufreq/ unless the path says otherwise. What
// the kernel leaves to the hardware or to the platform (the HWP and
// amd-pstate-epp powersave algorithm, the default sampling interval) is
// marked assumed.

// Kernel defaults of the generic governors' tunables. A zero tunable in
// GovernorTunables stands for its default.
const (
	// ondemand starts with MICRO_FREQUENCY_UP_THRESHOLD (95) when the kernel
	// accounts idle time in microseconds, as every NO_HZ kernel does, and
	// with DEF_FREQUENCY_UP_THRESHOLD (80) otherwise
	// (cpufreq_ondemand.c:21, 24, 369-374).
	defaultOndemandUpThreshold = 95
	// DEF_FREQUENCY_UP_THRESHOLD, DEF_FREQUENCY_DOWN_THRESHOLD and
	// DEF_FREQUENCY_STEP (cpufreq_conservative.c:31-33).
	defaultConservativeUpThreshold   = 80
	defaultConservativeDownThreshold = 20
	defaultConservativeFreqStep      = 5
	// get_freq_step falls back to DEF_FREQUENCY_STEP, in kHz, when the
	// percentage of policy->max rounds to zero (cpufreq_conservative.c:37-46).
	conservativeMinStepKHz = 5
	// The sampling interval of ondemand and conservative starts at
	// max(CPUFREQ_DBS_MIN_SAMPLING_INTERVAL, transition delay)
	// (cpufreq_governor.c:449-451), two ticks at least
	// (cpufreq_governor.c:22). assumed: HZ 1000 and a transition delay
	// below 2 ms, so 2000 us.
	defaultDBSSamplingUS = 2000
	// maxGovernorPeriods bounds the conservative loop of one call.
	maxGovernorPeriods = 100000
)

// Defaults of the assumed HWP powersave shape (GovernorInput.EPPGain and
// EPPCeilingDrop). assumed: EPP 0 reaches the maximum at half load, and EPP
// 128 lowers the full-load frequency by 0.2 of the range. Published
// full-load benchmarks put EPP 128 up to 1.25x slower than performance and
// up to 1.32x slower than EPP 32, and 0.2 sits between 1 - 1/1.25 and
// 1 - 1/1.32.
const (
	DefaultEPPGain        = 1.0
	DefaultEPPCeilingDrop = 0.2
)

// freqRelation is how a request maps to a frequency table entry
// (CPUFREQ_RELATION_L, H, C; include/linux/cpufreq.h:295-297). The
// efficiency flag E (:299-303) is dropped: a driver without inefficient
// entries ignores it (cpufreq.c:2341-2347).
type freqRelation int

const (
	relationLow     freqRelation = iota // lowest entry at or above the request
	relationHigh                        // highest entry at or below the request
	relationClosest                     // closest entry, the lower one on a tie
)

// resolveTableKHz maps v to an entry of table as
// cpufreq_frequency_table_target does (include/linux/cpufreq.h:787-806 for
// L, 855-883 for H, 923-955 for C, in an ascending table): L takes the
// highest entry when none is at or above v, H the lowest when none is at or
// below. Without a table v stands.
func resolveTableKHz(table []int64, v int64, rel freqRelation) int64 {
	if len(table) == 0 {
		return v
	}
	lo, hi := table[0], table[0]
	below, above := int64(0), int64(0)
	haveBelow, haveAbove := false, false
	for _, f := range table {
		lo, hi = min(lo, f), max(hi, f)
		if f <= v && (!haveBelow || f > below) {
			below, haveBelow = f, true
		}
		if f >= v && (!haveAbove || f < above) {
			above, haveAbove = f, true
		}
	}
	switch {
	case !haveBelow:
		return lo
	case !haveAbove:
		return hi
	}
	switch rel {
	case relationLow:
		return above
	case relationHigh:
		return below
	}
	if above-v < v-below {
		return above
	}
	return below
}

// policyRange is a policy's frequency range as the governors see it:
// policy->min and policy->max, each resolved to the table, the minimum
// upward and the maximum downward (cpufreq.c:2651-2654). ok is false for a
// policy without frequencies.
type policyRange struct {
	table  []int64
	lo, hi int64
}

func newPolicyRange(in GovernorInput) (policyRange, bool) {
	lo, hi := in.MinKHz, in.MaxKHz
	if hi <= 0 && lo <= 0 {
		return policyRange{}, false
	}
	lo = max(lo, 0)
	hi = max(hi, lo)
	r := policyRange{table: in.TableKHz}
	r.hi = resolveTableKHz(in.TableKHz, hi, relationHigh)
	r.lo = min(resolveTableKHz(in.TableKHz, lo, relationLow), r.hi)
	return r, true
}

// target is what __cpufreq_driver_target sets for a request: the request
// clamped to the policy range, then resolved (cpufreq.c:536-550, 2326).
func (r policyRange) target(v int64, rel freqRelation) int64 {
	return resolveTableKHz(r.table, min(max(v, r.lo), r.hi), rel)
}

// GovernorStep is one step of the governor of in: the request it keeps,
// to pass back as CurKHz at the next step, and the frequency, in kHz, it
// sets for the policy, a table entry when the policy has a table, within
// the resolved scaling_min_freq and scaling_max_freq.
//
//   - performance sets policy->max (cpufreq_performance.c:17).
//   - powersave sets policy->min (cpufreq_powersave.c:17); with HWP it is the
//     driver's own algorithm instead, modelled by the assumed shape of
//     hwpPowersaveKHz.
//   - schedutil requests 1.25 x the frequency the policy ran at x Util,
//     resolved to the lowest entry at or above it (cpufreq_schedutil.c:144-179
//     and include/linux/sched/cpufreq.h:26-35). This is the kernel's
//     frequency-invariant 1.25 x cpuinfo_max_freq x util in the units of
//     Util: Util is busy time at the frequency the policy ran at, so the
//     product is the demand in kHz and does not depend on that frequency.
//   - ondemand jumps to policy->max when the load is above up_threshold, else
//     sets a frequency proportional to the load from the minimum
//     (cpufreq_ondemand.c:135-169).
//   - conservative steps by freq_step per sampling period: its
//     requested_freq moves by freq_step per period and is resolved to a
//     table entry only when written (cpufreq_conservative.c:103-117,
//     126-140), so on a table coarser than the step the request climbs over
//     several periods while the policy stays on one entry. The request it
//     keeps equals the frequency it sets for every other governor.
//   - userspace holds its last setting (cpufreq_userspace.c:113-122).
//   - Any other word, and an empty one, holds the policy maximum: the
//     performance governor the legacy engines imply.
//
// CurKHz is the governor's previous request, the requestKHz of the previous
// step. schedutil reads it as the frequency the policy ran at, so where the
// policy ran elsewhere (a table driver boosting above its top entry, a
// binding cap) the caller passes the running frequency instead. A zero
// CurKHz is a governor that has not run yet: schedutil then reads Util as
// measured at MaxKHz, and conservative and userspace start at the policy
// maximum.
//
// prevTargetKHz is the frequency the governor set at the previous step, the
// targetKHz GovernorStep returned then (the kernel's policy->cur), and zero
// before the first step. conservative needs it on a table: a rising request
// resolves to the highest entry at or below it and a falling one to the
// lowest entry at or above it (cpufreq_conservative.c:114-115, 138-139), so
// the same request holds a different entry depending on the way it came,
// and a period in which nothing moves keeps that entry. A zero prevTargetKHz
// takes the entry a rising request holds. The other governors ignore it.
func GovernorStep(in GovernorInput, prevTargetKHz int64) (requestKHz, targetKHz int64) {
	return governorStep(in, prevTargetKHz)
}

func governorStep(in GovernorInput, prevTargetKHz int64) (request, target int64) {
	r, ok := newPolicyRange(in)
	if !ok {
		return 0, 0
	}
	util := clamp01(in.Util)
	switch in.Governor {
	case "powersave":
		if in.HWP {
			t := r.target(hwpPowersaveKHz(in, r, util), relationLow)
			return t, t
		}
		t := r.target(r.lo, relationLow)
		return t, t
	case "schedutil":
		t := schedutilKHz(in, r, util)
		return t, t
	case "ondemand":
		t := ondemandKHz(in, r, util)
		return t, t
	case "conservative":
		return conservativeKHz(in, r, util, prevTargetKHz)
	case "userspace":
		setspeed := in.CurKHz
		if setspeed <= 0 {
			setspeed = r.hi
		}
		t := r.target(setspeed, relationLow)
		return t, t
	}
	// performance, and the implied performance of any other word.
	t := r.target(r.hi, relationHigh)
	return t, t
}

// ranKHz is the frequency the policy ran at over the previous step, the one
// Util was measured at: CurKHz, or MaxKHz before the governor's first
// request. It is not clamped to the present limits: when the DVFS fallback
// moved scaling_max_freq, or a cap held the policy below scaling_min_freq,
// Util was still measured at the frequency the policy ran at, as the
// kernel's frequency-invariant util is.
func ranKHz(in GovernorInput) float64 {
	if in.CurKHz <= 0 {
		return float64(max(in.MaxKHz, in.MinKHz))
	}
	return float64(in.CurKHz)
}

// schedutilKHz: get_next_freq takes cpuinfo_max_freq x util / max with a
// frequency-invariant util (cpufreq_schedutil.c:126-140, 165-179), after
// sugov_effective_cpu_perf added the 25 % headroom of map_util_perf
// (cpufreq_schedutil.c:181-196, include/linux/sched/cpufreq.h:32-35), and
// resolves the result to the lowest entry at or above it
// (cpufreq.c:563-567). Without frequency invariance the kernel approximates
// the same with the current frequency and the raw util
// (cpufreq_schedutil.c:150-158), which is the form computed here. The rate
// limit (rate_limit_us) is not applied: the caller's one-step lag stands in
// for it.
func schedutilKHz(in GovernorInput, r policyRange, util float64) int64 {
	raw := int64(1.25 * ranKHz(in) * util)
	return r.target(raw, relationLow)
}

// ondemandKHz: a load above up_threshold sets policy->max
// (cpufreq_ondemand.c:146-150, through dbs_freq_increase, :114-127);
// otherwise freq_next = cpuinfo.min_freq + load x (cpuinfo.max_freq -
// cpuinfo.min_freq) / 100, set to the closest entry
// (cpufreq_ondemand.c:152-167). The load is the busiest CPU's busy share of
// the sampling period in whole percent (cpufreq_governor.c:193, 222-223),
// which Util is. MinKHz and MaxKHz stand in for cpuinfo_min_freq and
// cpuinfo_max_freq: they are equal while nobody writes scaling_min_freq and
// scaling_max_freq. powersave_bias (0 by default, :30) is not modelled.
func ondemandKHz(in GovernorInput, r policyRange, util float64) int64 {
	up := tunableOr(in.Tunables.OndemandUpThreshold, defaultOndemandUpThreshold)
	load := loadPercent(util)
	if load > up {
		return r.target(r.hi, relationHigh)
	}
	lo, hi := max(in.MinKHz, 0), max(in.MaxKHz, in.MinKHz)
	next := lo + load*(hi-lo)/100
	return r.target(next, relationClosest)
}

// conservativeKHz runs cs_dbs_update once per sampling period in PeriodS
// (at least once). Per period (cpufreq_conservative.c:58-145): a load above
// up_threshold raises requested_freq by freq_step % of policy->max, up to
// policy->max, and sets the highest entry at or below it; a load below
// down_threshold lowers it by the same step, down to policy->min, and sets
// the lowest entry at or above it; a load in between changes nothing, so
// the policy stays at the current frequency prevKHz. A request outside the
// policy range restarts from the current frequency (:75-83), and the
// governor starts from the current frequency (:308-314).
//
// The load of each period is recomputed from the work of the step: Util was
// measured at the current frequency, so the same cycles at a higher
// frequency are a lower load (work in cycles). A saturated
// Util stays saturated, since the demand above it is unknown.
// Several idle periods without a sample lower the request one step each
// (:86-99), which the per-period loop already does. sampling_down_factor is
// 1 by default (:34), so every period may lower the frequency (:120-123).
// The sampling interval is the dbs sampling_rate attribute, which ondemand
// and conservative share (cpufreq_ondemand.c:323, cpufreq_conservative.c:252)
// and start at the same value (cpufreq_governor.c:449-451); GovernorTunables
// carries it as OndemandSamplingRateUS.
func conservativeKHz(in GovernorInput, r policyRange, util float64, prevKHz int64) (request, target int64) {
	up := tunableOr(in.Tunables.ConservativeUpThreshold, defaultConservativeUpThreshold)
	down := tunableOr(in.Tunables.ConservativeDownThreshold, defaultConservativeDownThreshold)
	step := tunableOr(in.Tunables.ConservativeFreqStep, defaultConservativeFreqStep) * r.hi / 100
	if step <= 0 {
		step = conservativeMinStepKHz
	}

	// The current frequency, within the policy range as the limits leave it
	// (cpufreq_policy_apply_limits, include/linux/cpufreq.h:644-652).
	cur := prevKHz
	if cur <= 0 {
		cur = in.CurKHz
	}
	if cur <= 0 {
		cur = r.hi
	}
	cur = r.target(cur, relationHigh)

	request = in.CurKHz
	if request <= 0 || request > r.hi || request < r.lo {
		request = cur
	}
	target = cur
	demand := util * float64(target)
	saturated := util >= 1

	for range conservativePeriods(in) {
		load := int64(100)
		if !saturated && target > 0 {
			load = min(loadPercent(demand/float64(target)), 100)
		}
		switch {
		case load > up && request < r.hi:
			request = min(request+step, r.hi)
			target = r.target(request, relationHigh)
		case load < down && request > r.lo:
			request = max(request-step, r.lo)
			target = r.target(request, relationLow)
		default:
			// Nothing moves, and the next period sees the same load.
			return request, target
		}
	}
	return request, target
}

// conservativePeriods is the number of sampling periods in PeriodS, at least
// one, at most maxGovernorPeriods.
func conservativePeriods(in GovernorInput) int {
	if in.PeriodS <= 0 {
		return 1
	}
	samplingUS := tunableOr(in.Tunables.OndemandSamplingRateUS, defaultDBSSamplingUS)
	n := math.Floor(in.PeriodS * 1e6 / float64(samplingUS))
	return int(min(max(n, 1), maxGovernorPeriods))
}

// hwpPowersaveKHz is the powersave algorithm of intel_pstate in active mode
// with HWP and of amd-pstate-epp, which the hardware runs from EPP. assumed:
//
//	f = min + (max - min) x c(epp) x min(1, util x (1 + gain x (255 - epp) / 255))
//	c(epp) = max(0.5, 1 - ceilingDrop x epp / 128)
//
// The utilization term lets a lower EPP reach a higher frequency at partial
// load; the ceiling c makes EPP matter at full load too, where the
// utilization term alone gives the maximum for every EPP.
func hwpPowersaveKHz(in GovernorInput, r policyRange, util float64) int64 {
	epp := float64(min(max(in.EPP, 0), 255))
	gain := math.Max(0, in.EPPGain)
	ceiling := math.Min(1, math.Max(0.5, 1-math.Max(0, in.EPPCeilingDrop)*epp/128))
	x := math.Min(1, util*(1+gain*(255-epp)/255))
	return r.lo + int64(math.Round(float64(r.hi-r.lo)*ceiling*x))
}

// loadPercent is a busy share in whole percent, as the kernel's integer
// division computes it (cpufreq_governor.c:193). The small offset keeps a
// share such as 0.29 from flooring to 28 through float rounding.
func loadPercent(share float64) int64 {
	return int64(math.Floor(clamp01(share)*100 + 1e-9))
}

func tunableOr(v, def int64) int64 {
	if v > 0 {
		return v
	}
	return def
}
