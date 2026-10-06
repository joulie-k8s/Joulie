package hwemu

import (
	"strings"
	"testing"
)

// renderPCI is a listed PCI device for a test.
func renderPCI(class, vendor string) PCIDevice {
	return PCIDevice{
		Class:  Fact[string]{V: class, Src: "assumed:test device"},
		Vendor: Fact[string]{V: vendor, Src: "assumed:test device"},
	}
}

// H16: NFD PCI labels follow NFD's default rule, pci-<class4>_<vendor4> for
// the whitelisted class prefixes 03, 0b40 and 12. 0x030000 from vendor 0x102b
// gives the corpus label pci-0300_102b, the MI300X accelerator class 0x120000
// gives pci-1200_1002, a network controller gives none, and nfd: absent
// leaves no feature.node.kubernetes.io/ label at all.
func TestH16NFDPCILabels(t *testing.T) {
	t.Parallel()
	const present = "true"
	intel := renderBuiltin(t, "intel-xeon-2s-rapl")
	labels := intel.NodeObject("n").Labels
	if labels["feature.node.kubernetes.io/pci-0300_102b.present"] != present {
		t.Errorf("intel: no pci-0300_102b label, as the corpus capture has: %v", labels)
	}

	mi := renderBuiltin(t, "amd-instinct-mi300x-8gpu").NodeObject("n").Labels
	for _, want := range []string{"pci-1200_1002", "pci-0300_1a03"} {
		if mi["feature.node.kubernetes.io/"+want+".present"] != present {
			t.Errorf("mi300x: no %s label", want)
		}
	}
	nvl := renderBuiltin(t, "nvidia-h100-nvl-8gpu").NodeObject("n").Labels
	if nvl["feature.node.kubernetes.io/pci-0302_10de.present"] != present {
		t.Errorf("nvl: no pci-0302_10de label from gpus.pci")
	}

	intel.PCI = append(intel.PCI, renderPCI("0x020000", "0x8086"), renderPCI("0x0B4000", "0x15B3"), renderPCI("0x0b4100", "0x8086"))
	labels = intel.NodeObject("n").Labels
	for k := range labels {
		if strings.Contains(k, "pci-0200_") || strings.Contains(k, "pci-0b41_") {
			t.Errorf("classes 0x020000 and 0x0b4100 are not whitelisted, got %s", k)
		}
	}
	if labels["feature.node.kubernetes.io/pci-0b40_15b3.present"] != present {
		t.Errorf("coprocessor class 0x0B4000: no pci-0b40_15b3 label (class and vendor lowercased)")
	}

	intel.Node.NFD = "absent"
	labels = intel.NodeObject("n").Labels
	for k := range labels {
		if strings.HasPrefix(k, "feature.node.kubernetes.io/") {
			t.Errorf("nfd absent, but %s is set", k)
		}
	}
	if labels["kubernetes.io/arch"] != "amd64" || labels["kubernetes.io/hostname"] != "n" {
		t.Errorf("nfd absent must keep the non-NFD labels, got %v", labels)
	}
}
