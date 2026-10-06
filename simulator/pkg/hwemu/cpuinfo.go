package hwemu

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// renderCPU is one logical CPU: its number and where it sits.
type renderCPU struct {
	ID     int
	Socket int
	Core   int
	Thread int
}

// renderCPUs lists the node's logical CPUs in number order. A verbatim
// cpuinfo is the truth when present; otherwise cpu.numbering places them.
// socket-major-smt-last, the only numbering, puts CPU k on socket
// (k mod S*C)/C, core k mod C and thread k/(S*C): the corpus machine
// xeon-4socket-nfd-labels matches on 192 of 192 CPUs.
func renderCPUs(p *Profile) []renderCPU {
	if p.CPU.CPUInfoVerbatim != "" {
		return renderCPUsFromCPUInfo(p.CPU.CPUInfoVerbatim)
	}
	s, c, t := p.CPU.Sockets.V, p.CPU.CoresPerSocket.V, p.CPU.ThreadsPerCore.V
	out := make([]renderCPU, 0, s*c*t)
	for k := 0; k < s*c*t; k++ {
		out = append(out, renderCPU{ID: k, Socket: (k % (s * c)) / c, Core: k % c, Thread: k / (s * c)})
	}
	return out
}

// renderCPUsFromCPUInfo reads processor, physical id and core id from each
// block. Threads of a core are numbered in the order they appear.
func renderCPUsFromCPUInfo(text string) []renderCPU {
	var out []renderCPU
	threads := map[[2]int]int{}
	for _, block := range strings.Split(text, "\n\n") {
		f := renderCPUInfoFields(block)
		id, err := strconv.Atoi(f["processor"])
		if err != nil {
			continue
		}
		socket, _ := strconv.Atoi(f["physical id"])
		core, _ := strconv.Atoi(f["core id"])
		key := [2]int{socket, core}
		out = append(out, renderCPU{ID: id, Socket: socket, Core: core, Thread: threads[key]})
		threads[key]++
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// renderCPUInfoFields parses one cpuinfo block into key and value, keeping
// the first value of a repeated key.
func renderCPUInfoFields(block string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, seen := out[k]; !seen {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// renderCPUInfo is the node's /proc/cpuinfo: the verbatim capture when the
// profile has one, otherwise one block per logical CPU in the x86 format
// (keys and tab padding as in the corpus captures), with hypervisor added to
// the flags of a virtual machine.
func renderCPUInfo(p *Profile) string {
	if p.CPU.CPUInfoVerbatim != "" {
		return p.CPU.CPUInfoVerbatim
	}
	c := p.CPU
	flags := strings.Fields(c.Flags)
	if c.Hypervisor && !renderContains(flags, "hypervisor") {
		flags = append(flags, "hypervisor")
	}
	var sb strings.Builder
	for _, cpu := range renderCPUs(p) {
		fmt.Fprintf(&sb, "processor\t: %d\n", cpu.ID)
		fmt.Fprintf(&sb, "vendor_id\t: %s\n", c.VendorID.V)
		fmt.Fprintf(&sb, "cpu family\t: %d\n", c.Family.V)
		fmt.Fprintf(&sb, "model\t\t: %d\n", c.Model.V)
		fmt.Fprintf(&sb, "model name\t: %s\n", c.ModelName.V)
		fmt.Fprintf(&sb, "stepping\t: %d\n", c.Stepping.V)
		fmt.Fprintf(&sb, "cpu MHz\t\t: %.3f\n", c.CPUMHz.V)
		fmt.Fprintf(&sb, "physical id\t: %d\n", cpu.Socket)
		fmt.Fprintf(&sb, "siblings\t: %d\n", c.CoresPerSocket.V*c.ThreadsPerCore.V)
		fmt.Fprintf(&sb, "core id\t\t: %d\n", cpu.Core)
		fmt.Fprintf(&sb, "cpu cores\t: %d\n", c.CoresPerSocket.V)
		if len(flags) > 0 {
			fmt.Fprintf(&sb, "flags\t\t: %s\n", strings.Join(flags, " "))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
