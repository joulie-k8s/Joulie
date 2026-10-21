package api

import "time"

// StatusRewriteInterval is how often a writer rewrites a status that has not
// changed: the agent skips an unchanged NodeHardware or controlStatus write
// for at most this long, which bounds how long a lost object stays
// unrepaired.
const StatusRewriteInterval = 5 * time.Minute

// The NodeTwin condition the controller manager writes about the node's
// agent, in status.conditions, and its reasons.
const (
	// ConditionAgentReporting is True while an agent publishes the node's
	// NodeHardware. False means the caps in spec are planned and nothing
	// enforces them.
	ConditionAgentReporting = "AgentReporting"
	// ReasonNodeHardwareCurrent: the NodeHardware was written within
	// AgentReportingStaleAfter.
	ReasonNodeHardwareCurrent = "NodeHardwareCurrent"
	// ReasonNoNodeHardware: the node has no NodeHardware, so no agent has
	// run there.
	ReasonNoNodeHardware = "NoNodeHardware"
	// ReasonNodeHardwareStale: the last NodeHardware write is older than
	// AgentReportingStaleAfter, so the agent that wrote it stopped.
	ReasonNodeHardwareStale = "NodeHardwareStale"
)

// AgentReportingStaleAfter is the age of a NodeHardware write past which no
// agent counts as running on the node. A running agent rewrites it at least
// every StatusRewriteInterval plus one reconcile interval.
const AgentReportingStaleAfter = 3 * StatusRewriteInterval
