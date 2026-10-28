package main

import (
	"fmt"
	"strings"

	joulie "github.com/matbun/joulie/pkg/api"
	"github.com/matbun/joulie/pkg/governor"
	"github.com/matbun/joulie/simulator/pkg/hwemu"
)

// Grids, the values of Cell.Grid.
const (
	gridCPU = "cpu"
	gridGPU = "gpu"
)

// Load kinds of a CPU cell, and transfer kinds of a GPU cell.
const (
	loadIdle   = "idle"
	loadSteady = "steady"
	loadBursty = "bursty"
	loadJob    = "job"

	transferNone   = "none"
	transferPeer   = "peer"
	transferNIC    = "nic"
	transferGather = "gather"
)

// Cell is one run: a fresh hwemu node of Profile, the arm set on it, then
// the load. Sens is empty for the base profile, else fact=value of the
// sensitivity variant it runs on.
type Cell struct {
	Grid    string
	Seed    uint64
	Profile string
	Sens    string
	sensFn  func(*hwemu.Profile) bool

	// The arm. CPU: Preset (or ArmDefault) and Boost. GPU: Preset,
	// CeilingPct for efficient, CapPct and Gate.
	Preset     string
	Boost      string
	CeilingPct float64
	CapPct     float64
	Gate       string

	// The load. Load is idle, steady or bursty on CPU cells, idle or job on
	// GPU cells; Pct is the steady load in percent; Class and Intensity the
	// workload class.
	Load      string
	Pct       float64
	Class     string
	Intensity Intensity
	// GPU jobs: GPUs, Transfer (none, peer, nic, gather) and
	// TransferIntensity. A peer job sends a ring over its GPUs, each busy;
	// a nic job receives from the host into each GPU; a gather job computes
	// on GPUs 1..GPUs-1, which each send to GPU 0, idle: the receiver a
	// demand-driven governor never raises.
	GPUs              int
	Transfer          string
	TransferIntensity float64
}

// ID names the cell: unique in a sweep, stable across runs, and usable as a
// file name and as the cell's host name in the OTLP export.
func (c Cell) ID() string {
	parts := []string{c.Grid, c.Profile, c.armID()}
	switch {
	case c.Load == loadSteady:
		parts = append(parts, "steady"+fmtNum(c.Pct), c.Class)
	case c.Load == loadJob:
		parts = append(parts, c.Class, fmt.Sprintf("%dgpu", c.GPUs))
		if c.Transfer != transferNone {
			parts = append(parts, c.Transfer+fmtNum(c.TransferIntensity))
		}
	case c.Load == loadBursty:
		parts = append(parts, "bursty", c.Class)
	default:
		parts = append(parts, c.Load)
	}
	if c.Sens != "" {
		parts = append(parts, "sens-"+c.Sens)
	}
	parts = append(parts, fmt.Sprintf("s%d", c.Seed))
	return strings.ReplaceAll(strings.Join(parts, "_"), "=", "-")
}

// armID spells the arm in a cell id and in the CSV arm column.
func (c Cell) armID() string {
	switch c.Grid {
	case gridCPU:
		if c.Boost != "" {
			return c.Preset + "-boost-" + c.Boost
		}
		return c.Preset
	default:
		s := c.Preset
		if c.Preset == joulie.GPUGovernorPresetEfficient {
			s += fmtNum(c.CeilingPct)
		}
		return fmt.Sprintf("%s-cap%s-gate-%s", s, fmtNum(c.CapPct), c.Gate)
	}
}

// expand lists every cell of c, in a fixed order: grid, sensitivity variant,
// profile, arm, load, seed.
func (c *Config) expand() []Cell {
	var out []Cell
	type variant struct {
		name, grid string // grid "" for the base profiles, which both grids run
		fn         func(*hwemu.Profile) bool
	}
	variants := []variant{{}}
	for _, s := range c.Sensitivity {
		f := sensitivityFacts[s.Fact]
		for _, v := range s.Values {
			v := v
			variants = append(variants, variant{s.Fact + "=" + fmtNum(v), f.grid, func(p *hwemu.Profile) bool { return f.set(p, v) }})
		}
	}
	for _, vr := range variants {
		if g := c.CPU; g != nil && (vr.grid == "" || vr.grid == gridCPU) {
			for _, prof := range g.Profiles {
				for _, arm := range g.Arms {
					base := Cell{Grid: gridCPU, Profile: prof, Sens: vr.name, sensFn: vr.fn, Preset: arm.Preset, Boost: arm.Boost}
					var loads []Cell
					if g.Idle {
						l := base
						l.Load = loadIdle
						loads = append(loads, l)
					}
					for _, pct := range g.SteadyPct {
						for _, class := range sortedClasses(g.Classes) {
							l := base
							l.Load, l.Pct, l.Class, l.Intensity = loadSteady, pct, class, g.Classes[class]
							loads = append(loads, l)
						}
					}
					if b := g.Bursty; b != nil {
						for _, class := range b.Classes {
							l := base
							l.Load, l.Class, l.Intensity = loadBursty, class, g.Classes[class]
							loads = append(loads, l)
						}
					}
					out = appendSeeds(out, loads, c.Seeds)
				}
			}
		}
		if g := c.GPU; g != nil && (vr.grid == "" || vr.grid == gridGPU) {
			for _, prof := range g.Profiles {
				for _, arm := range g.Arms {
					ceilings := arm.CeilingPct
					if arm.Preset == joulie.GPUGovernorPresetEfficient && len(ceilings) == 0 {
						ceilings = []float64{governor.DefaultCeilingPctOfMax}
					}
					if len(ceilings) == 0 {
						ceilings = []float64{0}
					}
					for _, ceil := range ceilings {
						for _, capPct := range g.CapsPct {
							for _, gate := range g.ReceiverGates {
								base := Cell{Grid: gridGPU, Profile: prof, Sens: vr.name, sensFn: vr.fn, Preset: arm.Preset,
									CeilingPct: ceil, CapPct: capPct, Gate: gate}
								var loads []Cell
								// A gate changes peer transfers only: the
								// profile's gate covers every other load.
								peerOnly := gate != hwemu.ReceiverGateProfile
								if g.Idle && !peerOnly {
									l := base
									l.Load = loadIdle
									loads = append(loads, l)
								}
								for _, class := range sortedClasses(g.Classes) {
									for _, j := range g.Jobs {
										l := base
										l.Load, l.Class, l.Intensity, l.GPUs = loadJob, class, g.Classes[class], j.GPUs
										if len(j.Peer) == 0 && len(j.NIC) == 0 && len(j.Gather) == 0 && !peerOnly {
											l.Transfer = transferNone
											loads = append(loads, l)
										}
										for _, p := range j.Peer {
											l.Transfer, l.TransferIntensity = transferPeer, p
											loads = append(loads, l)
										}
										for _, p := range j.Gather {
											l.Transfer, l.TransferIntensity = transferGather, p
											loads = append(loads, l)
										}
										if peerOnly {
											continue
										}
										for _, p := range j.NIC {
											l.Transfer, l.TransferIntensity = transferNIC, p
											loads = append(loads, l)
										}
									}
								}
								out = appendSeeds(out, loads, c.Seeds)
							}
						}
					}
				}
			}
		}
	}
	return out
}

func appendSeeds(out, loads []Cell, seeds []uint64) []Cell {
	for _, l := range loads {
		for _, s := range seeds {
			l.Seed = s
			out = append(out, l)
		}
	}
	return out
}
