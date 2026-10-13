package semconv

// Names of calibration sweeps. A sweep holds a node at a series of
// setpoints (a load, a frequency ceiling, a power limit), records the node's
// hw.* readings together with its CPU utilization and the setpoints it held,
// and hwcalib fit (simulator/pkg/calib) turns the record into a local
// hardware profile. The readings use the hardware names above; this file
// adds the CPU utilization a collector reports and the setpoint record.

// Attributes of a sweep. cpu.mode and cpu.logical_number come from the
// semantic conventions v1.44.0 registry (model/cpu/registry.yaml); the
// joulie.calibration.* attributes are Joulie's own.
const (
	// AttrCPUMode is the mode a system.cpu.utilization point measures.
	// hwcalib reads CPUModeIdle only: utilization is one minus idle.
	AttrCPUMode = "cpu.mode"
	// AttrCPULogicalNumber is the logical CPU of a per-CPU
	// system.cpu.utilization point, opt-in in the conventions. Without it
	// a point covers the whole host.
	AttrCPULogicalNumber = "cpu.logical_number"
	// AttrCalibrationKnob names what a joulie.calibration.setpoint point
	// holds: one of the Knob* values.
	AttrCalibrationKnob = "joulie.calibration.knob"
	// AttrCalibrationClass is the workload class of the load a
	// utilization knob drives, as the simulator's power models name it,
	// such as cpu.compute_bound or gpu.memory_bound. Empty means the
	// models' default weights.
	AttrCalibrationClass = "joulie.calibration.class"
	// AttrCalibrationMemoryIntensity is the memory intensity of that load,
	// a double in 0..1.
	AttrCalibrationMemoryIntensity = "joulie.calibration.memory_intensity"
)

// Attribute values.
const (
	// CPUModeIdle is the cpu.mode of idle time.
	CPUModeIdle = "idle"

	// Knobs of a sweep, the values of joulie.calibration.knob, and the unit
	// of the setpoint each one holds.
	//
	// KnobCPUUtilization is the busy fraction of every logical CPU, 0..1.
	KnobCPUUtilization = "cpu.utilization"
	// KnobCPUFrequencyMax is the scaling_max_freq written to every cpufreq
	// policy, in Hz.
	KnobCPUFrequencyMax = "cpu.frequency.max"
	// KnobCPUPowerLimit is the package power limit written to every
	// package (RAPL PL1 or the HSMP socket cap), in W.
	KnobCPUPowerLimit = "cpu.power.limit"
	// KnobGPUUtilization is the busy fraction of every GPU, 0..1.
	KnobGPUUtilization = "gpu.utilization"
	// KnobGPUPowerLimit is the power limit written to every GPU, in W.
	KnobGPUPowerLimit = "gpu.power.limit"
)

func init() {
	registerAttributes(
		AttrCPUMode, AttrCPULogicalNumber,
		AttrCalibrationKnob, AttrCalibrationClass, AttrCalibrationMemoryIntensity,
	)
}

// Knobs returns every knob value, in the order of the constants.
func Knobs() []string {
	return []string{KnobCPUUtilization, KnobCPUFrequencyMax, KnobCPUPowerLimit, KnobGPUUtilization, KnobGPUPowerLimit}
}

// Metrics of a sweep. system.cpu.utilization is the conventions' metric
// (model/system/metrics.yaml); joulie.calibration.setpoint is Joulie's.
var (
	SystemCPUUtilization = Register(Metric{
		Name: "system.cpu.utilization", Unit: "1", Owner: OwnerSemconv, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrCPUMode, AttrCPULogicalNumber},
		Brief: "For each logical CPU, the utilization is calculated as the change in cumulative CPU time (cpu.time) over a measurement interval, divided by the elapsed time.",
	})
	// JoulieCalibrationSetpoint carries one point per knob a sweep holds,
	// at every sample. The unit depends on the knob (see the Knob*
	// values), hence the annotation.
	JoulieCalibrationSetpoint = Register(Metric{
		Name: "joulie.calibration.setpoint", Unit: "{setpoint}", Owner: OwnerJoulie, Stability: StabilityDevelopment,
		Kind: Gauge, Value: Double, Attributes: []string{AttrCalibrationKnob, AttrCalibrationClass, AttrCalibrationMemoryIntensity},
		Brief: "A setpoint a calibration sweep holds the node at: the value of the knob joulie.calibration.knob names.",
	})
)
