package main

import "time"

// The -o json document. Its field names are a public contract: scripts
// read them, so a rename needs the same care as a metric rename.
type jsonDoc struct {
	Time    time.Time   `json:"time"`
	Summary jsonSummary `json:"summary"`
	Nodes   []jsonNode  `json:"nodes,omitzero"`
	Pods    []jsonPod   `json:"pods,omitzero"`
}

type jsonSummary struct {
	Nodes              map[string]int `json:"nodes"`
	Pods               int            `json:"pods"`
	PerformancePods    int            `json:"performancePods"`
	MisplacedPods      int            `json:"misplacedPods"`
	PodsError          string         `json:"podsError,omitempty"`
	MeasuredPowerW     float64        `json:"measuredPowerW"`
	CappedPowerW       float64        `json:"cappedPowerW"`
	TdpW               float64        `json:"tdpW"`
	PowerTrendWMin     float64        `json:"powerTrendWPerMin"`
	AvgHeadroomPct     float64        `json:"avgHeadroomPct"`
	AvgCoolingPct      float64        `json:"avgCoolingStressPct"`
	AvgPsuPct          float64        `json:"avgPsuStressPct"`
	AvgPUE             float64        `json:"avgPUE,omitempty"`
	AgentsReporting    int            `json:"agentsReporting"`
	AgentsNotReporting int            `json:"agentsNotReporting"`
}

type jsonNode struct {
	Name            string   `json:"name"`
	Class           string   `json:"class"`
	CPUModel        string   `json:"cpuModel,omitempty"`
	Sockets         int      `json:"sockets,omitempty"`
	GPUModel        string   `json:"gpuModel,omitempty"`
	GPUs            int      `json:"gpus,omitempty"`
	PowerSource     string   `json:"powerSource,omitempty"`
	MeasuredPowerW  float64  `json:"measuredPowerW"`
	CappedPowerW    float64  `json:"cappedPowerW"`
	TdpW            float64  `json:"tdpW"`
	PowerTrendWMin  float64  `json:"powerTrendWPerMin"`
	CPUCapPct       *float64 `json:"cpuCapPct,omitempty"`
	GPUCapPct       *float64 `json:"gpuCapPct,omitempty"`
	HeadroomPct     float64  `json:"headroomPct"`
	CoolingPct      float64  `json:"coolingStressPct"`
	PsuPct          float64  `json:"psuStressPct"`
	CPURequestedPct float64  `json:"cpuRequestedPct"`
	MemRequestedPct float64  `json:"memoryRequestedPct"`
	GPURequestedPct *float64 `json:"gpuRequestedPct,omitempty"`
	Pods            int      `json:"pods"`
	PerformancePods int      `json:"performancePods"`
	AgentReporting  *bool    `json:"agentReporting,omitempty"`
	AgentReason     string   `json:"agentReason,omitempty"`
}

type jsonPod struct {
	Namespace string  `json:"namespace"`
	Name      string  `json:"name"`
	Class     string  `json:"class"`
	Node      string  `json:"node"`
	NodeClass string  `json:"nodeClass"`
	Misplaced bool    `json:"misplaced"`
	CPU       float64 `json:"cpu"`
	GPUs      float64 `json:"gpus,omitempty"`
}

// toJSON is the document of a command: status lists the nodes, pods the
// pods, node the one node and its pods.
func toJSON(c cluster, command string, args []string) jsonDoc {
	t := summarize(c)
	doc := jsonDoc{Time: c.taken.UTC(), Summary: jsonSummary{
		Nodes:              map[string]int{classPerformance: t.performance, classEco: t.eco, classDraining: t.draining, classUnknown: t.unknown},
		Pods:               t.pods,
		PerformancePods:    t.perfPods,
		MisplacedPods:      t.misplaced,
		MeasuredPowerW:     t.measuredW,
		CappedPowerW:       t.cappedW,
		TdpW:               t.tdpW,
		PowerTrendWMin:     t.trendWMin,
		AvgHeadroomPct:     t.headroom,
		AvgCoolingPct:      t.cooling,
		AvgPsuPct:          t.psu,
		AvgPUE:             t.pue,
		AgentsReporting:    t.agentsReporting,
		AgentsNotReporting: t.agentsMissing,
	}}
	if c.podsErr != nil {
		doc.Summary.PodsError = c.podsErr.Error()
	}
	wantNode := ""
	if command == "node" && len(args) == 1 {
		wantNode = args[0]
	}
	// A command's list is present even when empty, "pods": [], so a script
	// can tell no pods from a command that lists none.
	if command != "pods" {
		doc.Nodes = []jsonNode{}
		for _, n := range c.nodes {
			if wantNode != "" && n.name != wantNode {
				continue
			}
			doc.Nodes = append(doc.Nodes, nodeJSON(n))
		}
	}
	if command != "status" {
		doc.Pods = []jsonPod{}
		for _, p := range c.pods {
			if wantNode != "" && p.node != wantNode {
				continue
			}
			doc.Pods = append(doc.Pods, jsonPod{
				Namespace: p.namespace, Name: p.name, Class: p.class, Node: p.node, NodeClass: p.nodeClass,
				Misplaced: p.misplaced(), CPU: float64(p.cpuMilli) / 1000, GPUs: p.gpus,
			})
		}
	}
	return doc
}

func nodeJSON(n nodeView) jsonNode {
	j := jsonNode{
		Name: n.name, Class: n.class,
		CPUModel: n.cpuModel, Sockets: n.sockets, GPUModel: n.gpuModel, GPUs: n.gpus,
		PowerSource: n.powerSource, MeasuredPowerW: n.measuredW, CappedPowerW: n.cappedW, TdpW: n.tdpW,
		PowerTrendWMin: n.trendWMin,
		HeadroomPct:    n.headroom, CoolingPct: n.cooling, PsuPct: n.psu,
		CPURequestedPct: n.cpuAllocPct, MemRequestedPct: n.memAllocPct,
		Pods: n.pods, PerformancePods: n.perfPods,
	}
	if n.capKnown {
		cpu := n.cpuCapPct
		j.CPUCapPct = &cpu
		if n.hasGPU || n.gpuCapPlanned {
			gpu := n.gpuCapPct
			j.GPUCapPct = &gpu
		}
	}
	if n.hasGPU {
		v := n.gpuAllocPct
		j.GPURequestedPct = &v
	}
	if n.agent.known {
		v := n.agent.reporting
		j.AgentReporting = &v
		j.AgentReason = n.agent.reason
	}
	return j
}
