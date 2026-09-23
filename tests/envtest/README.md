# envtest suite

These tests run the generated CRDs against a real Kubernetes control plane:
`sigs.k8s.io/controller-runtime/pkg/envtest` starts an `etcd` and a
`kube-apiserver` binary, installs `config/crd/bases` into it and hands the
tests a client. No kubelet, no nodes, no containers.

## What it proves that nothing else does

Two rules in this repository were, until now, checked only by looking at what
the code intends to write. The API server is what actually decides, so these
tests let it decide.

**Schema acceptance.** `tests/contracts` compares the CRD enum against
`pkg/api.PowerSourceValues` as two lists of strings. That catches a rename,
but it cannot catch a schema that rejects the payload for some other reason,
and a drifted enum once made every status write fail on a cluster while the
unit tests stayed green.

| Test | What it proves |
|---|---|
| `TestNodeTwinStatusAcceptsEveryPowerSourceValue` | every value in `pkg/api.PowerSourceValues` is accepted as `status.powerMeasurement.source` |
| `TestNodeTwinStatusRejectsUnknownPowerSource` | the enum is enforced, so the acceptance above is evidence and not an unvalidated field |
| `TestNodeTwinSpecRejectsUnknownProfile` | the same for `spec.profile`, which the controller manager writes on every policy decision |
| `TestNodeHardwareAcceptsTheStatusTheAgentWrites` | the exact unstructured map from `upsertNodeHardwareStatus` (`cmd/agent/main.go`) is accepted and nothing in it is pruned |
| `TestWholeNumberWattsSurviveTheTypedRoundTrip` | whole-number watts read back unchanged through the typed client, and come back as `int64` through an unstructured read, which is why components read CRs through `api/v1alpha1` |

**Field ownership.** The controller manager owns `NodeTwin.spec` and all of
`status` except `status.controlStatus`; the agent owns `status.controlStatus`
and nothing else. Unit tests check the shape of the patch payloads; here the
two field managers from `pkg/api` do server-side apply against the API server
and it records who owns what.

| Test | What it proves |
|---|---|
| `TestBothManagersWriteTheirOwnSubtreeWithoutClobbering` | both managers apply twice and neither erases the other's fields; server-side apply prunes what a manager owned and stopped sending, so an over-broad payload deletes the other subtree here |
| `TestManagedFieldsAttributeEachFieldToItsOwner` | `managedFields` attributes each field to the right manager, and the agent owns exactly one status field |
| `TestTakingAFieldOwnedByTheOtherManagerConflicts` | reaching into the other manager's subtree is a conflict in both directions, and `ForceOwnership` is the only way past it |

## Running

```bash
make test-envtest
```

The target installs `setup-envtest` into `./bin` (pinned to `release-0.19`,
matching controller-runtime v0.19 in `go.mod`), downloads the control-plane
binaries for `ENVTEST_K8S_VERSION` (1.31.0) into `./bin/k8s/`, exports
`KUBEBUILDER_ASSETS` and runs the suite. The download happens once.

By hand, with assets you already have:

```bash
KUBEBUILDER_ASSETS="$(setup-envtest use 1.31.0 -p path)" \
  go test -tags envtest ./tests/envtest/...
```

Overridable variables: `ENVTEST_VERSION`, `ENVTEST_K8S_VERSION`,
`ENVTEST_BIN_DIR`, `SETUP_ENVTEST`.

## Why it is not part of `go test ./...`

The suite needs two binaries that are not in the repository and not in the Go
module cache. Downloading them is a network call, and starting a control plane
costs a few seconds per run, so a tagged package keeps `go test ./...` fast and
hermetic. Without the `envtest` build tag the package is not compiled at all,
so a developer without the assets never sees it.

If the tag is set but `KUBEBUILDER_ASSETS` is unset, every test skips with a
message naming both ways to get the assets. Once a control plane is available,
nothing skips: every failure is a real failure.
