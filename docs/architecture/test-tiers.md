# Test tiers: which tests get a machine, and what only a fake can prove

Issue #447. Rom asked for every test to run the way
`scripts/e2e/two-machine-backup.sh` does: two containers on a dedicated
network, one playing the rclone-manager machine and one playing the VPS being
backed up. I put that to four adversarial perspectives before writing
anything, and the record is on the issue. This page is the rule that came
out of it, the part that has to outlive the discussion.

The short version: the two-machine topology is the definition of the top
tier, one harness is the only way a test reaches it, and a guard decides
which tier a test is in from what it needs rather than from where somebody
put it. Every test does not run on two machines, and the second half of this
page is the list of coverage that would be silently deleted if it did.

## The three tiers

A test belongs to the tier of the heaviest thing it needs. That is the whole
rule, and the tier decides the directory.

| tier | what the test needs | where it lives | how the gate runs it |
|---|---|---|---|
| unit | nothing outside the process: fakes, `t.TempDir()`, a real SQLite file, rclone's local backend, a subprocess of this repository's own code | the package under test, under `core/internal`, `core/service`, `core/cmd` | `go test -race ./internal/... ./service/... ./cmd/...`, including under `CI_LOCAL_FAST=1` |
| integration | several real packages composed, or a real subprocess driven from outside, still with no container | `core/tests/<name>`, importing no machine package | the `go test -race ./...` step |
| machine | a source machine, optionally a storage medium, on a dedicated network | `core/tests/<name>`, reached through `core/tests/machines` | the gotestwatch step, `-race` too, never a fixed `go test` timeout |

