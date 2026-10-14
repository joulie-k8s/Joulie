package layout

import "testing"

// The governor paths are where the agent reads and writes on a real host,
// so they are pinned to the kernel's spelling.
func TestGovernorPaths(t *testing.T) {
	link, target := PCIBusDeviceLink(0xc1)
	for _, tc := range []struct{ got, want string }{
		{CPUFreqDir(), "devices/system/cpu/cpufreq"},
		{GovernorTunablesDir("conservative"), "devices/system/cpu/cpufreq/conservative"},
		{IntelPstateDir(), "devices/system/cpu/intel_pstate"},
		{AMDPstateDir(), "devices/system/cpu/amd_pstate"},
		{IntelUncoreDieDir(1, 0), "devices/system/cpu/intel_uncore_frequency/package_01_die_00"},
		{IntelUncoreClusterDir(3), "devices/system/cpu/intel_uncore_frequency/uncore03"},
		{AMDGPUPMPolicyDir(0xc1), "devices/pci0000:c1/0000:c1:00.0/pm_policy"},
		{link, "bus/pci/devices/0000:c1:00.0"},
		{target, "../../../devices/pci0000:c1/0000:c1:00.0"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
