# Joulie

Kubernetes energy management: a controller manager (policy + digital twin), a node agent (RAPL/NVML caps), a scheduler extender, a kubectl plugin, a simulator. Read `.claude/skills/joulie-dev/SKILL.md` before changing code; it holds the architecture map, the ownership contract, the pitfalls and the per-change checklists.

## Invariants

- Toolchain: Go from `go.mod` (`toolchain` line). On the main dev box use `$HOME/.local/go/bin/go`; `/usr/bin/go` is a different version and fails with `compile: version ... does not match`.
- `api/v1alpha1` is the source of truth for the CRDs. Never edit `config/crd/bases/*.yaml` or `charts/joulie/crds/*.yaml` by hand; run `make generate manifests` after touching `api/` and commit the output. CI runs `make verify-manifests`.
- Before claiming anything works: `go build ./... && go vet ./... && go test ./...`, then `helm lint charts/joulie` and `helm template joulie charts/joulie -f values/joulie.yaml` if the chart changed. `go test -race ./cmd/...` when touching caches or goroutines.
- Numbers read from the API server arrive as `int64` when whole. Read CRs through the typed `api/v1alpha1` structs, never through `unstructured` maps with a `float64` assertion.
- `NodeTwin` has two writers with per-field ownership (see the skill). Every write carries a field manager from `pkg/api`.
- Node object names come from `pkg/api.ObjectNameForNode`; never derive them elsewhere.
- Every confirmed bug ships with a regression test that fails on the old code.
- Every user-visible change (env var, metric, Helm value, CRD field, image name) updates `website/content/en/docs/getting-started/05-configuration-reference.md` and, for chart values, `charts/joulie/values.yaml`.
- No em dashes or en dashes in prose, docs, commit messages or code comments.
