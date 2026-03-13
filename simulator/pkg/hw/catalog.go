package hw

import "github.com/matbun/joulie/pkg/hwinv"

type Catalog = hwinv.Catalog
type CPUModelSpec = hwinv.CPUModelSpec
type CPUOfficialSpec = hwinv.CPUOfficialSpec
type CPUMeasuredCurveSet = hwinv.CPUMeasuredCurveSet
type CurveSource = hwinv.CurveSource
type CPUProxySpec = hwinv.CPUProxySpec
type GPUModelSpec = hwinv.GPUModelSpec
type GPUOfficialSpec = hwinv.GPUOfficialSpec

func LoadCatalog(path string) (*Catalog, error) {
	return hwinv.LoadCatalog(path)
}

func ValidateCatalog(c *Catalog) error {
	return hwinv.ValidateCatalog(c)
}
