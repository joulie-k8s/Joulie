// Package simcluster is the library behind the simulated cluster harness: a
// k3s server in Docker, fake Nodes rendered from a cluster spec and kept
// Ready by KWOK, the simulator in emulate mode keeping one hwemu tree per
// Node, one emulated Joulie agent per eligible Node reading and writing that
// tree, and Prometheus receiving the simulator's OTLP. The scripts under
// scripts/simcluster drive the cluster; the simcluster command gives them
// what they cannot compute in shell:
//
//   - Placement. Which spec Nodes the Joulie agent DaemonSet would reach,
//     judged from the chart's real DaemonSet render with the same values
//     files as the install: the node selector must match, every NoSchedule
//     and NoExecute taint must be tolerated (counting the tolerations the
//     DaemonSet controller adds), and the architecture must match the image.
//     The KWOK taint every fake Node carries is ignored: it exists only to
//     keep infrastructure pods off fake Nodes, and a real Node does not have
//     it. Every other Node gets an Exclusion with its reason.
//   - Emulated agents. One EmulatedAgent per eligible Node, the entries of
//     the Joulie chart's agent.emulated.nodes value.
//   - Telemetry. For a Mode, a Scope and a Timing: the Prometheus and
//     Collector configuration, the simulator and Joulie values files, the
//     query that says every measured Node has reported, and the Checks that
//     compare what Joulie reads with the emulator's truth (NodeTruth and
//     FacilityTruth, taken from the simulator's /debug/emulate snapshot).
//   - Scale. A cluster spec of N Nodes of one profile.
//
// Node rendering is not here: package clusterspec is the only renderer of
// fake Nodes, for the simulator and the harness alike.
//
// # The harness section of a cluster spec
//
// Package clusterspec keeps spec.harness opaque; this package owns its
// schema, Harness. Each entry under pools names a pool of the spec:
//
//	harness:
//	  pools:
//	    nvl-nodriver: {toolDelivery: none}
//
// A pool that is not listed gets ToolDeliveryMounted.
//
// # Names
//
// Mode, Scope and timing names are the values of the scripts' variables
// SIMCLUSTER_TELEMETRY, SIMCLUSTER_POWER_SCOPE and SIMCLUSTER_TIMING, and of
// the simcluster command's --mode, --scope and --timing flags. A timing name
// selects scripts/simcluster/timing/<name>.env. Every metric and label name
// in a generated query comes from package promname applied to an entry of
// package semconv (both under pkg/otel), or is a Kepler name pinned to a
// Kepler release.
package simcluster
