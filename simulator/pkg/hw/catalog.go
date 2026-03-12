package hw

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

type Catalog struct {
	CatalogVersion string                   `yaml:"catalogVersion"`
	CPUModels      map[string]CPUModelSpec  `yaml:"cpuModels"`
	GPUModels      map[string]GPUModelSpec  `yaml:"gpuModels"`
	NodeClasses    map[string]NodeClassSpec `yaml:"nodeClasses"`
}

type CPUModelSpec struct {
	Provenance string               `yaml:"provenance"`
	Official   CPUOfficialSpec      `yaml:"official"`
	Measured   *CPUMeasuredCurveSet `yaml:"measuredCurves,omitempty"`
	ProxyFrom  *CPUProxySpec        `yaml:"proxyFrom,omitempty"`
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

type CPUProxySpec struct {
	Family string `yaml:"family"`
	Method string `yaml:"method"`
}

type GPUModelSpec struct {
	Provenance string          `yaml:"provenance"`
	Official   GPUOfficialSpec `yaml:"official"`
}

type GPUOfficialSpec struct {
	Vendor         string  `yaml:"vendor"`
	MaxBoardPowerW float64 `yaml:"maxBoardPowerW"`
}

type NodeClassSpec struct {
	Count       int     `yaml:"count"`
	CPUModel    string  `yaml:"cpuModel"`
	CPUSockets  int     `yaml:"cpuSockets"`
	CoresPerCPU int     `yaml:"coresPerCpu"`
	MemoryGiB   float64 `yaml:"memoryGiB"`
	NetworkGbps float64 `yaml:"networkGbps"`
	GPUModel    string  `yaml:"gpuModel,omitempty"`
	GPUsPerNode int     `yaml:"gpusPerNode,omitempty"`
}

func LoadCatalog(path string) (*Catalog, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
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
		if cpu.Measured != nil && cpu.Measured.Node2S != nil {
			if len(cpu.Measured.Node2S.Points) == 0 {
				return fmt.Errorf("cpu model %q: measured node2S points required", key)
			}
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
	for key, cls := range c.NodeClasses {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("node class key must be non-empty")
		}
		if cls.Count <= 0 {
			return fmt.Errorf("node class %q: count must be > 0", key)
		}
		if cls.CPUSockets <= 0 || cls.CoresPerCPU <= 0 {
			return fmt.Errorf("node class %q: cpu sockets/cores must be > 0", key)
		}
		if strings.TrimSpace(cls.CPUModel) == "" {
			return fmt.Errorf("node class %q: cpuModel is required", key)
		}
		if cls.GPUsPerNode > 0 && strings.TrimSpace(cls.GPUModel) == "" {
			return fmt.Errorf("node class %q: gpuModel required when gpusPerNode > 0", key)
		}
	}
	return nil
}
