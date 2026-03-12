package hw

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Profile defines the node hardware-power model used by the simulator.
type Profile struct {
	BaseIdleW                 float64
	PodW                      float64
	DvfsDropW                 float64
	RaplHeadW                 float64
	DefaultCapW               float64
	PMaxW                     float64
	AlphaUtil                 float64
	BetaFreq                  float64
	FMinMHz                   float64
	FMaxMHz                   float64
	RaplCapMinW               float64
	RaplCapMaxW               float64
	DvfsRampMS                int
	NoiseStddevW              float64
	CPUModel                  string
	CPUProvenance             string
	CPUCurveSource            string
	CPUProxyFrom              string
	CPUPowerCurve             []PowerPoint
	CPUDriverFamily           string
	CPULowestNonlinearFreqMHz float64
	CPUSockets                int
	CPUSocketCapMinW          float64
	CPUSocketCapMaxW          float64
	GPU                       GPUProfile
}

type PowerPoint struct {
	LoadPct float64 `yaml:"loadPct"`
	PowerW  float64 `yaml:"powerW"`
}

type GPUProfile struct {
	Vendor            string
	Product           string
	Count             int
	IdleWattsPerGPU   float64
	MaxWattsPerGPU    float64
	MinCapWattsPerGPU float64
	CapApplyTauMS     int
	Provenance        string
	ComputeGamma      float64
	MemoryEpsilon     float64
	MemoryGamma       float64
	PowerModel        GPUPowerModel
}

type GPUPowerModel struct {
	AlphaUtil float64
	BetaCap   float64
}

type ClassFile struct {
	Classes []Class `yaml:"classes"`
}

type Class struct {
	Name        string            `yaml:"name"`
	MatchLabels map[string]string `yaml:"matchLabels"`
	Model       Overrides         `yaml:"model"`
}

type Overrides struct {
	BaseIdleW                 *float64      `yaml:"baseIdleW"`
	PodW                      *float64      `yaml:"podW"`
	DvfsDropW                 *float64      `yaml:"dvfsDropWPerPct"`
	RaplHeadW                 *float64      `yaml:"raplHeadW"`
	DefaultCapW               *float64      `yaml:"defaultCapW"`
	PMaxW                     *float64      `yaml:"pMaxW"`
	AlphaUtil                 *float64      `yaml:"alphaUtil"`
	BetaFreq                  *float64      `yaml:"betaFreq"`
	FMinMHz                   *float64      `yaml:"fMinMHz"`
	FMaxMHz                   *float64      `yaml:"fMaxMHz"`
	RaplCapMinW               *float64      `yaml:"raplCapMinW"`
	RaplCapMaxW               *float64      `yaml:"raplCapMaxW"`
	DvfsRampMS                *int          `yaml:"dvfsRampMs"`
	NoiseStddevW              *float64      `yaml:"noiseStddevW"`
	CPUModel                  *string       `yaml:"cpuModel"`
	CPUProvenance             *string       `yaml:"cpuProvenance"`
	CPUCurveSource            *string       `yaml:"cpuCurveSource"`
	CPUProxyFrom              *string       `yaml:"cpuProxyFrom"`
	CPUPowerCurve             []PowerPoint  `yaml:"cpuPowerCurve"`
	CPUDriverFamily           *string       `yaml:"cpuDriverFamily"`
	CPULowestNonlinearFreqMHz *float64      `yaml:"cpuLowestNonlinearFreqMHz"`
	CPUSockets                *int          `yaml:"cpuSockets"`
	CPUSocketCapMinW          *float64      `yaml:"cpuSocketCapMinW"`
	CPUSocketCapMaxW          *float64      `yaml:"cpuSocketCapMaxW"`
	GPU                       *GPUOverrides `yaml:"gpu"`
}

type GPUOverrides struct {
	Vendor            *string            `yaml:"vendor"`
	Product           *string            `yaml:"product"`
	Count             *int               `yaml:"count"`
	IdleWattsPerGPU   *float64           `yaml:"idleWattsPerGpu"`
	MaxWattsPerGPU    *float64           `yaml:"maxWattsPerGpu"`
	MinCapWattsPerGPU *float64           `yaml:"minCapWattsPerGpu"`
	CapApplyTauMS     *int               `yaml:"capApplyTauMs"`
	Provenance        *string            `yaml:"provenance"`
	ComputeGamma      *float64           `yaml:"computeGamma"`
	MemoryEpsilon     *float64           `yaml:"memoryEpsilon"`
	MemoryGamma       *float64           `yaml:"memoryGamma"`
	PowerModel        *GPUPowerOverrides `yaml:"powerModel"`
}

