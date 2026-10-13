package semconv

// Attributes of the simulator's node and hardware events.
const (
	// AttrSimProfile is the hardware profile the cluster spec gives a node.
	AttrSimProfile = "joulie.sim.profile"
	// AttrSimPool is the cluster spec pool of a node.
	AttrSimPool = "joulie.sim.pool"
	// AttrSimTreeResumed is true when the node's tree existed and was
	// resumed, false when it was rendered for this start.
	AttrSimTreeResumed = "joulie.sim.tree.resumed"
	// AttrSimTreeAdded is how many missing entries a resume put back.
	AttrSimTreeAdded = "joulie.sim.tree.added"
	// AttrSimNodeProfile is the profile the Kubernetes Node object names, when
	// it differs from AttrSimProfile.
	AttrSimNodeProfile = "joulie.sim.node.profile"
	// AttrSimFile is the file of the event, relative to the node root.
	AttrSimFile = "joulie.sim.file"
	// AttrSimOutcome is the emulator's event kind, such as accepted,
	// clamped or reset-persistence-off.
	AttrSimOutcome = "joulie.sim.outcome"
	// AttrSimValueWritten is the content found in the file.
	AttrSimValueWritten = "joulie.sim.value.written"
	// AttrSimValueEffective is the value in effect after the step.
	AttrSimValueEffective = "joulie.sim.value.effective"
)

// error.type values of the simulator's events.
const (
	ErrorTypeNodeMissing        = "node_missing"
	ErrorTypeProfileMismatch    = "profile_mismatch"
	ErrorTypeRevertedOutOfRange = "reverted_out_of_range"
	ErrorTypeParseError         = "parse_error"
	ErrorTypeRestored           = "restored"
)

func init() {
	registerAttributes(
		AttrSimProfile, AttrSimPool, AttrSimTreeResumed, AttrSimTreeAdded, AttrSimNodeProfile,
		AttrSimFile, AttrSimOutcome, AttrSimValueWritten, AttrSimValueEffective,
	)
}

// simHardwareEventAttributes are the attributes of every event about a write
// to an emulated node's file.
var simHardwareEventAttributes = []string{
	AttrHwID, AttrHwType, AttrSimFile, AttrSimOutcome, AttrSimValueWritten, AttrSimValueEffective,
}

// Events of the simulator's emulated nodes. A record is emitted on the node's
// resource, so the node is not an attribute.
var (
	EventSimNodeReady = RegisterEvent(Event{
		Name: "joulie.sim.node.ready", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: []string{AttrSimProfile, AttrSimPool, AttrSimTreeResumed, AttrSimTreeAdded},
		Brief:      "An emulated node's tree is in place and its first step ran, once per node per simulator start.",
	})
	EventSimNodeMismatch = RegisterEvent(Event{
		Name: "joulie.sim.node.mismatch", SeverityNumber: SeverityWarn, SeverityText: SeverityTextWarn,
		Attributes: []string{AttrErrorType, AttrSimProfile, AttrSimNodeProfile},
		Brief:      "The Kubernetes Node of a spec node is missing or names another profile; emitted once per change.",
	})
	EventSimCapApplied = RegisterEvent(Event{
		Name: "joulie.sim.cap.applied", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: simHardwareEventAttributes,
		Brief:      "A power limit written to an emulated node took effect, possibly quantized or clamped.",
	})
	EventSimSettingApplied = RegisterEvent(Event{
		Name: "joulie.sim.setting.applied", SeverityNumber: SeverityInfo, SeverityText: SeverityTextInfo,
		Attributes: simHardwareEventAttributes,
		Brief:      "A setting other than a power limit, such as a frequency bound or governor, took effect.",
	})
	EventSimWriteRejected = RegisterEvent(Event{
		Name: "joulie.sim.write.rejected", SeverityNumber: SeverityWarn, SeverityText: SeverityTextWarn,
		Attributes: append(append([]string(nil), simHardwareEventAttributes...), AttrErrorType),
		Brief:      "A write to an emulated node was reverted: out of range, unparsable, or to a file only the emulator writes.",
	})
	EventSimCapReset = RegisterEvent(Event{
		Name: "joulie.sim.cap.reset", SeverityNumber: SeverityWarn, SeverityText: SeverityTextWarn,
		Attributes: simHardwareEventAttributes,
		Brief:      "A GPU power limit went back to its default with persistence mode off and no client; written is the old limit, effective the default.",
	})
)
