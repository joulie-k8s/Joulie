package hw

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Profile defines the node hardware-power model used by the simulator.
type Profile struct {
	BaseIdleW    float64
	PodW         float64
	DvfsDropW    float64
	RaplHeadW    float64
	DefaultCapW  float64
	PMaxW        float64
	AlphaUtil    float64
	BetaFreq     float64
	FMinMHz      float64
	FMaxMHz      float64
	RaplCapMinW  float64
	RaplCapMaxW  float64
	DvfsRampMS   int
	NoiseStddevW float64
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
	BaseIdleW    *float64 `yaml:"baseIdleW"`
	PodW         *float64 `yaml:"podW"`
	DvfsDropW    *float64 `yaml:"dvfsDropWPerPct"`
	RaplHeadW    *float64 `yaml:"raplHeadW"`
	DefaultCapW  *float64 `yaml:"defaultCapW"`
	PMaxW        *float64 `yaml:"pMaxW"`
	AlphaUtil    *float64 `yaml:"alphaUtil"`
	BetaFreq     *float64 `yaml:"betaFreq"`
	FMinMHz      *float64 `yaml:"fMinMHz"`
	FMaxMHz      *float64 `yaml:"fMaxMHz"`
	RaplCapMinW  *float64 `yaml:"raplCapMinW"`
	RaplCapMaxW  *float64 `yaml:"raplCapMaxW"`
	DvfsRampMS   *int     `yaml:"dvfsRampMs"`
	NoiseStddevW *float64 `yaml:"noiseStddevW"`
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
