# rclone upgrade procedure and regression gate

This is the FR-2 checklist from `docs/EPIC.md` ("rclone Dependency Management"),
written down as something you can actually follow, plus the CI gate that backs
it. Read `docs/EPIC.md` first if you haven't, this file assumes it.

## The rule, stated once, non-negotiable

**No rclone dependency bump auto-merges. Ever.** Not from Dependabot, not from
a green CI run, not because the diff is "just a patch version." A person reads
the release notes and signs off. This is written into
`.github/dependabot.yml` as a comment, enforced by omission (there is no
auto-merge workflow in this repository for the `gomod` ecosystem), and it must
stay that way. If you're reading this because you're about to add one, don't.
The whole point of embedding rclone instead of forking it was to keep a human
in the loop on every upstream change that reaches this binary, and auto-merge
removes exactly that human.

## Why this needs its own gate instead of just "green CI"

Ordinary CI (`.github/workflows/ci.yml`) tells you the module still builds,
vets clean, and passes whatever unit tests exist. That is necessary for an
rclone upgrade but nowhere near sufficient. FR-2 lists eight requirements, and
a dependency bump PR that only proves compilation is not a reviewed upgrade,
it's a `go build` with commentary.

## The eight FR-2 requirements, and their state today

| # | Requirement | Status | Where |
|---|---|---|---|
| 1 | Dependency update | **Enforced** | The Dependabot/manual PR itself is the update. The gate workflow records the old and new pinned version. |
| 2 | Compilation | **Enforced** | `go build ./...` runs in both `ci.yml` and the dedicated gate workflow. |
| 3 | Unit tests | **Partially enforced** | `go test ./...` runs on every PR. Today the module has no `*_test.go` files, so this currently runs clean but exercises nothing. Once the suite lands (`#30`), this line becomes real enforcement with no workflow changes needed, `go test ./...` picks up whatever exists. |
| 4 | Transport contract tests | **Pending** | Tracked in `#30` (A1.6). Will live under `internal/transport` as ordinary `_test.go` files, so it also rides on `go test ./...` once written. |
| 5 | SFTP integration tests | **Pending** | Tracked in `#31` (A2.13). Needs a disposable SFTP server (Docker is already available in this environment for exactly that). Not wired into any workflow yet because the suite doesn't exist to wire in. |
| 6 | Crash / reconciliation tests | **Pending** | Tracked in `#31`. The crash matrix in `docs/EPIC.md` under Testing Requirements. |
| 7 | Destructive-safety tests | **Pending** | Tracked in `#31`. Malicious paths, symlinks, replaced remote objects, malformed config, stale journal state. |
| 8 | Upstream release notes / changelog review | **Manual, permanently** | This is a human reading prose and judging risk. It cannot be automated away, and pretending otherwise would defeat the point. The gate workflow surfaces the changelog and release URLs for the exact version range so there's no excuse to skip it, but it does not and cannot verify that anyone read them. |

"Pending" here means the suite does not exist in this repository yet, not
that it's optional. `.github/workflows/rclone-upgrade-gate.yml` says so
explicitly in its job summary for each pending item, with the tracking issue,
rather than showing a green checkmark next to a suite that never ran. Once a
pending suite lands, whoever lands it should update the corresponding step in
that workflow to actually invoke it, and flip this table's row to Enforced.

## Doing an upgrade, step by step

1. **Let Dependabot open the PR, or open one yourself.** Either way it should
   touch only `go.mod` and `go.sum`. Dependabot is configured in
   `.github/dependabot.yml` to check weekly and label the PR `rclone-upgrade`.
   If you're bumping by hand: `go get github.com/rclone/rclone@vX.Y.Z` and let
   `go mod tidy` resolve the graph. Expect this to be slow, resolving rclone's
   module graph from cold takes 6+ minutes and pulls down roughly 1.7GB across
   ~260 modules (see "What actually gets pulled in" below), so don't do this
   on a whim or in a hot loop.
2. **Read the release notes before you read the diff.** Every rclone release
   between the old pin and the new one, not just the latest. Look specifically
   for changes to: the `fs` interfaces this adapter consumes, the `sftp` and
   `local` backends, `fs/operations.Copy`, hashing support, and anything
   flagged as a breaking or security change. The gate workflow's job summary
   links the changelog and release pages for you; it does not read them for
   you.
