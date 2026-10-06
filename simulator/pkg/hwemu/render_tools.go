package hwemu

import (
	"encoding/json"
	"path"

	"github.com/matbun/joulie/simulator/pkg/hwemu/layout"
)

// renderTools writes state/tools.json, the same envelope for every family
// (L03), and a bin/<tool> link to the fake tool for each entry, so a
// tool the profile does not list is not on the emulated PATH.
func renderTools(b *renderBuilder, p *Profile, o RenderOptions) error {
	m := layout.ToolsManifest{SchemaVersion: 1, Tools: map[string]layout.ToolEntry{}}
	if p.GPUs != nil {
		for _, t := range p.GPUs.Tools {
			m.Tools[t.Name] = layout.ToolEntry{Variant: t.Variant}
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := b.file(path.Join(layout.StateDir, layout.ToolsFile), string(data)+"\n", layout.OwnerRender, 0o444); err != nil {
		return err
	}
	if p.GPUs == nil {
		return nil
	}
	for _, t := range p.GPUs.Tools {
		if err := b.link(path.Join(layout.BinDir, t.Name), layout.FakeSMIInContainer); err != nil {
			return err
		}
	}
	return nil
}
