# rclone-manager

A backup lifecycle manager for a UGREEN NAS. It ingests backup artifacts produced by
remote application and database jobs, pulls them over SFTP, verifies them, commits them
durably, and only then deletes the remote copy.

It is a standalone Go binary that **embeds pinned rclone Go packages**. It does not fork
rclone, and it does not shell out to the `rclone` CLI for normal data movement.

## The rule everything else serves

> A remote backup artifact MUST NOT be deleted until a verified and durably committed NAS
> copy exists. If state is uncertain, preserve the remote copy.

## Who owns what

rclone owns the data plane: SFTP and local backends, listing, copying, hashing, deletion
primitives, transfer accounting. This project owns the control plane: backup-set config,
artifact discovery, the durable lifecycle journal, copy/verify/commit/delete sequencing,
GFS retention, last-known-good protection, validation and quarantine, freshness, health,
and reconciliation after a crash.

Put another way:

```text
rclone:
    move bytes reliably

backup-manager:
    decide what those bytes mean,
    when they are safe,
    when the source may be destroyed,
    and which restore points must survive
```

That boundary is the central architectural constraint. Application packages outside
`internal/transport/rclone` must not import rclone packages, so upstream API churn stays
contained in one adapter.

## Two decisions that are easy to get wrong

**rclone `move` is not the backup transaction.** Copy through rclone, verify and commit
under manager control, persist the commit in SQLite, revalidate remote object identity,
and only then delete the remote source explicitly. A `move` collapses those steps and
takes the delete decision away from the manager.

**Phase 1 is a gate, not a warm-up.** It proves rclone can be embedded behind the
manager-owned transport interface without leaning on unstable internals. If that gate
fails, the answer is a subprocess architecture, not a fork.

## Retention defaults

| Tier    | Default           |
|---------|-------------------|
| Daily   | 7 days            |
| Weekly  | 3 calendar months |
| Monthly | 12 calendar months|

## Status

Nothing is implemented yet. `docs/EPIC.md` is the full specification, including 24
functional requirements, the security and failure-safety invariants, the rclone upgrade
compatibility contract, the testing matrix, and the five-reviewer adversarial consensus
that settled the architecture. The delivery plan and its phases are tracked as issues.

## Layout

The repository root is the Go module root. `cmd/backup-manager/` is the entry point,
`internal/` holds the application packages, and every rclone import stays inside
`internal/transport/rclone/`. The full tree is in `docs/EPIC.md`.

The project was first scoped as `tools/backup-manager/` inside `iasbuilt/iac`. It lives
here instead, and the specification says so. Nothing in the design depended on the
location, so the move cost nothing beyond the wording.

## Toolchain

Go 1.27, and Docker for the disposable SFTP server the integration tests use.

The certified rclone version is pinned in `go.mod`. Do not move it without running the
full regression set described in `docs/rclone-upgrade.md`, which turns the rclone Upgrade
Compatibility Contract in `docs/EPIC.md` into an actual checklist and the CI gate that backs
it. Automatic merge is never enabled for that dependency, that rule is non-negotiable and
spelled out in `docs/rclone-upgrade.md`.

```bash
go build ./...
go vet ./...
go test ./...
```

CI (`.github/workflows/ci.yml`) runs the same three commands on every push and pull
request, with the Go module cache preserved between runs, and separately cross-compiles
`cmd/backup-manager` for both UGREEN targets (`linux/amd64` and `linux/arm64`,
`CGO_ENABLED=0`) as a compile check. `.github/workflows/rclone-upgrade-gate.yml` runs
whenever `go.mod` or `go.sum` changes and reports the FR-2 checklist status.

The architecture decision behind embedding rclone this way, and what it costs, is in
`docs/adr/0001-embed-rclone-behind-transport-adapter.md`.
