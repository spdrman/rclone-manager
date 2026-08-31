# The browser e2e signal

The Playwright suite used to live in `ui/shared/e2e/`. It does not any
more: issue #158 moved it to
[`spdrman/rclone-manager-tests`](https://github.com/spdrman/rclone-manager-tests)
as Suite B, so it tests this product the way an operator meets it, from
outside, with nothing but a browser and a built artefact.

This directory is what replaced it, and the replacement had to land in the
same change as the removal. Issue #197 is why: before it, the suite had no
automated execution anywhere. `nightly-e2e.yml`'s schedule was commented
out, every workflow here is `workflow_dispatch`-only, and
`scripts/ci-local.sh` never invoked Playwright. So it ran when somebody
remembered to run it, and a deterministically red spec sat on `main`
through four merges and was dismissed twice as an ordering flake.

## What runs, when, and how a red blocks

`scripts/ci-local.sh` runs `run-tests-repo-gate.sh` on every non-FAST run,
which is every commit through `.husky/pre-commit`. The step:

1. clones `rclone-manager-tests` at the sha in `tests-repo.pin` into
   `${XDG_CACHE_HOME:-$HOME/.cache}/rclone-manager-tests-gate/<sha>`, once
   per pin;
2. builds `backup-manager` from **this working tree** and runs that
   repository's CLI smoke slice against it, 55 black-box cases in about
   eleven seconds. That is a signal this repository has never had: nothing
   here exercised the CLI black-box on a per-commit basis at all;
3. runs Suite B's own unit tests, and then Suite B itself, 165 browser
   tests in about twenty-two seconds, against **this working tree's**
   `ui/shared` on a port the harness picked and proved free.

A red suite exits nonzero, `ci-local.sh` runs under `set -e`, so the
commit is refused and the verdict line names the step. `--no-verify` stays
the documented WIP escape hatch it already was.

The build under test is the working tree, never the pin's own idea of a
build. The `-ldflags` in the step are load-bearing rather than cosmetic:
without them the binary reports `commit none`, and the tests repository's
identity handshake aborts the run rather than certifying a build it cannot
name.

## A machine with no browser

Refused, with the fix named, and `CI_LOCAL_SKIP_E2E=1` as the out-loud
opt-out that ledgers the skip so the run ends `INCOMPLETE` instead of
`ok`. That is the same shape `gate_require_docker` uses for a stopped
daemon, and the argument is consistency: Docker is the higher-consequence
capability and it is refuse-by-default with a ledgered opt-out, so a
browser gets that and not something weaker. An `INCOMPLETE` run is not
merge evidence and says so.

## Moving the pin

```sh
scripts/e2e/bump-tests-pin.sh              # the pinned branch's tip
scripts/e2e/bump-tests-pin.sh <full sha>   # an exact commit
```

The bump carries no proof of its own, deliberately. The commit that lands
it runs through the gate like any other, and the gate runs the newly
pinned suites, so a pin that points at a red or unreachable tests commit
cannot be committed. A check that ran in the bump script as well would
just be a check that can disagree with the gate.

## When the pin and the working tree disagree

They are different builds by construction, and a case can legitimately go
red because this tree predates a behaviour the pinned suite expects, or is
ahead of one. That is the suite working, and the fix is to read which case
failed rather than to loosen it: every case in that repository names, in a
`contract:` list, the FR number or document section its expectation came
from. A deliberate behaviour change here lands with the matching case
change there and the pin bump in the same PR.

## The other two directions

- **`.github/workflows/nightly-e2e.yml`** checks the same repository out at
  the same pin and runs the full Suite B including the seven-provider
  matrix, still `workflow_dispatch`-only. It exists for a run on a clean
  machine with a downloadable trace, not as a gate: nothing triggers it.
- **`rclone-manager-tests` pins a build of this repository**, in its own
  `build-under-test.json`. The two pins point opposite ways on purpose. A
  new test cannot break in-flight work here until someone bumps this one,
  and a release here cannot silently change what those suites certify.
  Each bump is one line in one file.

The design, including the parts that differ from what was originally
specified and why, is in that repository's `docs/ci-signal.md`.
