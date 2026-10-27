package hwinv

import (
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

//go:embed assets/hardware.yaml
var defaultCatalogYAML []byte

type Catalog struct {
	CatalogVersion string                  `yaml:"catalogVersion"`
	CPUModels      map[string]CPUModelSpec `yaml:"cpuModels"`
	GPUModels      map[string]GPUModelSpec `yaml:"gpuModels"`
}

// CPUModelSpec is one CPU model in the catalogue. A computeDensity or
// performanceHints key in an older catalogue file is ignored: the first had no
// defined meaning and ranked many-core CPU-only nodes above GPU nodes, the second was
// never read.
type CPUModelSpec struct {
	Aliases        []string             `yaml:"aliases,omitempty"`
	Provenance     string               `yaml:"provenance"`
	Official       CPUOfficialSpec      `yaml:"official"`
	MeasuredCurves *CPUMeasuredCurveSet `yaml:"measuredCurves,omitempty"`
	ProxyFrom      *CPUProxySpec        `yaml:"proxyFrom,omitempty"`
}

type CPUOfficialSpec struct {
	Vendor       string    `yaml:"vendor"`
	BaseGHz      float64   `yaml:"baseGHz"`
	BoostGHz     float64   `yaml:"boostGHz"`
	TDPW         float64   `yaml:"tdpW"`
	CTdpRangeW   []float64 `yaml:"cTdpRangeW,omitempty"`
	DriverFamily string    `yaml:"driverFamily,omitempty"`
}

// SocketCapMinFractionOfTDP is the lowest package power cap of one socket, as
// a fraction of its TDP, that a percent cap resolves to (assumed: vendors
// publish no minimum; a package power limit may be set below the low end of
// the cTDP range, AMD EPYC 9004 HPC tuning guide, publication 58002).
const SocketCapMinFractionOfTDP = 0.55

// SocketCapRangeW is the package power cap range of one socket of this CPU:
// the TDP is the maximum and SocketCapMinFractionOfTDP x TDP the minimum.
// The cTDP range is not a cap floor. ok is false without a TDP. The simulator models a node's socket
// cap range with it, and the agent resolves a percent CPU cap against it on
// a node it controls over HTTP that has no RAPL files, so the two agree.
func (o CPUOfficialSpec) SocketCapRangeW() (minW, maxW float64, ok bool) {
	if o.TDPW <= 0 {
		return 0, 0, false
	}
	maxW = o.TDPW
	return SocketCapMinFractionOfTDP * maxW, maxW, true
}

// CapForPctW is pct of maxW clamped to minW and maxW: how the agent turns a
// percent CPU cap into watts against a cap range, whether the range comes
// from RAPL files or from the catalog.
func CapForPctW(pct, minW, maxW float64) float64 {
	target := pct / 100 * maxW
	if target < minW {
		target = minW
	}
	if target > maxW {
		target = maxW
	}
	return target
}

// SimulatedNodeCPU is the CPU the simulator models a node with, read from
// the node's labels as the simulator reads them: the model from
// joulie.io/hw.cpu-model, else from NFD's cpu-model.name, and the socket
// count from joulie.io/hw.cpu-sockets alone, 0 without it (the simulator
// then models SIM_CPU_SOCKETS, which the agent cannot see).
func SimulatedNodeCPU(labels map[string]string) (rawModel string, sockets int) {
	for _, k := range []string{"joulie.io/hw.cpu-model", "feature.node.kubernetes.io/cpu-model.name"} {
		if v := strings.TrimSpace(labels[k]); v != "" {
			rawModel = v
			break
		}
	}
	return rawModel, ParseIntString(labels["joulie.io/hw.cpu-sockets"])
}

// NodeCPUCapRange is the CPU package cap range of a whole node from the
// catalog: Sockets x the socket range of the catalog CPU Key.
type NodeCPUCapRange struct {
	Key                    string
	Sockets                int
	SocketMinW, SocketMaxW float64
}

// MinW is the node's lowest CPU cap, MaxW its highest.
func (r NodeCPUCapRange) MinW() float64 { return r.SocketMinW * float64(r.Sockets) }
func (r NodeCPUCapRange) MaxW() float64 { return r.SocketMaxW * float64(r.Sockets) }

// CapForPctW is the node's CPU cap for a percent cap (CapForPctW).
func (r NodeCPUCapRange) CapForPctW(pct float64) float64 {
	return CapForPctW(pct, r.MinW(), r.MaxW())
}

// NodeCPUCapRangeW is the CPU package cap range of a node of sockets sockets
// of the CPU rawModel names: SocketCapRangeW of its catalog entry, times
// sockets. It is what the agent resolves a percent cap against on a node it
// controls over HTTP without RAPL files, as one total the simulator splits
// over the sockets, and what standalone mode applies for that agent. The
// error says why there is no range: a CPU the catalog does not know, an entry
// without a TDP, or an unknown socket count.
func (c *Catalog) NodeCPUCapRangeW(rawModel string, sockets int) (NodeCPUCapRange, error) {
	key, spec, ok := c.MatchCPU(rawModel)
	if !ok {
		return NodeCPUCapRange{}, fmt.Errorf("cpu model %q not in the hardware catalog", rawModel)
	}
	minW, maxW, ok := spec.Official.SocketCapRangeW()
	if !ok {
		return NodeCPUCapRange{}, fmt.Errorf("hardware catalog %s has no TDP", key)
	}
	if sockets <= 0 {
		return NodeCPUCapRange{}, fmt.Errorf("socket count of %s unknown", key)
	}
	return NodeCPUCapRange{Key: key, Sockets: sockets, SocketMinW: minW, SocketMaxW: maxW}, nil
}

type CPUMeasuredCurveSet struct {
	Node2S *CurveSource `yaml:"node2S,omitempty"`
}

type CurveSource struct {
	Source string       `yaml:"source"`
	Points []PowerPoint `yaml:"points"`
}

type PowerPoint struct {
	LoadPct float64 `yaml:"loadPct"`
	PowerW  float64 `yaml:"powerW"`
}

type CPUProxySpec struct {
	Family string `yaml:"family"`
	Method string `yaml:"method"`
}

// GPUModelSpec is one GPU model in the catalogue; see CPUModelSpec for the
// keys older files may still carry.
type GPUModelSpec struct {
	Aliases    []string        `yaml:"aliases,omitempty"`
	Provenance string          `yaml:"provenance"`
	Official   GPUOfficialSpec `yaml:"official"`
}

type GPUOfficialSpec struct {
	Vendor         string  `yaml:"vendor"`
	MaxBoardPowerW float64 `yaml:"maxBoardPowerW"`
	MinBoardPowerW float64 `yaml:"minBoardPowerW,omitempty"`
}

type NodeDescriptor struct {
	CPUModelRaw string
	CPUVendor   string
	CPUSockets  int
	CPUCores    int
	GPUModelRaw string
	GPUVendor   string
	GPUCount    int
}

type MatchResult struct {
	CPUKey   string
	CPUSpec  *CPUModelSpec
	GPUKey   string
	GPUSpec  *GPUModelSpec
	Warnings []string
}

func LoadCatalog(path string) (*Catalog, error) {
	if strings.TrimSpace(path) == "" {
		return LoadDefaultCatalog()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return LoadDefaultCatalog()
	}
	return loadCatalogBytes(b)
}

func LoadDefaultCatalog() (*Catalog, error) {
	return loadCatalogBytes(defaultCatalogYAML)
}

func loadCatalogBytes(b []byte) (*Catalog, error) {
	var c Catalog
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if err := ValidateCatalog(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

func ValidateCatalog(c *Catalog) error {
	if c == nil {
		return nil
	}
	if strings.TrimSpace(c.CatalogVersion) == "" {
		return fmt.Errorf("catalogVersion is required")
	}
	for key, cpu := range c.CPUModels {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("cpu model key must be non-empty")
		}
		if cpu.Official.TDPW <= 0 {
			return fmt.Errorf("cpu model %q: official.tdpW must be > 0", key)
		}
		if cpu.Official.BaseGHz <= 0 || cpu.Official.BoostGHz <= 0 {
			return fmt.Errorf("cpu model %q: baseGHz/boostGHz must be > 0", key)
		}
		if cpu.MeasuredCurves != nil && cpu.MeasuredCurves.Node2S != nil && len(cpu.MeasuredCurves.Node2S.Points) == 0 {
			return fmt.Errorf("cpu model %q: measured node2S points required", key)
		}
	}
	for key, gpu := range c.GPUModels {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("gpu model key must be non-empty")
		}
		if gpu.Official.MaxBoardPowerW <= 0 {
			return fmt.Errorf("gpu model %q: official.maxBoardPowerW must be > 0", key)
		}
	}
	return nil
}

func (c *Catalog) MatchNode(desc NodeDescriptor) MatchResult {
	result := MatchResult{}
	if c == nil {
		if desc.CPUModelRaw != "" {
			result.Warnings = append(result.Warnings, "catalog unavailable for cpu matching")
		}
		if desc.GPUModelRaw != "" {
			result.Warnings = append(result.Warnings, "catalog unavailable for gpu matching")
		}
		return result
	}
	if key, spec, ok := c.MatchCPU(desc.CPUModelRaw); ok {
		result.CPUKey = key
		result.CPUSpec = spec
	} else if strings.TrimSpace(desc.CPUModelRaw) != "" {
		result.Warnings = append(result.Warnings, "cpu model not recognized: "+desc.CPUModelRaw)
	}
	if key, spec, ok := c.MatchGPU(desc.GPUModelRaw); ok {
		result.GPUKey = key
		result.GPUSpec = spec
	} else if strings.TrimSpace(desc.GPUModelRaw) != "" {
		result.Warnings = append(result.Warnings, "gpu model not recognized: "+desc.GPUModelRaw)
	}
	return result
}

func (c *Catalog) MatchCPU(raw string) (string, *CPUModelSpec, bool) {
	if c == nil {
		return "", nil, false
	}
	needle := normalizeName(raw)
	if needle == "" {
		return "", nil, false
	}
	for key, spec := range c.CPUModels {
		if normalizeName(key) == needle {
			specCopy := spec
			return key, &specCopy, true
		}
		for _, alias := range spec.Aliases {
			if normalizeName(alias) == needle {
				specCopy := spec
				return key, &specCopy, true
			}
		}
	}
	return "", nil, false
}

func (c *Catalog) MatchGPU(raw string) (string, *GPUModelSpec, bool) {
	if c == nil {
		return "", nil, false
	}
	needle := normalizeName(raw)
	if needle == "" {
		return "", nil, false
	}
	for key, spec := range c.GPUModels {
		if normalizeName(key) == needle {
			specCopy := spec
			return key, &specCopy, true
		}
		for _, alias := range spec.Aliases {
			if normalizeName(alias) == needle {
				specCopy := spec
				return key, &specCopy, true
			}
		}
	}
	return "", nil, false
}

func normalizeName(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	// /proc/cpuinfo reports Intel parts as "Intel(R) Xeon(R) Gold 6530 CPU @
	// 2.10GHz". The clock speed is a property of the part, not a way to tell
	// two parts apart, so drop it before matching; catalog aliases carry the
	// model alone. AMD reports no such suffix.
	s = cpuClockSuffixRE.ReplaceAllString(s, "")
	s = strings.NewReplacer(
		"(r)", "",
		"®", "",
		"™", "",
		"(", " ",
		")", " ",
		"-", " ",
		"_", " ",
		"/", " ",
		",", " ",
	).Replace(s)
	// A label-safe spelling turns "(R)" and "(TM)" into "-R-" and "-TM-", which
	// leave lone "r" and "tm" tokens; drop them so that spelling matches too.
	tokens := strings.Fields(s)
	kept := tokens[:0]
	for _, tok := range tokens {
		if tok != "r" && tok != "tm" {
			kept = append(kept, tok)
		}
	}
	// It also turns " CPU @ 2.40GHz" into "-CPU---2.40GHz", which the
	// suffix pattern above, anchored on "@", misses: drop a trailing clock
	// token and the "cpu" before it.
	if n := len(kept); n > 0 && cpuClockTokenRE.MatchString(kept[n-1]) {
		kept = kept[:n-1]
		if n := len(kept); n > 0 && kept[n-1] == "cpu" {
			kept = kept[:n-1]
		}
	}
	return strings.Join(kept, " ")
}

var whitespaceRE = regexp.MustCompile(`\s+`)

// cpuClockSuffixRE matches a trailing " cpu @ 2.10ghz", with or without the
// "cpu" word.
var cpuClockSuffixRE = regexp.MustCompile(`(?:\s+cpu)?\s+@\s*[0-9.]+\s*[gm]hz\s*$`)

// cpuClockTokenRE matches a clock speed token such as "2.40ghz".
var cpuClockTokenRE = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?[gm]hz$`)

func ParseIntString(raw string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n
}
