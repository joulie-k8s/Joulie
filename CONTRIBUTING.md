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
- `make build-push-all TAG=<tag>` (agent + operator + simulator)
- `make test-examples` (YAML dry-run validation)

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
