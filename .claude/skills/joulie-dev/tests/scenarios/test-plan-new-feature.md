# Test plan for a new feature

Tests the "Tests that would have caught it" section: whether the assistant
tests the whole road a value travels rather than only the component that
writes it, knows that fake clients prove nothing about the wire, and reaches
for captured input over invented input.

Baseline for this scenario is the skill *without* that section (the version
merged in #57), not the absence of the skill: it measures what the section
adds.

## Prompt

You are a contributor to this Go repository (a Kubernetes energy-management project). READ-ONLY task: do not create, edit or delete any file, do not run go build or go test. First read `.claude/skills/joulie-dev/SKILL.md` and follow it.

You are about to implement this change and you have not written the tests yet:

"The agent should discover the GPU power cap range by running `nvidia-smi --query-gpu=power.min_limit,power.max_limit --format=csv,noheader,nounits`, publish it in `NodeHardware.status.gpu.capRangePerGpu`, and fall back to the hardware catalog when the query fails or returns nothing."

Question: which tests do you write, and why is each one worth writing? For every test give the file it goes in, what it asserts, and state explicitly whether it would fail if the feature were absent or subtly wrong. Be specific to this repository. Max 35 lines.

## Expect

- controller-manager|fetchNodeHardware|hwInfoFromObject|reader\.go
- round.?trip|DefaultUnstructuredConverter|envtest
- cmd/agent/testdata/hardware|corpus|captured
- empty output|returns nothing|empty string

## Baseline

With the skill as merged in #57 but without the "Tests that would have caught it" section (sonnet, read-only, 2026-09-23), the answer proposed four solid tests in `cmd/agent/main_test.go`: the query wired into discovery, the fallback when the command fails, a separate case for the command succeeding with empty output, and a write test asserting the `capRangePerGpu` key reaches the object. It reasoned about each failing mode, including a typo in the map key.

What it did not cover, and what the section exists to fix:

- the reading side: nothing checked that the controller manager or the scheduler ever see the new field, so a feature could ship written but unread;
- the wire: its write test read back through a fake dynamic client, which returns the Go values it was handed, so a whole number maximum such as 300 W would never exercise the `int64` decode that broke three times before;
- captured input: every input was a hand written string.

## With the skill

Two runs, same prompt and model. The first ran while the fixture corpus was still being added to the tree and did not mention it; it did name the reading side, the `int64` decode and the `api/v1alpha1` round trip, and kept the empty output case.

The second ran with the corpus present and matched every expectation: a corpus machine carrying GPU query output as the only test that exercises the real command and parsing path, the empty output boundary as its own case, a fourth case asserting that a miss on both the query and the catalog publishes no `capRangePerGpu` at all rather than a zero range a consumer would read as a real cap, and a check of the existing round trip coverage instead of a duplicate of it, with the consumer side ruled out by pointing at the lines where `capRangePerGpu` is already read.

An earlier version of this scenario also required the words `int64` or "whole number". The second run covered the wire by verifying the existing round trip test rather than restating the reason, which is the judgment the skill asks for, so that expectation was testing vocabulary rather than behaviour and was removed. The round trip expectation still covers the same ground.
