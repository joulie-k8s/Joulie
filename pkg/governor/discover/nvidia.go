package discover

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/matbun/joulie/pkg/governor"
)

// Runner runs a tool and returns its combined output, as the agent's
// command runner and the simulator's fake tools do. A failed run returns an
// error whose text includes the output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// NvidiaArch is the pkg/governor architecture of a node's NVIDIA GPUs, from
// the node's labels and the GPU product name; empty when it has no mapping
// table. The compute capability labels of GPU feature discovery come first:
// its gpu.family label names compute 8.9 (Ada) ampere on some releases. The
// product name comes next, the family label last.
func NvidiaArch(nodeLabels map[string]string, product string) string {
	if major, err := strconv.Atoi(nodeLabels["nvidia.com/gpu.compute.major"]); err == nil {
		minor, _ := strconv.Atoi(nodeLabels["nvidia.com/gpu.compute.minor"])
		switch {
		case major == 8 && minor == 9:
			return governor.NVArchAda
		case major == 8:
			return governor.NVArchAmpere
		case major == 9:
			return governor.NVArchHopper
		case major == 10 || major == 12:
			return governor.NVArchBlackwell
		}
		return ""
	}
	for _, tok := range strings.FieldsFunc(strings.ToUpper(product), func(r rune) bool { return r == ' ' || r == '-' }) {
		switch tok {
		case "A100", "A800", "A40", "A30", "A10", "A16", "A2":
			return governor.NVArchAmpere
		case "L4", "L40", "L40S", "L20", "ADA":
			return governor.NVArchAda
		case "H100", "H200", "H800", "H20", "GH200":
			return governor.NVArchHopper
		case "B100", "B200", "B300", "GB200", "GB300", "BLACKWELL":
			return governor.NVArchBlackwell
		}
	}
	switch strings.ToLower(nodeLabels["nvidia.com/gpu.family"]) {
	case "ampere":
		return governor.NVArchAmpere
	case "ada", "ada-lovelace":
		return governor.NVArchAda
	case "hopper":
		return governor.NVArchHopper
	case "blackwell":
		return governor.NVArchBlackwell
	}
	return ""
}

// The clock query's fields, in order, and the two names of its last one.
// nvidia-smi names the reasons a clock is held clocks_event_reasons.active
// from R535 on and keeps clocks_throttle_reasons.active as an alias; R525
// and older know only the throttle name and reject the other ("Field
// \"clocks_event_reasons.active\" is not a valid field to query.", exit 2).
// The query asks for the throttle name and, should a driver reject it,
// asks again with the event name.
const (
	clockQueryFields      = "pci.bus_id,clocks.sm,clocks.mem,clocks.max.sm,clocks.max.mem,pstate,"
	reasonsFieldThrottle  = "clocks_throttle_reasons.active"
	reasonsFieldEvent     = "clocks_event_reasons.active"
	invalidFieldText      = "is not a valid field to query"
	clockQueryFormat      = "--format=csv,noheader,nounits"
	supportedClocksSelect = "SUPPORTED_CLOCKS"
)

// clockQueryArgs is the nvidia-smi clock query with the reasons field named
// reasonsField.
func clockQueryArgs(reasonsField string) []string {
	return []string{"--query-gpu=" + clockQueryFields + reasonsField, clockQueryFormat}
}

// NvidiaClockQueryArgs is the clock query the agent runs first, and
// NvidiaClockQueryFallbackArgs the one it runs when a driver rejects the
// first one's reasons field.
var (
	NvidiaClockQueryArgs         = clockQueryArgs(reasonsFieldThrottle)
	NvidiaClockQueryFallbackArgs = clockQueryArgs(reasonsFieldEvent)
)

// NvidiaSupportedClocksArgs lists the graphics and memory clocks every GPU
// accepts in a lock.
var NvidiaSupportedClocksArgs = []string{"-q", "-d", supportedClocksSelect}

// NvidiaClocks is one GPU's row of the clock query. SMMHz and MemMHz are
// the current clocks, valid when ClocksOK; Reasons is the bitmask of the
// reasons a clock is held (NVML clocksEventReason*), valid when ReasonsOK;
// a maximum that does not parse is zero.
type NvidiaClocks struct {
	BusID               string
	SMMHz, MemMHz       float64
	MaxSMMHz, MaxMemMHz float64
	Reasons             uint64
	ClocksOK, ReasonsOK bool
}

// splitCSV splits one line of nvidia-smi's csv output into trimmed fields.
func splitCSV(line string) []string {
	parts := strings.Split(line, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), "\""))
	}
	return parts
}

// parseMHz reads "1980 MHz" (or a bare number) as MHz.
func parseMHz(s string) (int, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return int(v), true
}

// FieldRejected reports whether err is a run of nvidia-smi that rejected a
// --query-gpu field the driver does not know, as R525 rejects the event
// name of the clock reasons; QueryNvidiaClocks then asks again.
func FieldRejected(err error) bool {
	return err != nil && strings.Contains(err.Error(), invalidFieldText)
}

