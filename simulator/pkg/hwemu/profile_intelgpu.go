package hwemu

import "fmt"

// renderValidateIntelGPU checks gpus.intel: an Intel GPU needs it, with a
// driver of i915 or xe, a power unit shift that fits PKG_PWR_UNIT and a
// time window above 0, and no other vendor may set it. The amdgpu power
// sensor and PPT1 do not exist on an Intel GPU; no GPU tool reports on one,
// which the tools check of renderValidateGPUs enforces.
func renderValidateIntelGPU(p *Profile) []error {
	g := p.GPUs
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: gpus."+format, append([]any{p.Name}, args...)...))
	}
	if g.Vendor != "intel" {
		if g.Intel != nil {
			fail("intel applies to Intel GPUs only")
		}
		return errs
	}
	in := g.Intel
	if in == nil {
		fail("intel is required for Intel GPUs: the driver, its power unit and the PL1 time window")
		return errs
	}
	if !renderIntelDrivers[in.Driver.V] {
		fail("intel.driver %q is not %s or %s", in.Driver.V, IntelDriverI915, IntelDriverXe)
	}
	if in.PowerUnitShift.V < 0 || in.PowerUnitShift.V > renderIntelMaxPowerUnitShift {
		fail("intel.powerUnitShift %d is not in [0, %d]", in.PowerUnitShift.V, renderIntelMaxPowerUnitShift)
	}
	if in.MaxIntervalMS.V <= 0 {
		fail("intel.maxIntervalMS must be above 0")
	}
	if g.PowerSensor.V != "" || g.PPT1.V != "" {
		fail("powerSensor and ppt1 apply to amdgpu only")
	}
	return errs
}
