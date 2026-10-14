package semconv

// Names of the governor stage: the bytes a GPU moves, the clocks of its
// domains, and the governor state of CPUs and GPUs. The semantic
// conventions v1.44.0 define hw.gpu.io (model/hardware/gpu-metrics.yaml) and
// network.io.direction (model/network/registry.yaml), and no GPU clock and
// no governor, so those are Joulie's own. The GPU clock metrics mirror
// hw.cpu.speed and hw.cpu.speed.limit (model/hardware/cpu-metrics.yaml): a
// gauge in Hz, with hw.limit_type on the limit.

// Attributes of the governor stage.
const (
	// AttrNetworkIODirection is the direction of hw.gpu.io: receive or
	// transmit. Required on hw.gpu.io by the conventions.
	AttrNetworkIODirection = "network.io.direction"
	// AttrJoulieGPUIOPath is where the bytes of hw.gpu.io come from or go
	// to: another GPU of the node, the host, or a network interface.
	AttrJoulieGPUIOPath = "joulie.hw.gpu.io.path"
	// AttrJoulieClockDomain is the GPU clock domain of joulie.hw.gpu.speed
	// and joulie.hw.gpu.speed.limit.
	AttrJoulieClockDomain = "joulie.hw.clock.domain"
	// AttrJoulieGovernorPreset is the vendor-neutral preset of a
	// joulie.hw.governor point, empty when no preset maps to the native
	// state.
	AttrJoulieGovernorPreset = "joulie.hw.governor.preset"
	// AttrJoulieGovernorNative is the native state of a joulie.hw.governor
	// point, in the driver's own words, such as schedutil or high.
	AttrJoulieGovernorNative = "joulie.hw.governor.native"
)

// Attribute values of the governor stage.
const (
	// network.io.direction of hw.gpu.io.
	NetworkIODirectionReceive  = "receive"
	NetworkIODirectionTransmit = "transmit"
	// joulie.hw.gpu.io.path of hw.gpu.io: another GPU of the same node,
	// host memory, or a network interface.
	GPUIOPathPeer = "peer"
	GPUIOPathHost = "host"
	GPUIOPathNIC  = "nic"
	// joulie.hw.clock.domain of the GPU clock metrics: the compute clock
	// (AMD sclk, NVIDIA graphics/SM), the memory clock (AMD mclk or uclk,
	// NVIDIA memory), the data fabric (AMD fclk) and the SoC clock (AMD
	// socclk).
	ClockDomainGraphics = "graphics"
	ClockDomainMemory   = "memory"
	ClockDomainFabric   = "fabric"
	ClockDomainSoC      = "soc"
)

func init() {
	registerAttributes(
		AttrNetworkIODirection, AttrJoulieGPUIOPath, AttrJoulieClockDomain,
		AttrJoulieGovernorPreset, AttrJoulieGovernorNative,
	)
}

// ClockDomains returns every joulie.hw.clock.domain value.
func ClockDomains() []string {
	return []string{ClockDomainGraphics, ClockDomainMemory, ClockDomainFabric, ClockDomainSoC}
}

// Metrics of the governor stage.
var (
	HwGPUIO = Register(Metric{
		Name: "hw.gpu.io", Unit: "By", Owner: OwnerSemconv,
		Kind: Counter, Value: Int,
		Attributes: []string{AttrHwID, AttrHwName, AttrHwVendor, AttrHwModel, AttrNetworkIODirection, AttrJoulieGPUIOPath},
		Brief:      "Received and transmitted bytes by the GPU.",
	})
	JoulieHwGPUSpeed = Register(Metric{
		Name: "joulie.hw.gpu.speed", Unit: "Hz", Owner: OwnerJoulie,
		Kind: Gauge, Value: Int, Attributes: []string{AttrHwID, AttrHwVendor, AttrHwModel, AttrJoulieClockDomain},
		Brief: "Current frequency of a GPU clock domain.",
	})
	JoulieHwGPUSpeedLimit = Register(Metric{
		Name: "joulie.hw.gpu.speed.limit", Unit: "Hz", Owner: OwnerJoulie,
		Kind: Gauge, Value: Int, Attributes: []string{AttrHwID, AttrHwVendor, AttrHwModel, AttrJoulieClockDomain, AttrHwLimitType},
		Brief: "Maximum frequency of a GPU clock domain: the hardware maximum, and the maximum in effect after limits and locks.",
	})
	// JoulieHwGovernor is 1 for the governor state a component is in and 0
	// for the one it left, as hw.status reports a state.
	JoulieHwGovernor = Register(Metric{
		Name: "joulie.hw.governor", Unit: "1", Owner: OwnerJoulie,
		Kind: UpDownCounter, Value: Int, Attributes: []string{AttrHwID, AttrHwType, AttrJoulieGovernorPreset, AttrJoulieGovernorNative},
		Brief: "Governor state of a component: 1 for the state it is in, 0 for the state it left.",
	})
)
