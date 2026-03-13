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

type CPUModelSpec struct {
	Aliases          []string             `yaml:"aliases,omitempty"`
	Provenance       string               `yaml:"provenance"`
	Official         CPUOfficialSpec      `yaml:"official"`
	MeasuredCurves   *CPUMeasuredCurveSet `yaml:"measuredCurves,omitempty"`
	ProxyFrom        *CPUProxySpec        `yaml:"proxyFrom,omitempty"`
	ComputeDensity   float64              `yaml:"computeDensity,omitempty"`
	PerformanceHints map[string]float64   `yaml:"performanceHints,omitempty"`
}

type CPUOfficialSpec struct {
	Vendor       string    `yaml:"vendor"`
	BaseGHz      float64   `yaml:"baseGHz"`
	BoostGHz     float64   `yaml:"boostGHz"`
	TDPW         float64   `yaml:"tdpW"`
	CTdpRangeW   []float64 `yaml:"cTdpRangeW,omitempty"`
	DriverFamily string    `yaml:"driverFamily,omitempty"`
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

type GPUModelSpec struct {
	Aliases          []string           `yaml:"aliases,omitempty"`
	Provenance       string             `yaml:"provenance"`
	Official         GPUOfficialSpec    `yaml:"official"`
	ComputeDensity   float64            `yaml:"computeDensity,omitempty"`
	PerformanceHints map[string]float64 `yaml:"performanceHints,omitempty"`
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
	s = whitespaceRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var whitespaceRE = regexp.MustCompile(`\s+`)

func ParseIntString(raw string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n
}
