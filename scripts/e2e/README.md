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

## The other thing in here: two throwaway machines and one real backup

`two-machine-backup.sh` is issue #356, and it answers a different question
from everything above. Suite B asks whether the pages work. The CLI smoke
slice asks whether the binary's contract holds. Neither can say that a
fresh install, on a machine nobody has touched, pointed at another
machine, actually pulls a backup off it, which is the only claim a user
makes.

It stands up two containers on a temporary network per case: a source
machine running a real sshd with a payload of known content, and a manager
machine running docker-in-docker, so it is a box with Docker and nothing
else. Then it installs with `scripts/install/install_docker_host.py`, the
real installer, creates a backup set through the CLI, runs it, and
compares the artifact's SHA-256 against the source's.

Docker-in-docker rather than the host's socket, because mounting the
socket would make the product's own containers siblings on the developer's
machine rather than residents of the fake one, and the installer would be
installing onto the machine the test is running on. That is the one thing
this test exists not to do.

The image under test is built from the working tree and moved across with
`docker save | docker load`, so no registry is involved and the run proves
this code. Every case asserts the engine reports the version and commit
that were installed, which is #342's shape: a stale default installed
0.1.0 once and the installer said "Installed."

Four cases, and three of them exist because an issue was closed on
evidence that stopped short of a completed install:

| case | what it settles |
|---|---|
| `plain` | the ordinary route: an explicit `--image`, and the canonical compose copied in from a checkout |
| `no-arguments` | #347 and #346. `install` with no arguments at all, from one copied `install_docker_host.py` on a machine with no checkout: no `--compose-file`, no `--ssh-key`, no `--prefix`, no `--image`, running all the way to a serving stack and then a real backup |
| `connection-cap` | #264. The source refuses a third simultaneous SSH connection from one address, with an `iptables` `connlimit` rule, which is the production rule restated. The case proves the cap bites before it trusts it |
| `lifecycle` | #343's two counting criteria: an upgrade that preserves every user, backup set and catalogued artifact, counted before and after, and a factory reset proven by the resulting install issuing an enrollment link |

Three outcomes rather than two, and the third is the point. A machine with
no Docker, or one whose daemon refuses a privileged container, cannot
perform this proof: the script says CANNOT RUN and exits 3, and
`ci-local.sh` ledgers that, so the run ends INCOMPLETE and names the proof
it could not perform. `CI_LOCAL_SKIP_TWO_MACHINE=1` is the out-loud
opt-out, and it ledgers too.

Run one case on its own with `--case`, and add `--keep-on-failure` to
leave a failing case's containers up for reading. Everything else is torn
down on success, on failure and on interrupt.