type GPUPowerOverrides struct {
	AlphaUtil *float64 `yaml:"alphaUtil"`
	BetaCap   *float64 `yaml:"betaCap"`
}

func ApplyOverrides(base Profile, o Overrides) Profile {
	out := base
	if o.BaseIdleW != nil {
		out.BaseIdleW = *o.BaseIdleW
	}
	if o.PodW != nil {
		out.PodW = *o.PodW
	}
	if o.DvfsDropW != nil {
		out.DvfsDropW = *o.DvfsDropW
	}
	if o.RaplHeadW != nil {
		out.RaplHeadW = *o.RaplHeadW
	}
	if o.DefaultCapW != nil {
		out.DefaultCapW = *o.DefaultCapW
	}
	if o.PMaxW != nil {
		out.PMaxW = *o.PMaxW
	}
	if o.AlphaUtil != nil {
		out.AlphaUtil = *o.AlphaUtil
	}
	if o.BetaFreq != nil {
		out.BetaFreq = *o.BetaFreq
	}
	if o.FMinMHz != nil {
		out.FMinMHz = *o.FMinMHz
	}
	if o.FMaxMHz != nil {
		out.FMaxMHz = *o.FMaxMHz
	}
	if o.RaplCapMinW != nil {
		out.RaplCapMinW = *o.RaplCapMinW
	}
	if o.RaplCapMaxW != nil {
		out.RaplCapMaxW = *o.RaplCapMaxW
	}
	if o.DvfsRampMS != nil {
		out.DvfsRampMS = *o.DvfsRampMS
	}
	if o.NoiseStddevW != nil {
		out.NoiseStddevW = *o.NoiseStddevW
	}
	if o.CPUModel != nil {
		out.CPUModel = strings.TrimSpace(*o.CPUModel)
	}
	if o.CPUProvenance != nil {
		out.CPUProvenance = strings.TrimSpace(*o.CPUProvenance)
	}
	if o.CPUCurveSource != nil {
		out.CPUCurveSource = strings.TrimSpace(*o.CPUCurveSource)
	}
	if o.CPUProxyFrom != nil {
		out.CPUProxyFrom = strings.TrimSpace(*o.CPUProxyFrom)
	}
	if len(o.CPUPowerCurve) > 0 {
		out.CPUPowerCurve = append([]PowerPoint(nil), o.CPUPowerCurve...)
	}
	if o.CPUDriverFamily != nil {
		out.CPUDriverFamily = strings.TrimSpace(*o.CPUDriverFamily)
	}
	if o.CPULowestNonlinearFreqMHz != nil {
		out.CPULowestNonlinearFreqMHz = *o.CPULowestNonlinearFreqMHz
	}
	if o.CPUSockets != nil {
		out.CPUSockets = *o.CPUSockets
	}
	if o.CPUSocketCapMinW != nil {
		out.CPUSocketCapMinW = *o.CPUSocketCapMinW
	}
	if o.CPUSocketCapMaxW != nil {
		out.CPUSocketCapMaxW = *o.CPUSocketCapMaxW
	}
	if o.GPU != nil {
		if o.GPU.Vendor != nil {
			out.GPU.Vendor = strings.TrimSpace(*o.GPU.Vendor)
		}
		if o.GPU.Product != nil {
			out.GPU.Product = strings.TrimSpace(*o.GPU.Product)
		}
		if o.GPU.Count != nil {
			out.GPU.Count = *o.GPU.Count
		}
		if o.GPU.IdleWattsPerGPU != nil {
			out.GPU.IdleWattsPerGPU = *o.GPU.IdleWattsPerGPU
		}
		if o.GPU.MaxWattsPerGPU != nil {
			out.GPU.MaxWattsPerGPU = *o.GPU.MaxWattsPerGPU
		}
		if o.GPU.MinCapWattsPerGPU != nil {
			out.GPU.MinCapWattsPerGPU = *o.GPU.MinCapWattsPerGPU
		}
		if o.GPU.CapApplyTauMS != nil {
			out.GPU.CapApplyTauMS = *o.GPU.CapApplyTauMS
		}
		if o.GPU.Provenance != nil {
			out.GPU.Provenance = strings.TrimSpace(*o.GPU.Provenance)
		}
		if o.GPU.ComputeGamma != nil {
			out.GPU.ComputeGamma = *o.GPU.ComputeGamma
		}
		if o.GPU.MemoryEpsilon != nil {
			out.GPU.MemoryEpsilon = *o.GPU.MemoryEpsilon
		}
		if o.GPU.MemoryGamma != nil {
			out.GPU.MemoryGamma = *o.GPU.MemoryGamma
		}
		if o.GPU.PowerModel != nil {
			if o.GPU.PowerModel.AlphaUtil != nil {
				out.GPU.PowerModel.AlphaUtil = *o.GPU.PowerModel.AlphaUtil
			}
			if o.GPU.PowerModel.BetaCap != nil {
				out.GPU.PowerModel.BetaCap = *o.GPU.PowerModel.BetaCap
			}
		}
	}
	return out
}

