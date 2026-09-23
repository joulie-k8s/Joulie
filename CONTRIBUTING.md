# Contributing to Joulie

Thanks for contributing. Joulie is a Kubernetes-native project focused on
energy-aware node power orchestration, simulation, and experiment reproducibility.

## Where to start

- Open an issue for bugs, questions, or feature proposals:
  - <https://github.com/joulie-k8s/Joulie/issues>
- If you already have a fix, you can open a PR directly and explain context in
  the description.

## Pull request workflow

1. Fork the repository (or create a branch if you have write access).
2. Create a focused branch from `main`.
3. Implement your change with tests/docs updates as needed.
4. Open a pull request against `main`.

Please include in the PR:

- what changed,
- why it changed,
- how it was validated (commands, screenshots, logs, etc.),
- any follow-up work not included.

## Local development

From repository root:

```bash
make help
make test
```

Useful targets:

- `make install TAG=<tag>`
- `make build-push TAG=<tag>`
- `make rollout TAG=<tag>`
- `make build-push-all TAG=<tag>` (agent + controller manager + simulator)
- `make test-examples` (YAML dry-run validation)

## Contributing a hardware capture

The agent reads hardware only through files and command output, so a copy of
those inputs replays a machine in a unit test with no hardware and no cluster.
`cmd/agent/testdata/hardware/` is a corpus of captured machines, and every one of them is
replayed on every `go test ./...`.

The corpus needs machines that maintainers do not own. If you have bare metal, a
GPU node or an unusual virtual machine, a capture takes a few minutes and turns
your machine into a permanent regression test:

```bash
sudo sh scripts/collect-hardware-fixture.sh --name <machine> --out /tmp/fixtures
tar -xf /tmp/fixtures/<machine>.tar -C cmd/agent/testdata/hardware/
# review every file, complete machine.yaml, then
go test ./cmd/agent/ -run Corpus -update
go test ./cmd/agent/...
```

Read [`cmd/agent/testdata/hardware/README.md`](./cmd/agent/testdata/hardware/README.md) before you
start. It has the full steps, a copyable template in
`cmd/agent/testdata/hardware/_template/`, and the rules about what is never accepted.

Two of those rules matter before you capture anything:

- **Captures are published as part of this repository**, permanently and in
  every fork. Nothing identifying may be in one: no hostname, MAC address, IP
  address, UUID, serial number, asset tag, username or cloud instance id.
  `cmd/agent/corpus_validate_test.go` fails the build on any of those, naming
  the file and the line, but it is a backstop and not a substitute for reading
  the files yourself.
- **`expected.json` is generated, never hand edited.** If it looks wrong, the
  fixture or the agent is wrong; say so in the pull request.

Open the pull request with the fixture checklist by appending
`?template=hardware-fixture.md` to the URL:

```
https://github.com/joulie-k8s/Joulie/compare/main...<your-branch>?template=hardware-fixture.md
```

## Documentation changes

Docs live under `website/` (Hugo + Docsy).

```bash
cd website
hugo server --disableFastRender
```

See [`website/README.md`](./website/README.md) for full docs workflow details.

## Code and review expectations

- Keep changes scoped and readable.
- Preserve backward compatibility only when explicitly required.
- Update docs/examples when behavior changes.
- Add or update tests for non-trivial logic changes.
- Be responsive to review feedback; reviewers may request follow-up edits before merge.

## Commit messages

Use clear, descriptive commit messages. Referencing issue/PR IDs is encouraged.

Example:

```text
simulator: fix queue-aware hpCount clamp and add regression test
```

## Release notes

Releases are managed by maintainers through GitHub Releases and CI workflows.
If your PR affects users, include a concise release-note style summary in the PR
description.

## Community standards

By participating, you agree to the project Code of Conduct:

- [`CODE_OF_CONDUCT.md`](./CODE_OF_CONDUCT.md)
