# Readiness check before merging an env var rename

Tests the "Before calling a change ready" checklist: whether the assistant
walks every consumer of a renamed name (docs, examples, CI, experiments),
knows where the compatibility alias lives, and pins an unreproducible
experiment to the version that produced its results.

## Prompt

You are a contributor to this Go repository (a Kubernetes energy-management project). READ-ONLY task: do not create, edit or delete any file, do not run go build or go test.

A pull request is about to merge. It renames the controller manager env var `STATIC_HP_FRAC` to `POLICY_STATIC_PERFORMANCE_FRACTION`, keeping the old name as a deprecated alias for one release. The Go code and its unit tests are done and green.

Question: before this merges, what else in this repository must be checked or updated so the repository stays consistent with the code? Give concrete file paths and the commands you would run to find and verify them. Also: experiment 01 under `experiments/` was last run with the old name and its results cannot be reproduced now; say exactly what you would do with that experiment's files. Max 40 lines.

## Expect

- git grep
- 05-configuration-reference
- policies\.md
- integration_runner|ci/
- deprecated\.go|legacyEnvAliases|envOrDeprecated
- REPORT\.md
- Joulie v[0-9X]|last (working )?version|version boundary|produced with .*version

## Baseline

Without the skill (sonnet, read-only, 2026-09-22) the assistant grepped the tree and found the configuration reference, `policies.md`, the example README, the CI runner and the Dagger module, and correctly left experiment 01's REPORT untouched. It missed:

- where the one-release alias lives (`cmd/controller-manager/deprecated.go`, `envOrDeprecated`), so it could not say how the old name keeps working;
- the version pin: its note for experiment 01 said the results are "not bit-for-bit reproducible after the alias is removed" without stating which Joulie version produced them, which is what makes the numbers interpretable later;
- experiments 02 and 03, which set the same variable, were only flagged as "should be checked too".

With the skill (same model, same day) the answer named the alias map and `envOrDeprecated`, listed experiments 02 and 03 installers and sweep scripts as required updates, and for experiment 01 kept the measured numbers, added a note with the producing Joulie version to README and REPORT, and only then considered the scripts. All expectations matched.