// QueryNvidiaClocks runs the clock query and parses one row per GPU listed
// with a bus id.
func QueryNvidiaClocks(ctx context.Context, run Runner) ([]NvidiaClocks, error) {
	out, err := run(ctx, "nvidia-smi", NvidiaClockQueryArgs...)
	if FieldRejected(err) {
		out, err = run(ctx, "nvidia-smi", NvidiaClockQueryFallbackArgs...)
	}
	if err != nil {
		return nil, err
	}
	var rows []NvidiaClocks
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := splitCSV(ln)
		if len(p) < 5 || p[0] == "" {
			continue
		}
		r := NvidiaClocks{BusID: p[0]}
		sm, smOK := parseMHz(p[1])
		mem, memOK := parseMHz(p[2])
		maxSM, _ := parseMHz(p[3])
		maxMem, _ := parseMHz(p[4])
		r.SMMHz, r.MemMHz, r.ClocksOK = float64(sm), float64(mem), smOK && memOK
		r.MaxSMMHz, r.MaxMemMHz = float64(maxSM), float64(maxMem)
		if len(p) >= 7 {
			v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(p[6]), "0x"), 16, 64)
			r.Reasons, r.ReasonsOK = v, err == nil
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// NvidiaSupported is one GPU's SUPPORTED_CLOCKS: graphics and memory clocks
// in MHz, in the order nvidia-smi lists them.
type NvidiaSupported struct {
	SM, Mem []int
}

// ParseNvidiaSupportedClocks reads nvidia-smi -q -d SUPPORTED_CLOCKS: a
// "GPU <bus id>" header per GPU, then "Memory : <MHz> MHz" lines, each
// followed by its "Graphics : <MHz> MHz" lines. A GPU whose table is N/A has
// no clocks. Keys are bus ids in lower case.
func ParseNvidiaSupportedClocks(out string) map[string]*NvidiaSupported {
	res := map[string]*NvidiaSupported{}
	var cur *NvidiaSupported
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(t, "GPU "); ok && strings.Contains(rest, ":") && !strings.ContainsAny(strings.TrimSpace(rest), " \t") {
			cur = &NvidiaSupported{}
			res[strings.ToLower(strings.TrimSpace(rest))] = cur
			continue
		}
		key, value, ok := strings.Cut(t, ":")
		if !ok || cur == nil {
			continue
		}
		mhz, ok := parseMHz(value)
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Memory":
			cur.Mem = append(cur.Mem, mhz)
		case "Graphics":
			cur.SM = append(cur.SM, mhz)
		}
	}
	return res
}

// NvidiaGPU is one NVIDIA GPU: the bus id nvidia-smi -i takes, what
// pkg/governor plans with, and the maximum clocks the clock query reports.
type NvidiaGPU struct {
	BusID               string
	Facts               governor.NVFacts
	MaxSMMHz, MaxMemMHz float64
}

// NvidiaFacts are the facts of a GPU of architecture arch with the
// supported graphics and memory clocks smMHz and memMHz: what ReadNvidia
// reads, and what an emulated GPU names its locks by.
func NvidiaFacts(arch string, smMHz, memMHz []int) governor.NVFacts {
	return governor.NVFacts{
		Arch:            arch,
		SupportedSMMHz:  slices.Clone(smMHz),
		SupportedMemMHz: slices.Clone(memMHz),
		// -lmc "does not work on GPUs based on NVIDIA Hopper".
		MemLockRuntime: arch != governor.NVArchHopper,
	}
}

// Errors of ReadNvidia, which wraps the tool's error in them.
var (
	ErrNvidiaClockQuery      = errors.New("the nvidia-smi clock query failed")
	ErrNvidiaSupportedClocks = errors.New("nvidia-smi -q -d SUPPORTED_CLOCKS failed")
)

// ReadNvidia reads every NVIDIA GPU of architecture arch (a pkg/governor
// NVArch* value, from NvidiaArch) through nvidia-smi: the clock query, then
// the supported clocks. A failed run, or a clock query that lists no GPU,
// is an error wrapping ErrNvidiaClockQuery or ErrNvidiaSupportedClocks. A
// GPU missing from the supported clocks has none.
func ReadNvidia(ctx context.Context, run Runner, arch string) ([]NvidiaGPU, error) {
	rows, err := QueryNvidiaClocks(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNvidiaClockQuery, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: no GPU listed", ErrNvidiaClockQuery)
	}
	sup, err := run(ctx, "nvidia-smi", NvidiaSupportedClocksArgs...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNvidiaSupportedClocks, err)
	}
	supported := ParseNvidiaSupportedClocks(string(sup))
	var out []NvidiaGPU
	for _, r := range rows {
		s := supported[strings.ToLower(r.BusID)]
		if s == nil {
			s = &NvidiaSupported{}
		}
		out = append(out, NvidiaGPU{
			BusID:     r.BusID,
			Facts:     NvidiaFacts(arch, s.SM, s.Mem),
			MaxSMMHz:  r.MaxSMMHz,
			MaxMemMHz: r.MaxMemMHz,
		})
	}
	return out, nil
}
