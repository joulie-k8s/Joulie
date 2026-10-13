// Package calib fits the physics of a hardware profile to a measured
// calibration sweep, for hwcalib fit (simulator/cmd/hwcalib).
//
// A sweep holds one node at a series of setpoints (CPU and GPU load,
// a frequency ceiling, a power limit) and records, at every sample, the
// node's readings under the names of pkg/otel/semconv: hw.energy, hw.power,
// hw.host.*, hw.cpu.speed and hw.cpu.speed.limit, hw.gpu.utilization and
// joulie.hw.power.limit as the simulator emits them, the CPU utilization
// as system.cpu.utilization{cpu.mode=idle}, per logical CPU with the opt-in
// cpu.logical_number or for the whole host, and the setpoints it held as
// joulie.calibration.setpoint, one point per knob. ReadOTLP reads the OTLP
// JSON Lines form, one MetricsData per line, and ReadCSV a CSV form with the
// same names. No common exporter emits hw.energy{hw.type=cpu} from RAPL, so
// a sweep comes from an on-node collector or from a converter.
//
// Segments splits the sweep where its setpoints change, and each segment's
// steady window drops max(10 s, 3 times the base profile's largest settling
// time of the domain) and keeps at least 5 samples. Fit then fits, from the
// steady windows, the analytic CPU model (idle, maximum and the two
// exponents), DRAM and platform power and the GPU board model (idle and the
// natural maximum, by inverting the model at the recorded class), and,
// from the setpoint steps, the settling times. LocalProfile writes the
// result as a local profile that extends the base profile and cites the
// sweep as a measured source; WriteProfile refuses any directory inside the
// repository, since a measured profile may name the machine it describes.
package calib