func ValidateProfile(p Profile) error {
	if p.BaseIdleW < 0 {
		return fmt.Errorf("baseIdleW must be >= 0")
	}
	if p.PMaxW <= 0 {
		return fmt.Errorf("pMaxW must be > 0")
	}
	if p.PMaxW < p.BaseIdleW {
		return fmt.Errorf("pMaxW must be >= baseIdleW")
	}
	if p.AlphaUtil <= 0 {
		return fmt.Errorf("alphaUtil must be > 0")
	}
	if p.BetaFreq <= 0 {
		return fmt.Errorf("betaFreq must be > 0")
	}
	if p.FMinMHz <= 0 || p.FMaxMHz <= 0 {
		return fmt.Errorf("fMinMHz/fMaxMHz must be > 0")
	}
	if p.FMaxMHz < p.FMinMHz {
		return fmt.Errorf("fMaxMHz must be >= fMinMHz")
	}
	if p.RaplCapMinW <= 0 {
		return fmt.Errorf("raplCapMinW must be > 0")
	}
	if p.RaplCapMaxW <= 0 {
		return fmt.Errorf("raplCapMaxW must be > 0")
	}
	if p.RaplCapMaxW < p.RaplCapMinW {
		return fmt.Errorf("raplCapMaxW must be >= raplCapMinW")
	}
	if p.DefaultCapW <= 0 {
		return fmt.Errorf("defaultCapW must be > 0")
	}
	if p.DvfsRampMS < 0 {
		return fmt.Errorf("dvfsRampMs must be >= 0")
	}
	if p.NoiseStddevW < 0 {
		return fmt.Errorf("noiseStddevW must be >= 0")
	}
	if p.CPUSockets < 0 {
		return fmt.Errorf("cpuSockets must be >= 0")
	}
	if p.CPUSocketCapMinW < 0 {
		return fmt.Errorf("cpuSocketCapMinW must be >= 0")
	}
	if p.CPUSocketCapMaxW < 0 {
		return fmt.Errorf("cpuSocketCapMaxW must be >= 0")
	}
	if p.CPUSocketCapMaxW > 0 && p.CPUSocketCapMinW > p.CPUSocketCapMaxW {
		return fmt.Errorf("cpuSocketCapMinW must be <= cpuSocketCapMaxW")
	}
	if p.CPULowestNonlinearFreqMHz < 0 {
		return fmt.Errorf("cpuLowestNonlinearFreqMHz must be >= 0")
	}
	if len(p.CPUPowerCurve) > 0 {
		lastLoad := -1.0
		lastPower := -1.0
		for _, pt := range p.CPUPowerCurve {
			if pt.LoadPct < 0 || pt.LoadPct > 100 {
				return fmt.Errorf("cpuPowerCurve loadPct must be in [0,100]")
			}
			if pt.PowerW < 0 {
				return fmt.Errorf("cpuPowerCurve powerW must be >= 0")
			}
			if pt.LoadPct <= lastLoad {
				return fmt.Errorf("cpuPowerCurve loadPct must be strictly increasing")
			}
			if lastPower >= 0 && pt.PowerW < lastPower {
				return fmt.Errorf("cpuPowerCurve powerW must be monotone non-decreasing")
			}
			lastLoad = pt.LoadPct
			lastPower = pt.PowerW
		}
	}
	if p.GPU.Count < 0 {
		return fmt.Errorf("gpu.count must be >= 0")
	}
	if p.GPU.Count > 0 {
		if p.GPU.MaxWattsPerGPU <= 0 {
			return fmt.Errorf("gpu.maxWattsPerGpu must be > 0 when gpu.count > 0")
		}
		if p.GPU.IdleWattsPerGPU < 0 {
			return fmt.Errorf("gpu.idleWattsPerGpu must be >= 0")
		}
		if p.GPU.MinCapWattsPerGPU <= 0 {
			return fmt.Errorf("gpu.minCapWattsPerGpu must be > 0 when gpu.count > 0")
		}
		if p.GPU.MinCapWattsPerGPU > p.GPU.MaxWattsPerGPU {
			return fmt.Errorf("gpu.minCapWattsPerGpu must be <= gpu.maxWattsPerGpu")
		}
		if p.GPU.PowerModel.AlphaUtil <= 0 {
			return fmt.Errorf("gpu.powerModel.alphaUtil must be > 0")
		}
		if p.GPU.PowerModel.BetaCap <= 0 {
			return fmt.Errorf("gpu.powerModel.betaCap must be > 0")
		}
		if p.GPU.CapApplyTauMS < 0 {
			return fmt.Errorf("gpu.capApplyTauMs must be >= 0")
		}
		if p.GPU.ComputeGamma <= 0 {
			return fmt.Errorf("gpu.computeGamma must be > 0")
		}
		if p.GPU.MemoryEpsilon < 0 || p.GPU.MemoryEpsilon > 1 {
			return fmt.Errorf("gpu.memoryEpsilon must be in [0,1]")
		}
		if p.GPU.MemoryGamma <= 0 {
			return fmt.Errorf("gpu.memoryGamma must be > 0")
		}
	}
	return nil
}

