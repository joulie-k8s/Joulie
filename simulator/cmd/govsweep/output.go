package main

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
)

// The CSV columns of each grid. One row per cell, in the grid's expansion
// order. Energies in J and powers in W over the measured window (the job,
// or DurationS idle), time in s, edp_js = host_j x time_s. cap_w is the
// limit the job's GPUs held when it started, the cap the board took.
var (
	cpuColumns = []string{
		"cell", "seed", "profile", "sens", "arm", "preset", "boost", "result", "native", "writes", "status",
		"load", "pct", "class", "time_s", "host_j", "cpu_j", "memory_j", "mean_host_w",
		"mean_cpu_util", "mean_cpu_mhz", "min_cpu_throughput", "burst_p50_ms", "burst_p95_ms", "edp_js",
	}
	gpuColumns = []string{
		"cell", "seed", "profile", "sens", "arm", "preset", "ceiling_pct", "cap_pct", "gate", "result", "native", "writes", "status",
		"load", "class", "gpus", "transfer", "transfer_intensity", "cap_w", "time_s", "host_j", "gpu_j", "cpu_j", "mean_host_w",
		"mean_compute_mhz", "mean_mem_mhz", "min_gpu_throughput", "min_peer_mul", "peer_rx_bytes", "peer_gbps", "edp_js",
	}
)

// num spells a measured value with a fixed precision, so a golden row is
// stable; a cell that did not run leaves its measured columns empty.
func num(v float64, ran bool) string {
	if !ran {
		return ""
	}
	return strconv.FormatFloat(v, 'f', 4, 64)
}

// record is r as a CSV record of its grid's columns.
func (r Row) record() []string {
	c := r.Cell
	ran := r.Status == statusOK || r.Status == statusTimeout
	common := []string{c.ID(), strconv.FormatUint(c.Seed, 10), c.Profile, c.Sens, c.armID(), c.Preset}
	if c.Grid == gridCPU {
		out := append(common, c.Boost, r.Arm.Result, r.Arm.Native, strconv.Itoa(r.Arm.Writes), r.Status,
			c.Load, fmtNum(c.Pct), c.Class,
			num(r.TimeS, ran), num(r.HostJ, ran), num(r.CPUJ, ran), num(r.MemoryJ, ran), num(r.MeanHostW, ran),
			num(r.MeanCPUUtil, ran), num(r.MeanCPUMHz, ran), num(r.MinThroughput, ran),
			num(r.BurstP50MS, ran && c.Load == loadBursty), num(r.BurstP95MS, ran && c.Load == loadBursty),
			num(r.HostJ*r.TimeS, ran))
		return out
	}
	ceil := ""
	if c.CeilingPct > 0 {
		ceil = fmtNum(c.CeilingPct)
	}
	transfer, intensity := c.Transfer, ""
	if c.Transfer != "" && c.Transfer != transferNone {
		intensity = fmtNum(c.TransferIntensity)
	}
	gpus := ""
	if c.GPUs > 0 {
		gpus = strconv.Itoa(c.GPUs)
	}
	return append(common, ceil, fmtNum(c.CapPct), c.Gate, r.Arm.Result, r.Arm.Native, strconv.Itoa(r.Arm.Writes), r.Status,
		c.Load, c.Class, gpus, transfer, intensity, num(r.CapW, ran && c.Load == loadJob),
		num(r.TimeS, ran), num(r.HostJ, ran), num(r.GPUJ, ran), num(r.CPUJ, ran), num(r.MeanHostW, ran),
		num(r.MeanComputeMHz, ran), num(r.MeanMemMHz, ran), num(r.MinThroughput, ran), num(r.MinPeerMul, ran),
		num(r.PeerRxBytes, ran), num(r.PeerGBps, ran), num(r.HostJ*r.TimeS, ran))
}

// writeCSV writes the header of grid and the rows of that grid.
func writeCSV(w io.Writer, grid string, rows []Row) error {
	cw := csv.NewWriter(w)
	cols := cpuColumns
	if grid == gridGPU {
		cols = gpuColumns
	}
	if err := cw.Write(cols); err != nil {
		return err
	}
	for _, r := range rows {
		if r.Cell.Grid != grid || r.Status == "" {
			continue
		}
		if err := cw.Write(r.record()); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// writeJSONLine writes v as one line of JSON, the OTel file exporter's
// format (one export request per line).
func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
