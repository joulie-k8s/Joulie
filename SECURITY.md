# Security policy

## Reporting a vulnerability

Report a vulnerability privately through GitHub: [open a private security advisory](https://github.com/joulie-k8s/Joulie/security/advisories/new). Do not open a public issue for it.

Say which version or commit is affected, which component (controller manager, agent, scheduler extender, kubectl plugin, simulator or Helm chart), how to reproduce the problem and what an attacker gains.

A maintainer acknowledges the report within 14 days, agrees on a fix and a disclosure date with the reporter, and publishes a GitHub security advisory with the release that fixes it. The release notes in [CHANGELOG.md](CHANGELOG.md) list every vulnerability a release fixes.

## Supported versions

The latest minor release, 0.2.x today, receives security fixes.

## How the project checks itself

- osv-scanner and govulncheck scan every dependency on each pull request that changes a manifest and nightly; the nightly run opens a pull request with the fixes it can make.
- CI actions are pinned by commit and container base images by digest.
- `go vet` and the race detector run on every pull request.