func ValidateOverrides(o Overrides) error {
	if o.AlphaUtil != nil && *o.AlphaUtil <= 0 {
		return fmt.Errorf("alphaUtil override must be > 0")
	}
	if o.BetaFreq != nil && *o.BetaFreq <= 0 {
		return fmt.Errorf("betaFreq override must be > 0")
	}
	if o.FMinMHz != nil && *o.FMinMHz <= 0 {
		return fmt.Errorf("fMinMHz override must be > 0")
	}
	if o.FMaxMHz != nil && *o.FMaxMHz <= 0 {
		return fmt.Errorf("fMaxMHz override must be > 0")
	}
	if o.RaplCapMinW != nil && *o.RaplCapMinW <= 0 {
		return fmt.Errorf("raplCapMinW override must be > 0")
	}
	if o.RaplCapMaxW != nil && *o.RaplCapMaxW <= 0 {
		return fmt.Errorf("raplCapMaxW override must be > 0")
	}
	if o.DvfsRampMS != nil && *o.DvfsRampMS < 0 {
		return fmt.Errorf("dvfsRampMs override must be >= 0")
	}
	if o.NoiseStddevW != nil && *o.NoiseStddevW < 0 {
		return fmt.Errorf("noiseStddevW override must be >= 0")
	}
	if o.CPULowestNonlinearFreqMHz != nil && *o.CPULowestNonlinearFreqMHz < 0 {
		return fmt.Errorf("cpuLowestNonlinearFreqMHz override must be >= 0")
	}
	if o.CPUSockets != nil && *o.CPUSockets < 0 {
		return fmt.Errorf("cpuSockets override must be >= 0")
	}
	if o.CPUSocketCapMinW != nil && *o.CPUSocketCapMinW < 0 {
		return fmt.Errorf("cpuSocketCapMinW override must be >= 0")
	}
	if o.CPUSocketCapMaxW != nil && *o.CPUSocketCapMaxW < 0 {
		return fmt.Errorf("cpuSocketCapMaxW override must be >= 0")
	}
	if o.CPUSocketCapMinW != nil && o.CPUSocketCapMaxW != nil && *o.CPUSocketCapMinW > *o.CPUSocketCapMaxW {
		return fmt.Errorf("cpuSocketCapMinW override must be <= cpuSocketCapMaxW override")
	}
	if len(o.CPUPowerCurve) > 0 {
		lastLoad := -1.0
		lastPower := -1.0
		for _, pt := range o.CPUPowerCurve {
			if pt.LoadPct < 0 || pt.LoadPct > 100 {
				return fmt.Errorf("cpuPowerCurve override loadPct must be in [0,100]")
			}
			if pt.PowerW < 0 {
				return fmt.Errorf("cpuPowerCurve override powerW must be >= 0")
			}
			if pt.LoadPct <= lastLoad {
				return fmt.Errorf("cpuPowerCurve override loadPct must be strictly increasing")
			}
			if lastPower >= 0 && pt.PowerW < lastPower {
				return fmt.Errorf("cpuPowerCurve override powerW must be monotone non-decreasing")
			}
			lastLoad = pt.LoadPct
			lastPower = pt.PowerW
		}
	}
	if o.GPU != nil {
		if o.GPU.Count != nil && *o.GPU.Count < 0 {
			return fmt.Errorf("gpu.count override must be >= 0")
		}
		if o.GPU.IdleWattsPerGPU != nil && *o.GPU.IdleWattsPerGPU < 0 {
			return fmt.Errorf("gpu.idleWattsPerGpu override must be >= 0")
		}
		if o.GPU.MaxWattsPerGPU != nil && *o.GPU.MaxWattsPerGPU <= 0 {
			return fmt.Errorf("gpu.maxWattsPerGpu override must be > 0")
		}
		if o.GPU.MinCapWattsPerGPU != nil && *o.GPU.MinCapWattsPerGPU <= 0 {
			return fmt.Errorf("gpu.minCapWattsPerGpu override must be > 0")
		}
		if o.GPU.PowerModel != nil {
			if o.GPU.PowerModel.AlphaUtil != nil && *o.GPU.PowerModel.AlphaUtil <= 0 {
				return fmt.Errorf("gpu.powerModel.alphaUtil override must be > 0")
			}
			if o.GPU.PowerModel.BetaCap != nil && *o.GPU.PowerModel.BetaCap <= 0 {
				return fmt.Errorf("gpu.powerModel.betaCap override must be > 0")
			}
		}
		if o.GPU.MinCapWattsPerGPU != nil && o.GPU.MaxWattsPerGPU != nil && *o.GPU.MinCapWattsPerGPU > *o.GPU.MaxWattsPerGPU {
			return fmt.Errorf("gpu.minCapWattsPerGpu override must be <= gpu.maxWattsPerGpu override")
		}
		if o.GPU.CapApplyTauMS != nil && *o.GPU.CapApplyTauMS < 0 {
			return fmt.Errorf("gpu.capApplyTauMs override must be >= 0")
		}
		if o.GPU.ComputeGamma != nil && *o.GPU.ComputeGamma <= 0 {
			return fmt.Errorf("gpu.computeGamma override must be > 0")
		}
		if o.GPU.MemoryEpsilon != nil && (*o.GPU.MemoryEpsilon < 0 || *o.GPU.MemoryEpsilon > 1) {
			return fmt.Errorf("gpu.memoryEpsilon override must be in [0,1]")
		}
		if o.GPU.MemoryGamma != nil && *o.GPU.MemoryGamma <= 0 {
			return fmt.Errorf("gpu.memoryGamma override must be > 0")
		}
	}
	if o.FMinMHz != nil && o.FMaxMHz != nil && *o.FMaxMHz < *o.FMinMHz {
		return fmt.Errorf("fMaxMHz override must be >= fMinMHz override")
	}
	if o.RaplCapMinW != nil && o.RaplCapMaxW != nil && *o.RaplCapMaxW < *o.RaplCapMinW {
		return fmt.Errorf("raplCapMaxW override must be >= raplCapMinW override")
	}
	return nil
}

func ValidateClasses(base Profile, classes []Class) error {
	if err := ValidateProfile(base); err != nil {
		return fmt.Errorf("base profile invalid: %w", err)
	}
	for i, c := range classes {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("class[%d] name is required", i)
		}
		if err := ValidateOverrides(c.Model); err != nil {
			return fmt.Errorf("class %q overrides invalid: %w", c.Name, err)
		}
		if err := ValidateProfile(ApplyOverrides(base, c.Model)); err != nil {
			return fmt.Errorf("class %q effective profile invalid: %w", c.Name, err)
		}
	}
	return nil
}

func LoadClasses(path string, base Profile) ([]Class, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg ClassFile
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	out := make([]Class, 0, len(cfg.Classes))
	for _, c := range cfg.Classes {
		if strings.TrimSpace(c.Name) == "" {
			continue
		}
		if c.MatchLabels == nil {
			c.MatchLabels = map[string]string{}
		}
		out = append(out, c)
	}
	if err := ValidateClasses(base, out); err != nil {
		return nil, err
	}
	return out, nil
}
