# The browser e2e signal

The Playwright suite used to live in `ui/shared/e2e/`. It does not any
more: issue #158 moved it to
[`spdrman/rclone-manager-tests`](https://github.com/spdrman/rclone-manager-tests)
as Suite B, so it tests this product the way an operator meets it, from
outside, with nothing but a browser and a built artefact.

This directory is what replaced it, and the replacement had to land in the
same change as the removal. Issue #197 is why: before it, the suite had no
automated execution anywhere. `nightly-e2e.yml`'s schedule was commented
out, no workflow here triggered on anything at all, and
`scripts/ci-local.sh` never invoked Playwright. So it ran when somebody
remembered to run it, and a deterministically red spec sat on `main`
through four merges and was dismissed twice as an ordering flake.

## What runs, when, and how a red blocks

`scripts/ci-local.sh` runs `run-tests-repo-gate.sh` on every non-FAST run,
which is every commit through `.husky/pre-commit`. That file is a four-line
bash shim that execs `scripts/rcmtools/e2e/run_tests_repo_gate.py`, which is
the gate (#672); it stays a file at that path because
`scripts/tests/ci-local-gate.test.sh` fabricates a stand-in gate there inside a
sandbox tree, and `ci-local.sh` keeps calling the shim so that stand-in is the
thing that runs. The step:

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
python3 scripts/rcmtools/e2e/bump_tests_pin.py              # the pinned branch's tip
python3 scripts/rcmtools/e2e/bump_tests_pin.py <full sha>   # an exact commit
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
  `ci.yml` is the one workflow here that does trigger on its own, on a
  pull request into `release` (#575), and Suite B is not in it.
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

### Where it runs besides here

All four cases run on every pull request into `release`, as the
`two-machine-e2e` job in `.github/workflows/ci.yml` (issue #575). Merging
into that branch publishes a signed image to a public registry, and this
is the only test in the tree that says the thing being published works.

CI calls it through `two-machine-ci.sh` rather than directly, because the
three outcomes above have to survive a workflow step, which has two. A
runner that cannot perform the proof comes out red under the word
INCOMPLETE, not green under no word at all: green there would be a
published release resting on a run that never happened.
`scripts/tests/two-machine-ci-verdict.test.sh` is what holds that in
place.

A GitHub-hosted `ubuntu-latest` runner does carry it, established by
running it rather than by reading the documentation: privileged containers
are allowed so docker-in-docker starts, `xt_connlimit` loads so the
connection-cap case can impose its rule, and the runner has around 87 GiB
free against the installer's 2048 MiB refusal. The whole job, image build
and all four cases, is about four and a half minutes there, against
roughly a minute per case plus the build on the machine this was written
on.

Run one case on its own with `--case`, and add `--keep-on-failure` to
leave a failing case's containers up for reading. Everything else is torn
down on success, on failure and on interrupt.

## Both drivers' `--help` is a pinned block, not a line range

`two-machine-backup.sh` and `scripts/rcmtools/e2e/run_machine_tier.py` print
their `--help` from the header block between `# HELP-START` and `# HELP-END`
near the top of each file.
That used to be a range of line numbers, `sed -n '2,110p' "$0"`, so the help an
operator reads was a set of coordinates rather than a piece of text: a comment
inserted above the boundary rewrote it and deleting one truncated it, with
nothing anywhere rendering either script's help. Both had already drifted by the
time #514 was written, one of them to a sentence cut off inside a word.

Edit the header freely; move a marker if you want the block to cover more or
less. `scripts/tests/e2e-help.test.sh` renders both drivers and diffs them
against `scripts/tests/testdata/*.help.txt` on every gate run, the way
`core/tests/compat` pins the CLI under FR-35 clause 4, so a reword fails until
somebody updates the golden on purpose. It also proves the property that used to
be missing: a comment added above the block leaves the rendered help unchanged.

## Reproducing #730 (the Activity fetch that throws)

`rclone-manager#730` is the Activity page's `fetch(/api/v1/activity)`
throwing `TypeError: Failed to fetch` on a real 0.4.0 NAS, while `curl` to
the same route answers cleanly. The client request is byte-for-byte the
same relative, same-origin GET every other page makes (`ui/shared/src/api/
client.ts`, `BASE = "/api/v1"`, `credentials: "same-origin"`), so a
same-origin *relative* fetch cannot be failing on CORS or mixed content.
What the operator's browser has and this plain-HTTP rig did not is a front
reverse proxy terminating TLS and speaking **HTTP/2** — browsers only
negotiate h2 over TLS, so the default rig drives the whole stack over
HTTP/1.1 and never exercises that transport.

`--front-proxy-tls` adds that missing hop:

```
browser --TLS/HTTP2--> nginx (proxy-machine) --HTTP/1.1--> serve-ui --> serve
```

Run it against the real published 0.4.0 image (the artefact #730 was seen
on), rather than a build from this tree:

```sh
docker pull ghcr.io/spdrman/backup-manager:0.4.0
scripts/e2e/three-machine-web-ui.sh \
  --image ghcr.io/spdrman/backup-manager:0.4.0 \
  --front-proxy-tls
# optionally enlarge the authenticated /api/v1/activity payload:
RM_SEED_CYCLES=8 scripts/e2e/three-machine-web-ui.sh \
  --image ghcr.io/spdrman/backup-manager:0.4.0 --front-proxy-tls
```

The built-in `web-ui-smoke.mjs` client counts a failed request or an
uncaught rejection on the Activity page as a failure, which is exactly
#730's shape; `--suite ../rclone-manager-tests/suites/web-ui` runs the full
Suite B, whose `real-path.spec.ts` asserts `getByRole("alert")` is absent
on `/activity`. Either goes **red** if #730 reproduces over this transport.

It is a genuine experiment, not a guaranteed repro. The proxy
(`proxy-machine.Dockerfile`, `proxy.nginx.conf`) is a deliberately ordinary
operator front door, not one built to trip the bug. If an ordinary h2 front
proxy in front of the real 0.4.0 image turns the Activity page red, the
fault is in what serve-ui / the engine put on the wire for that one route;
if it stays green even here, the trigger is more specific to the operator's
own front end (their proxy build, TLS stack, or browser), and the rig has
narrowed it either way.
