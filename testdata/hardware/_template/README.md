# Capture template

Copy this directory, replace every file in the copy with your machine's real
capture, and you have a corpus entry. Nothing here is real data: every value is
a placeholder chosen to show the shape of the file.

Directories whose name starts with an underscore are not machines. Both
`cmd/agent/fixture_corpus_test.go` and `cmd/agent/corpus_validate_test.go` skip
them, so this template is never replayed and never validated. That is asserted
by `TestCorpusListingsSkipUnderscoreDirectories`, not assumed.

## Use it

```sh
cp -r testdata/hardware/_template testdata/hardware/<machine>
rm testdata/hardware/<machine>/README.md
```

Then overwrite the copies with the output of the capture script, which is the
only thing that should ever produce the machine files:

```sh
sudo sh hack/collect-hardware-fixture.sh --name <machine> --out /tmp/fixtures
tar -xf /tmp/fixtures/<machine>.tar -C testdata/hardware/
```

The script overwrites `powercap/`, `cpuinfo`, `cpufreq-driver`,
`nvidia-smi.txt`, `rocm-smi.txt` and `node-labels.json`, and writes its own
`machine.yaml` skeleton. Keep whichever `machine.yaml` you prefer to start from,
this template's or the script's, and fill in the same keys either way. Delete
any file the script did not write, because it describes a machine that is not
yours: if your node has no GPU, `nvidia-smi.txt` has to go.

`testdata/hardware/README.md` is the full contributor guide. Read it before you
open a pull request.

## The files

| File | Required | What it is |
|---|---|---|
| `machine.yaml` | yes | The hand written half. Every key is documented in the placeholder. |
| `cpuinfo` | yes | Verbatim `/proc/cpuinfo`. |
| `node-labels.json` | yes | The node's labels, a flat JSON object of strings. `{}` is valid. |
| `expected.json` | yes | Generated golden. Never hand written, never hand edited. |
| `powercap/` | no | Copy of `/sys/class/powercap`, one directory per zone. Absent when the node has no RAPL. |
| `cpufreq-driver` | no | The active cpufreq scaling driver, one word. |
| `nvidia-smi.txt` | no | Stdout of the agent's NVIDIA query. Absent when the tool is not installed. |
| `rocm-smi.txt` | no | Stdout of the agent's ROCm query. Absent when the tool is not installed. |
| `README.md` | no | Notes that do not fit in `machine.yaml`. Delete this one when you copy. |

Any other file name is rejected. A tarball you forgot to delete, an editor
backup, a `.DS_Store`: the validation test names it and fails.

### `node-labels.json`

The node's labels as the API server reports them:

```sh
kubectl get node <node> -o jsonpath='{.metadata.labels}' | jq .
```

A flat object, string values only. This is the single easiest place for a site
name, a team name, a rack position or a tenant id to reach the repository, so
read it key by key. `kubernetes.io/hostname` must read `REDACTED-HOST`, which is
what the capture script writes in place of every spelling of the machine's name.

### `expected.json`

Generated, and `{}` here on purpose so that a copy which was never regenerated
fails loudly rather than looking plausible. Produce the real one after the
capture is complete and reviewed:

```sh
go test ./cmd/agent/ -run Corpus -update
git diff testdata/hardware
```

Read that diff. It is the agent's own conclusion about your machine, and it is
the thing a reviewer will question.

### `powercap/`

One directory per zone, named the way sysfs names it (`intel-rapl:0`,
`intel-rapl:0:0`). Every zone needs its `name` file, because the agent selects
package zones by that file and never by the directory pattern, and at least one
`constraint_0_*` file.

Delete the whole directory if the node has no RAPL. A machine with no powercap
tree is a useful fixture, not a broken one: `vm-no-rapl` is exactly that.

### `nvidia-smi.txt` and `rocm-smi.txt`

The stdout of the exact queries `cmd/agent/main.go` runs, no more and no less:

```sh
nvidia-smi --query-gpu=index,power.min_limit,power.max_limit,power.limit,power.draw,name --format=csv,noheader,nounits
rocm-smi --showpowercap --showproductname --json
```

One CSV line per GPU for NVIDIA, a JSON object with one `card<N>` key per GPU
for ROCm. `nvidia-smi -L` is not captured: the agent runs it only to see whether
the command succeeds, and its output is a list of GPU UUIDs.