3. **Let the gate workflow run.** `.github/workflows/rclone-upgrade-gate.yml`
   triggers on any PR touching `go.mod` or `go.sum` and writes the FR-2
   checklist status to the job summary, with old and new version, per item.
4. **Run what exists locally too, don't just trust CI.** At minimum:
   ```bash
   go build ./...
   go vet ./...
   go test ./...
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /dev/null ./cmd/backup-manager
   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./cmd/backup-manager
   ```
5. **Check what got registered, not just what got imported.** See the next
   section, this has bitten people before.
6. **Get a real review.** Someone other than the author reads the release
   notes summary and the diff, and says so in the PR. A rubber-stamp approval
   on a dependency bump PR is how a silent behavior change in a destructive
   code path reaches production.
7. **Merge by hand.** Never squash-and-auto-merge a dependency PR for this
   ecosystem, see the rule at the top of this file.
8. **Update the certified version** wherever it's recorded (`README.md`,
   `backup-manager version` output) if that hasn't already been done as part
   of the PR.

## What actually gets pulled in: three different questions

It's easy to conflate three things that are actually independent, and the
difference matters when you're reasoning about what an rclone upgrade can
possibly affect.

**The module graph** is everything `go.mod`/`go.sum` resolve, whether or not
this binary ever touches it. Importing only `backend/local` and
`backend/sftp` still causes `go mod tidy` to resolve rclone's *entire* module
graph, because Go resolves the whole graph rclone's `go.mod` declares, not
just the transitive closure of the packages you import. That's roughly 1.7GB
and 260 modules, including every cloud SDK rclone supports (S3, Azure,
Google Cloud, Dropbox, and so on) even though none of that code is reachable
from this repository's `import` statements. This is why a cold `go mod tidy`
is slow and why the module cache needs to be cached in CI, not why the binary
is large.

**What gets linked into the binary** is a much smaller set: the Go linker
only includes code reachable from `main`. Building
`./cmd/backup-manager` for `linux/arm64` with `CGO_ENABLED=0` produces a
21MB binary, nowhere near what you'd get if every cloud SDK in the module
graph were actually linked in. Unused packages in the module graph cost
resolve time and disk in the module cache; they do not cost binary size,
because nothing imports them.

**What gets registered at runtime** is smaller still than "what's linked",
but it is not the same as "what we explicitly imported", and this is the part
that has actually surprised us. `internal/transport/rclone/adapter.go`
deliberately imports exactly two backend packages for their registration side
effect:

```go
_ "github.com/rclone/rclone/backend/local"
_ "github.com/rclone/rclone/backend/sftp"
```

But the adapter also imports `github.com/rclone/rclone/fs/operations` for
`operations.Copy`, and `fs/operations` itself imports
`github.com/rclone/rclone/backend/crypt`. Backend packages register
themselves via `init()`, so importing `fs/operations` silently registers a
**third** backend, `crypt`, that nothing in this repository asked for by
name. Confirm this yourself with:

```bash
go mod why github.com/rclone/rclone/backend/crypt
```

which currently prints the exact chain: this adapter -> `fs/operations` ->
`backend/crypt`. FR-4 says "only required rclone backends SHOULD be
registered", so an upgrade that changes which stable-API packages pull in
which backends transitively is exactly the kind of thing item 4 (transport
contract tests) and item 8 (release notes review) above exist to catch. If an
upgrade adds a fourth transitively-registered backend, or removes `crypt`'s
registration, that's a real behavior change worth noticing, and today the
only thing that will notice it is a human running the `go mod why` command
above or reading the diff of `go list -deps` output, since no automated check
does this yet.

## Rolling back

The pin is a single line in `go.mod`. If a certified upgrade turns out to
regress something after merge, reverting is `go get
github.com/rclone/rclone@<previous-version>`, `go mod tidy`, and the same PR
process in reverse, not a hotfix branch juggling a partial upgrade. This is
one of the reasons the version is pinned explicitly rather than left on
"whatever `go get -u` finds": rollback has to name an exact known-good
version.
