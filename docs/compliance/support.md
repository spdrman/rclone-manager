# Support

Backup Manager, `com.iasbuilt.backupmanager`. This is the support material
§73 Work Package 5.2 requires, and it is what the `support` link in
`distribution/packaging/compliance.json` resolves to.

## Where to get help

Open an issue at <https://github.com/spdrman/rclone-manager/issues>.

That address is the single support channel. There is no separate support inbox,
no forum and no chat, on purpose: a channel nobody reads is worse than one
channel that is read, and a store listing that advertises three of them makes a
reviewer check three.

The repository is public, so the issue tracker is reachable by anyone with a
GitHub account. `provenance/release-provenance.json` records that in
machine-readable form (`links.publiclyReachable`), derived from
`distribution/packaging/compliance.json`'s one measured visibility field rather
than asserted here, so this page and the bundle cannot drift apart.
`docs/compliance/source-offer.md` still stands for anyone who has a package and
no browser: Apache-2.0 §4a is owed to a recipient, not to a visitor.

## What to include in an issue

Four things turn a report into something actionable, and all four come out of
the app itself rather than out of memory:

1. The version. `/backup-manager version` prints it, and it matches the
   `version` field of `container/release-manifest.json` for the release you are
   running.
2. The platform and how the app was installed: which NAS or hypervisor, and
   which of the packaging paths in `docs/acceptance/` you followed.
3. What the app says about itself. `/backup-manager status` reports each backup
   set's health state, and it is the same signal the container health check
   reads.
4. The relevant log lines. Secrets are redacted before anything is written, so
   the log is safe to paste, but read it before you do.

## Recovery

Recovery has its own procedure, and it is deliberately written so that it can be
followed without a terminal: `docs/recovery.md`. Read it before filing an issue
about lost or unreachable data, because the answer to "how do I get my files
back" should not depend on anyone answering an issue first.

The short version of the design behind that document: retained artifacts are
ordinary files at an ordinary path on the destination, put there by rclone. They
are not in a proprietary container, and reading them back never requires this
app to be running or even installed.

## Severity and what to expect

- **Data loss, data corruption, or a security problem.** Say so in the first
  line of the issue. These come first.
- **A backup that will not run, or reports unhealthy.** Include the four items
  above; most of these are resolved from the log and the status output.
- **Everything else.** Feature requests and questions are welcome in the same
  tracker.

This is a project run by its authors rather than a commercial support contract,
so no response-time commitment is offered here. Making one up for a store
listing would be worse than saying this.

## Diagnosing before you file

- `/backup-manager status` gives every backup set's health state and exits
  non-zero when any of them is degraded, stale or failing.
- `/backup-manager check` validates the configuration without moving data.
- `/backup-manager validate` exercises the configured transport, including the
  SSH host-key policy, which is the most common cause of a set that never
  starts.

Each of those runs read-only against the same state the scheduler uses, so
running them while the app is up changes nothing.