Every tier runs under `-race` (#417). The detector is a flag on the steps
above rather than a fourth column, because it changes the binary and not the
tier: a unit test and a machine test are still told apart by what they need,
not by whether anybody asked the detector to look.

"Needs" is the operative word, in both directions. A unit test that stands up
a container is slow, needs a daemon, and goes red when the shared Docker VM
is oversubscribed for reasons that have nothing to do with the code under
test... which is exactly what happened to `core/internal/transport/rclone`
(335s, failed) and `core/service` (234s, failed) the last time four gate
lanes ran at once, while no in-process test failed. An integration test
written with fakes is worse, because it is fast and green and proves
nothing about the boundary it claims to cover.

### The machine tier is the two-machine topology

`core/tests/machines` is the Go half of what the shell script does, and
since #450 it is the whole of it: `core/tests/sftpfixture` and
`core/tests/miniofixture` were folded into it, along with the three copies
of the docker plumbing, the `INFRA:` verdict and the #456 refusal tests they
each carried.

`machines.Start(t)` creates a network for the test and nothing else.
`m.Source(t)` starts the machine being backed up on it (a real sshd,
chrooted, key-only, with iptables so it can carry the production connection
cap from #264) the first time it is asked; `m.Medium(t)` joins a real S3 API
to the same network the first time IT is asked; `m.AnotherSource(t)` gives a
second, independent machine, which is what "this address has no known_hosts
entry" needs to be a real second server rather than a rebuilt image. Nothing
starts that a test did not ask for, which is not a micro-optimisation: the
MinIO suite runs eight tests, and an eagerly started source would have added
an `ssh-keygen rsa 2048` and a container start to every one of them.

Failure shapes and probes are methods: `LimitConnections` and
`RemoveConnectionLimit` (#264), `Kill` (#161), `EstablishedConnections`,
`AcceptedLogins` and `ConnectionTable`, `AuthorizeKey`, `KnownHostsFor` and
its negative control `DecoyKnownHostsFor`, and later a blackhole. The rule
for adding one is in the last section: if the harness cannot do what a test
needs, the capability goes into the harness.

The source machine is built from `scripts/e2e/source-machine.Dockerfile`,
which `two-machine-backup.sh` builds too. One definition of "the simulated
VPS", read by both, rather than two that agree until they do not.

The manager is the test process. On Docker Desktop for macOS a host process
cannot sit on a bridge network, so by default the source publishes a port on
127.0.0.1 and the test reaches it there, exactly as the fixtures always
have; the network still exists, the source is on it, and that is what the
connection-cap probe and a medium use to reach the source by name. When the
tests run inside a manager container on that network nothing publishes a
port and the source is reached by alias. `Source.Addr()` answers correctly
either way, so a test never has to know.

`scripts/e2e/run-machine-tier.sh` (#451) is that second placement. It builds
a manager machine from a Go toolchain with a docker client, mounts the
repository at the same absolute path inside as out, joins it to the network
as an ordinary user, and runs the machine-tier packages inside it. The
manager gets the docker socket, and the driver's header argues that out
against `two-machine-backup.sh`'s refusal of the same thing: that manager
runs the real installer, so a socket would install onto the developer's
host, while this one runs the harness, which is orchestration and not the
product.

A test that puts something in front of a machine (a relay that adds latency
or drops a connection) has to re-record the machine's real host keys against
the relay's address, because known_hosts matches on address.
`Source.KnownHostsFor(t, host, port)` does that, and it is a capability
rather than a paragraph because every way of getting it wrong by hand is
silent: pin an address nothing consults and the obvious fix is to weaken the
check, or hardcode `127.0.0.1` and verify against nothing the moment the
tier runs inside a manager container.

## The guard

`core/internal/testtier` scans the module's Go files with the Go parser (so
comments are invisible to it, the same reason `scripts/architecture/ownership.go`
parses rather than greps) and applies two rules:

- `unit-reaches-container`: a file under a unit directory imports a machine
  package or execs `docker`.
- `bypasses-harness`: a file under `core/tests` execs `docker` itself
  instead of going through the harness.

It also reads `scripts/ci-local.sh` and checks that every package importing
the harness is named on the gotestwatch line and in the exclusion group of
the plain `go test` step, so a machine-tier package cannot be run under a
fixed timeout (the #256 shape) or not at all (the #160 shape).

The ledger is empty. It held eight files when the guard landed: six
container-backed tests in unit packages, moved to `core/tests/machinegate`
by #448, and two integration tests that exec'd `docker` directly, given
harness capabilities by #450 (`Source.Kill` and `Medium.HasBucket`). The
mechanism stays with an empty slice, because the next migration should find
the shape already here.

The ledger cannot go stale: a listed file that stops violating fails the
guard until it is removed, and an unlisted file that starts violating fails
the guard and is told which tier to move to. Every rule was watched to fail
against a planted violation before the guard was trusted; those controls are
in `testtier_test.go`.

## What only a fake can prove

This product deletes backups, and most of the tests that prove it does not
delete the wrong one work by putting a fault where no server can put one.
This list is the safety engineer's, and it is here so a future restructure
toward containers has to argue with it rather than delete it by accident.

| what is injected | where | why no machine can do it |
|---|---|---|
| a crash between the rename and the directory fsync inside `Commit` | `internal/lifecycle/commit_test.go`, through `testHookAfterRename` | two syscalls in one function; nothing outside the process can fault between them |
| a copy that returns partial bytes then an error, or blocks until cancelled; a delete that errors after the server deleted | `internal/lifecycle/{transfer,verify,remotedelete}_test.go`, `fakeTransport.copyFunc` | a real server produces these rarely and non-deterministically, and the reconciliation they prove is the point of the journal |
| a `DeleteRemote` that fails the test the instant it is called; an action at the exact instant a transfer is in flight; one backup set's remote unreachable while another's is fine | `internal/app/helpers_test.go` (`poison`, `beforeCopy`, `failForSourceID`) and its thirteen consumers, `service/validator_integration_test.go` | a server can show "the file is still there", which cannot distinguish "never called" from "called and refused" (#282), and has no rendezvous with the code under test (#350) |
| a `Stat` or `RemoteHash` that fails for one named path | `internal/discovery/fake_transport_test.go` | per-path failure on a real server means breaking the server |
| an archive-class object that needs a restore first (`InvalidObjectState`, FR-34) | `internal/transport/rclone/medium_test.go` | MinIO has no archive tier that answers it, and a real one costs money and hours per case |
| a crash mid-migration against a real SQLite file; snapshot restore; migration immutability | `internal/state/*`, `service/startup_integration_test.go` | nothing about a container helps, and reading the journal back through `docker cp` hurts |
| an injected clock | `internal/retention/*`, `capacity`, `health`, `alert` | a container has no clock control, and a golden retention table is a pure function |
| a real SIGKILL the instant a journal state commits | `tests/crashmatrix` | precise because the decorator is compiled into the dying process; a container changes nothing about the kill and takes away direct access to what it left on disk. Its one sftp case is machine tier, through the harness; the rest stays a subprocess harness on local disk |

What survives in the machine tier and is better there: the server's own
connection table and login counter (`Source.EstablishedConnections`,
`Source.AcceptedLogins` and `Source.ConnectionTable`), the connection cap
(#264), a network that
blackholes (an iptables DROP on the source produces a real half-open TCP no
fake can), and a container that dies mid-test (#161). The blackhole should
exist in both tiers: the fake for determinism in seconds, the DROP rule for
the real TCP behaviour.

## What it costs, measured

Read out of the gate logs rather than guessed. On a quiet machine
(`e2-gate5`, 11m44s for the whole gate with warm caches):

| suite | wall clock |
|---|---|
| `core/internal/transport/rclone`, the unit package that builds its own sshd image | 128 to 180s |
| `core/internal/app`, in-process with fakes, the local backend and real SQLite | 49 to 57s |
| every other `core/internal` package | under 20s, most under 6s |
| `core/tests/sftpintegration` (7 tests) | 117 to 171s |
| `core/tests/crashmatrix` | about 90s |
| `core/tests/miniointegration` (8 tests) | 21 to 32s |
| two-machine script | about 70s a case, four cases, plus the image build |

After #448 and #450, on the same machine:

| suite | wall clock |
|---|---|
| `core/internal/transport/rclone`, now with no container in it at all | 23s |
| `core/service`, same | 8s |
| `go test ./internal/... ./service/... ./cmd/...` with no daemon reachable | 52s |
| `core/tests/machines` (the harness and its own #161, #243 and #456 proofs) | 35s |
| `core/tests/machinegate` (the six moved tests, plus #463's) | 107s |
| `core/tests/sftpintegration` | 121s |
| `core/tests/miniointegration` | 16s |
| every machine package inside a manager container, `-race`, under gotestwatch, warm | 164s |
| the same with both caches empty (45s of which is the compile) | 169s |

A two-machine case per test function, for the 1555 test functions under
`core/internal`, `core/service` and `core/cmd`, would be thirty hours. One
per package would be thirty-five minutes of setup before a test ran, with
`go test` inside the manager container, where a cold compile of rclone's
260-module graph took over six minutes in CI. That is the number the
"literal" option was rejected on, alongside the table above.

#451 asked for that number to be measured here rather than inherited, and
it does not reproduce: the compile inside the manager container, from an
empty module cache and an empty build cache, is 45 seconds with `-race` and
about 76 without. Two
reasons, both worth knowing before either figure is quoted again. Only the
packages the tier imports get compiled, not the whole product. And this
builds arm64 natively, where `DOCKER_DEFAULT_PLATFORM=linux/amd64` is set on
this machine and, left in force, had the manager built emulated and the
measurement measuring qemu. The conclusion above is unchanged: 76 seconds
per package, times the number of packages, is still the reason the tier is a
tier and not a default.

## Writing a new test

Ask what it needs. If the answer is a fake, a temp directory or a real
SQLite file, it is a unit test and lives with the package. If it composes
several real packages or drives a subprocess and needs no container, it is
integration and lives under `core/tests/<name>`. If it needs a machine, it
calls `machines.Start(t)`, lives under `core/tests/<name>`, and the gate
runs it under gotestwatch, which the guard checks.

Never write `exec.Command("docker", ...)` in a test. If the harness cannot
do what the test needs, the capability goes into the harness, where the
watchdog and the sweep and the bounded calls apply to every test that
comes after.
