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

## The third thing in here: a browser that has to use the wire

`three-machine-web-ui.sh` exists because of a sentence in the section
above that is easy to read past. Suite B "drives a mock through a dev
server, so it proves a component renders rather than that the product
works." That is not a caveat, it is a structural ceiling, and the run it
let through was on real hardware: the Activity page errored in the browser
while the server answered HTTP 200 with valid JSON in twenty milliseconds.
Nothing watching the server can see that. Nothing mocking the client can
either, because a mock answers with the shape the client already expects.

So this stands up the whole path and puts a real browser at the end of it:

```
  CLIENT ── edge ── web-ui ── internal ── engine ── backhaul ── VPS
 Playwright         serve-ui              serve                sshd
 + Chromium         + proxy               + SQLite             + files
```

Three machines. Four containers, because the middle machine is the product's
own two-container split (`/rbm-web serve` and `/rbm-web serve-ui`) and
collapsing it into one would delete the reverse-proxy hop, which is half of
what this exists to cover.

Three networks rather than one, and that is the topology doing work rather
than decoration. The client can reach the UI container and nothing else, so
a spec cannot pass by talking to the engine directly, and the engine runs
with `TRUST_FORWARDED_HEADERS=true` next to the only peer
`container/compose.yaml` says may ever be its peer. Nothing is published to
a host port on any of the three.

### It tests the running product, not the installer

`two-machine-backup.sh` runs its manager machine as docker-in-docker and
installs onto it with the real installer, and its own comment says why this
one cannot borrow that: the dind daemon is a network namespace of its own,
so 8080 in there is not 8080 out here and a sibling client container has no
route to it. The two ways out were to publish the inner port back out onto
the shared network, or to drop dind. This drops dind, because publishing
back out means a browser talking to a port forwarded by a daemon inside a
daemon, and every reset on that hop would be a question about the rig
rather than about the product.

What that gives up is worth saying plainly rather than leaving to be
discovered: **a green run here says nothing about whether an operator can
install this.** `two-machine-backup.sh` is still the only test that
`scripts/install/install_docker_host.py` takes a bare machine to a working
deployment, and it has to stay that way.

### What is seeded, and where the credentials come from

The VPS holds three files of known content. The deployment is configured
with a read-only backup set pointing at it over SSH, one cycle is run so
the journal and the catalogue have real rows in them, and an administrator
is enrolled with `rbm-web auth create-admin`. So a browser can sign in and
read data that came off another machine.

Nothing here is committed and nothing is reused between runs. The SSH
keypair is generated INSIDE a container and lives in a Docker volume that
teardown destroys, so its private half never touches the host at all: that
is also what satisfies `core/internal/transport/rclone/ssh.go`, which
refuses a key that is not exactly 0600 or that sits under a group-writable
directory, and a bind mount cannot promise either across a Docker Desktop
share. The administrator's password is generated per run, reaches the
product down a pipe into stdin, and is printed on the terminal because a
suite that has to sign in needs it.

### Running it

```sh
scripts/e2e/three-machine-web-ui.sh
    # stand it up and run the built-in browser check

scripts/e2e/three-machine-web-ui.sh --suite ../rclone-manager-tests/suites/web-ui
    # ... and run that directory's Playwright suite in the client container

scripts/e2e/three-machine-web-ui.sh --keep-up
    # stand it up, print how to drive it, and leave it running
```

`--image REF` skips the product build and uses an image already on the
machine, `--artifacts DIR` says where traces and screenshots land, and
`--keep-on-failure` leaves a failed stack up for reading. `--help` prints
the full reasoning, from the same pinned header block both other drivers
use.

The exit status is the client container's, never the teardown's: a run that
tore down cleanly after a red suite is a red run.

### The contract with the suite

The client container is where `npx playwright test` runs. Not Playwright on
the developer's machine driving a remote browser, and not a bare Chromium
something else attaches to: the runner, the browser and the page all share
one network position, which is why a spec cannot reach the engine by
accident. The image is `scripts/e2e/client-machine.Dockerfile`, the
official Playwright image pinned by digest the way `container/Dockerfile`
pins its own bases, and its `@playwright/test` version tracks
`suites/web-ui/package-lock.json` in the tests repository. The two have to
move together: the browsers live in the image under a build id the npm
package computes, so a skew reads as "Executable doesn't exist" rather than
as a version problem.

These five variables are the whole handover:

| variable | what it is |
|---|---|
| `RM_BASE_URL` | `http://rclone-manager:8080`, resolved by Docker's embedded DNS on the edge network. Never localhost, and nothing is published to the host |
| `RM_ADMIN_USERNAME` | the enrolled administrator |
| `RM_ADMIN_PASSWORD` | its password, generated this run |
| `RM_BACKUP_SET` | the seeded set's name |
| `RM_ARTIFACTS_DIR` | `/artifacts`, mounted out to the host and never removed by the teardown |

A Playwright config that sees `RM_BASE_URL` must use it as `baseURL` and
must not start a web server of its own. There is one, it is another
container, and `npm run dev` in there would serve the mock this whole thing
exists to get away from.

The suite is MOUNTED rather than baked in, because specs change every time
somebody works on them and the runtime changes when a lockfile moves. Its
own `node_modules` is not used: an anonymous volume over `/suite/node_modules`
is seeded from the image's linux install, so a `node_modules` built for the
developer's Mac never reaches the container.

### Two things about running a browser in a container

Both are the failures everyone meets once, so they are settled here rather
than left to be rediscovered. Chromium's renderers put their shared buffers
in `/dev/shm` and Docker's 64 MiB default is not enough for a 1440x900 page,
so the client gets `--shm-size=1g`; `--ipc=host` would also work and is a
much larger hole than the size default is a problem. And the client runs
`--init`, because Chromium leaves zombies when its parent is a test runner
rather than an init, and a few hundred defunct processes is how the next run
finds no pids left.

The client runs as the invoking user's uid so the traces and screenshots it
writes are owned by whoever has to read them. Chromium's own sandbox stays
on: it was checked rather than assumed, and `RM_CHROMIUM_NO_SANDBOX=1` is
there for a host where it cannot be.


## Every driver's `--help` is a pinned block, not a line range

`two-machine-backup.sh`, `run-machine-tier.sh` and `three-machine-web-ui.sh`
all print their `--help` from the header block between `# HELP-START` and
`# HELP-END` near the top of each file.
That used to be a range of line numbers, `sed -n '2,110p' "$0"`, so the help an
operator reads was a set of coordinates rather than a piece of text: a comment
inserted above the boundary rewrote it and deleting one truncated it, with
nothing anywhere rendering either script's help. Both had already drifted by the
time #514 was written, one of them to a sentence cut off inside a word.

Edit the header freely; move a marker if you want the block to cover more or
less. `scripts/tests/e2e-help.test.sh` renders every driver and diffs each
against `scripts/tests/testdata/*.help.txt` on every gate run, the way
`core/tests/compat` pins the CLI under FR-35 clause 4, so a reword fails until
somebody updates the golden on purpose. It also proves the property that used to
be missing: a comment added above the block leaves the rendered help unchanged.
