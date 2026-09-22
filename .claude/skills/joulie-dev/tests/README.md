# Behavioural tests for the joulie-dev skill

A skill is only as good as the behaviour it produces. These tests give an
assistant a realistic Joulie task and check that its answer shows the
knowledge the skill is meant to carry: the right files, the right commands,
the pitfalls avoided.

Each scenario in `scenarios/` has three parts:

- `## Prompt`: the task, given verbatim to the assistant. Read-only tasks
  (plans, reviews, diagnoses) so a run never edits the repository.
- `## Expect`: one regular expression per line, matched case-insensitively
  against the answer. A scenario passes when every expectation matches.
- `## Baseline`: what an assistant answered without the skill when the
  scenario was written, so a future edit to the skill can be judged against
  the failure it was meant to fix.

## Running

Requires the `claude` CLI. From the repository root:

```bash
.claude/skills/joulie-dev/tests/run.sh              # with the skill (repo as is)
.claude/skills/joulie-dev/tests/run.sh --baseline   # same prompts, skill hidden
.claude/skills/joulie-dev/tests/run.sh nodetwin-field   # one scenario
```

`--baseline` copies the repository to a temporary directory without
`.claude/skills/joulie-dev` and runs there, so the two runs differ only by
the skill. Expect the baseline to fail; if it passes, the scenario is not
testing the skill.

Runs are non-deterministic and cost tokens. Treat a failure as "read the
answer", not as a red build: the answer is saved next to the results so you
can see which expectation was missed and whether the expectation or the
skill is wrong.

## Adding a scenario

1. Write the prompt for a task the skill should change.
2. Run it with `--baseline` first and paste the shortcomings into
   `## Baseline`. If the baseline already does everything right, the skill
   does not need that scenario.
3. Write expectations for exactly those shortcomings, as specific phrases the
   right answer must contain (a file path, a make target, a rule).
4. Run with the skill; edit the skill until it passes; keep the expectations
   honest rather than loosening them.
