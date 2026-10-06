package hwemu

import (
	"strconv"
	"strings"
)

// Node labels that no profile lists by hand are generated here by the rules
// of the components that publish them on a real cluster, so a fake node
// carries what NFD and GPU feature discovery would put on it.

// renderNFDPrefix is the namespace of every Node Feature Discovery label.
const renderNFDPrefix = "feature.node.kubernetes.io/"

// renderNFDClassWhitelist is NFD's default deviceClassWhitelist: PCI class
// prefixes 03 (display), 0b40 (coprocessor) and 12 (processing accelerator).
// The default deviceLabelFields are class and vendor (NFD worker
// configuration reference).
var renderNFDClassWhitelist = []string{"03", "0b40", "12"}

// renderNFDPCILabels returns feature.node.kubernetes.io/pci-<class4>_<vendor4>.present
// for every listed device, the GPUs included, whose class is whitelisted.
// class4 is the first four hex digits of the class, so 0x030000 with vendor
// 0x102b gives pci-0300_102b, as in the corpus.
func renderNFDPCILabels(p *Profile) map[string]string {
	devices := append([]PCIDevice(nil), p.PCI...)
	if p.GPUs != nil {
		devices = append(devices, p.GPUs.PCI)
	}
	out := map[string]string{}
	for _, d := range devices {
		class := strings.ToLower(strings.TrimPrefix(d.Class.V, "0x"))
		vendor := strings.ToLower(strings.TrimPrefix(d.Vendor.V, "0x"))
		if len(class) < 4 || vendor == "" {
			continue
		}
		class4 := class[:4]
		for _, prefix := range renderNFDClassWhitelist {
			if strings.HasPrefix(class4, prefix) {
				out[renderNFDPrefix+"pci-"+class4+"_"+vendor+".present"] = "true"
				break
			}
		}
	}
	return out
}

// renderGFDProduct is nvidia.com/gpu.product for a product name: spaces
// become dashes (assumed; it matches the catalog alias NVIDIA-H100-NVL).
func renderGFDProduct(product string) string {
	return strings.Join(strings.Fields(product), "-")
}

// renderGFDLabels returns the labels GPU feature discovery and the NVIDIA
// device plugin publish for an exposure mode (NVIDIA/k8s-device-plugin at
// d7265fdcf89f: docs/gpu-feature-discovery/README.md lines 216-242, README.md
// lines 250-263, 402, 954-975):
//
//   - whole: gpu.product, gpu.count = N, gpu.replicas = 1;
//   - mig-single: product suffixed -MIG-<profile>, count N*m,
//     mig.strategy=single;
//   - mig-mixed: mig.strategy=mixed and mig-<profile>.count = N*m, with the
//     full-GPU product and count kept (assumed);
//   - timeslice-rename: gpu.replicas = r and
//     gpu.sharing-strategy=time-slicing;
//   - timeslice-norename: as rename, with the product suffixed -SHARED;
//   - dra: product and count only (assumed: the labeller runs beside a DRA
//     driver).
//
// gpu.replicas is 1 in the mig modes (assumed: no sharing is configured).
func renderGFDLabels(g *GPUSpec) map[string]string {
	n := g.Count.V
	product := renderGFDProduct(g.Product.V)
	out := map[string]string{
		"nvidia.com/gpu.product":  product,
		"nvidia.com/gpu.count":    strconv.Itoa(n),
		"nvidia.com/gpu.replicas": "1",
	}
	switch g.Exposure.V {
	case "mig-single":
		out["nvidia.com/gpu.product"] = product + "-MIG-" + g.MIGProfile
		out["nvidia.com/gpu.count"] = strconv.Itoa(n * g.MIGDevicesPerGPU)
		out["nvidia.com/mig.strategy"] = "single"
	case "mig-mixed":
		out["nvidia.com/mig.strategy"] = "mixed"
		out["nvidia.com/mig-"+g.MIGProfile+".count"] = strconv.Itoa(n * g.MIGDevicesPerGPU)
	case "timeslice-rename":
		out["nvidia.com/gpu.replicas"] = strconv.Itoa(g.TimesliceReplicas)
		out["nvidia.com/gpu.sharing-strategy"] = "time-slicing"
	case "timeslice-norename":
		out["nvidia.com/gpu.product"] = product + "-SHARED"
		out["nvidia.com/gpu.replicas"] = strconv.Itoa(g.TimesliceReplicas)
		out["nvidia.com/gpu.sharing-strategy"] = "time-slicing"
	case "dra":
		delete(out, "nvidia.com/gpu.replicas")
	}
	return out
}

// renderNodeLabels is the label set of NodeObject: the profile's label
// groups, then the generated NFD and GFD labels, with every NFD label dropped
// when node.nfd is absent, and kubernetes.io/hostname last.
func renderNodeLabels(p *Profile, nodeName string) map[string]string {
	out := map[string]string{}
	for _, g := range renderSortedGroups(p.Node.Labels) {
		for k, v := range p.Node.Labels[g].Values {
			out[k] = v
		}
	}
	for k, v := range renderNFDPCILabels(p) {
		out[k] = v
	}
	if g := p.GPUs; g != nil && g.Labeller.V == "gfd" {
		for k, v := range renderGFDLabels(g) {
			out[k] = v
		}
	}
	if p.Node.NFD == "absent" {
		for k := range out {
			if strings.HasPrefix(k, renderNFDPrefix) {
				delete(out, k)
			}
		}
	}
	out["kubernetes.io/hostname"] = nodeName
	return out
}
