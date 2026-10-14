package contracts_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/matbun/joulie/pkg/hwinv"
	"sigs.k8s.io/yaml"
)

// --------------------------------------------------------------------------
// Experiments and the embedded hardware catalog
// --------------------------------------------------------------------------

// experimentInventory is the node inventory an experiment describes its fake
// cluster with: the input of scripts/generate_heterogeneous_assets.py and of
// the standalone simulator (SIM_STANDALONE_INVENTORY).
type experimentInventory struct {
	Nodes []struct {
		NodeName       string `json:"node_name"`
		NodeNamePrefix string `json:"node_name_prefix"`
		Vendor         string `json:"vendor"`
		Product        string `json:"product"`
		CPU            string `json:"cpu"`
		GPUCount       int    `json:"gpu_count"`
	} `json:"nodes"`
}

// The simulator matches the joulie.io/hw.cpu-model and joulie.io/hw.gpu-model
// node labels against the catalog, not the raw names. Two producers write
// them, with slightly different spellings:
//   - generatorLabel mirrors label_safe in
//     scripts/generate_heterogeneous_assets.py (fake nodes in a cluster);
//   - standaloneLabel mirrors labelSafe in
//     simulator/cmd/simulator/standalone.go (standalone mode).
var generatorLabelRE = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func generatorLabel(v string) string {
	s := strings.Trim(generatorLabelRE.ReplaceAllString(strings.TrimSpace(v), "-"), "-.")
	if s == "" {
		return "unknown"
	}
	return s
}

func standaloneLabel(v string) string {
	var b strings.Builder
	for _, r := range v {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}

// simulatorSpellings is every spelling of an inventory model the simulator
// matches against the catalog: the raw name, which standalone mode also keys
// performance families by, and both label forms.
func simulatorSpellings(raw string) []string {
	return []string{raw, generatorLabel(raw), standaloneLabel(raw)}
}

// The experiments used to generate a hardware catalog from their inventories
// and mount it over the one embedded in the simulator. Every generated entry
// was a placeholder (each CPU 2.0 GHz base, 3.0 GHz boost, 300 W TDP; each GPU
// a board maximum copied from the inventory, with no minimum), and it replaced
// the embedded catalog whole, so every experiment node was modelled from those
// placeholders instead of the published facts in pkg/hwinv. The experiments now
// read the embedded catalog, which is only right while it knows every CPU and
// GPU their inventories name, in every spelling the simulator matches.
func TestExperimentInventoriesMatchTheEmbeddedCatalog(t *testing.T) {
	root := repoRoot(t)
	catalog, err := hwinv.LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("load the embedded catalog: %v", err)
	}

	t.Run("every inventory model has an entry", func(t *testing.T) {
		inventories := experimentInventoryPaths(t, root)
		for _, rel := range inventories {
			body, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			var inv experimentInventory
			if err := yaml.Unmarshal(body, &inv); err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			if len(inv.Nodes) == 0 {
				t.Errorf("%s lists no nodes", rel)
			}
			for _, n := range inv.Nodes {
				name := n.NodeNamePrefix
				if name == "" {
					name = n.NodeName
				}
				if strings.TrimSpace(n.CPU) == "" {
					t.Errorf("%s: %s names no cpu", rel, name)
				} else {
					for _, cpu := range simulatorSpellings(n.CPU) {
						if _, _, ok := catalog.MatchCPU(cpu); !ok {
							t.Errorf("%s: %s: cpu %q has no entry in pkg/hwinv/assets/hardware.yaml", rel, name, cpu)
						}
					}
				}
				vendor := strings.ToLower(strings.TrimSpace(n.Vendor))
				if n.GPUCount == 0 || (vendor != "nvidia" && vendor != "amd") {
					continue
				}
				for _, gpu := range simulatorSpellings(n.Product) {
					_, spec, ok := catalog.MatchGPU(gpu)
					if !ok {
						t.Errorf("%s: %s: gpu %q has no entry in pkg/hwinv/assets/hardware.yaml", rel, name, gpu)
						continue
					}
					if got := strings.ToLower(spec.Official.Vendor); got != vendor {
						t.Errorf("%s: %s: gpu %q matches a %s entry, the inventory says %s", rel, name, gpu, got, vendor)
					}
				}
			}
		}
	})

	t.Run("no generated catalog ships", func(t *testing.T) {
		const generated = "simulator/catalog/hardware.generated.yaml"
		if _, err := os.Stat(filepath.Join(root, generated)); err == nil {
			t.Errorf("%s exists; the experiments read the catalog embedded in pkg/hwinv", generated)
		}
	})

	// The chart's default SIM_HARDWARE_CATALOG_PATH is the file the catalog
	// ConfigMap mounts. An empty file there is not "no catalog": LoadCatalog
	// reads it, fails validation and the simulator runs with no catalog at
	// all. An installer that creates the empty ConfigMap therefore has to
	// clear the path too.
	t.Run("installers clear the catalog path", func(t *testing.T) {
		clearPath := regexp.MustCompile(`--set "env\.SIM_HARDWARE_CATALOG_PATH="`)
		emptyConfigMap := regexp.MustCompile(`--from-literal=hardware\.generated\.yaml=""`)
		installers, err := filepath.Glob(filepath.Join(root, "experiments", "*", "scripts", "03_install_components.sh"))
		if err != nil {
			t.Fatal(err)
		}
		if len(installers) < 3 {
			t.Fatalf("found %d experiment installers, want at least 3", len(installers))
		}
		for _, path := range installers {
			rel, _ := filepath.Rel(root, path)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !clearPath.Match(body) {
				t.Errorf("%s does not pass --set \"env.SIM_HARDWARE_CATALOG_PATH=\"", rel)
			}
			if !emptyConfigMap.Match(body) {
				t.Errorf("%s does not create the catalog ConfigMap empty", rel)
			}
			if strings.Contains(string(body), "GENERATED_CATALOG") {
				t.Errorf("%s still reads GENERATED_CATALOG", rel)
			}
		}
	})
}

// experimentInventoryPaths returns every inventory an experiment names: each
// configs/cluster-nodes*.yaml and each inventory.source of a config.
func experimentInventoryPaths(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	nodes, err := filepath.Glob(filepath.Join(root, "experiments", "*", "configs", "cluster-nodes*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range nodes {
		rel, _ := filepath.Rel(root, p)
		seen[filepath.ToSlash(rel)] = true
	}
	configs, err := filepath.Glob(filepath.Join(root, "experiments", "*", "configs", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range configs {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Inventory struct {
				Source string `json:"source"`
			} `json:"inventory"`
		}
		// A kind cluster config or a node list is not a benchmark config;
		// it simply has no inventory.source.
		if err := yaml.Unmarshal(body, &cfg); err != nil {
			continue
		}
		if src := strings.TrimSpace(cfg.Inventory.Source); src != "" {
			if _, err := os.Stat(filepath.Join(root, src)); err != nil {
				rel, _ := filepath.Rel(root, p)
				t.Errorf("%s: inventory.source %s: %v", rel, src, err)
				continue
			}
			seen[filepath.ToSlash(src)] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) < 3 {
		t.Fatalf("found %d experiment inventories, want at least one per experiment", len(out))
	}
	return out
}
