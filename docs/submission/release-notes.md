# Release notes

Version 1.0.0, the reference every provider package carries. It is pinned in one place,
`distribution/packaging/canonical.json`, and every target's metadata is checked against it
on every commit, so a release note describing 1.0.0 beside a package deploying something
else is a failing build rather than a review catch.

## 1.0.0

First release. Everything below is new, so this is a description of the product rather
than a diff.

### What you get

- Scheduled pull-based collection of backup artifacts from a remote SFTP source, with the
  remote's host key pinned before the first transfer rather than trusted on first use.
- Hash verification of every artifact before it counts as retained, and a durable SQLite
  catalog of every artifact held, including the ones that failed and why.
- Two-step retention: a preview of exactly which restore points a policy would delete, and
  an apply that runs only the plan you confirmed. A plan that has gone stale deletes
  nothing.
- A web interface covering configuration and monitoring without a terminal, served from a
  container separate from the engine, with local account authentication and one-time
  enrollment.
- Proactive alerting on stale backups, repeated failures, a changed SSH host key, and
  critical storage pressure.
- One canonical multi-architecture image on amd64 and arm64, wrapped by every provider
  package rather than rebuilt per platform.

### Known limitations in this release

Listed because a store review reads them and an administrator deserves them before
installing, not after.

- There is no restore command. Recovery is a documented procedure; see the support
  materials for where it lives.
- Native platform notifications are delivered only where the platform offers a local
  notification capability the app can adapt. On the targets in this release the
  administrator's path to an alert is the app's own dashboard.
- Native platform sign-on is not wired up on these targets; the app uses its own local
  account.

### Upgrading

There is nothing to upgrade from. Future releases ship as a new package: this application
never replaces its own reviewed code, never pulls a floating tag, and has no self-update
path of any kind. See the privacy disclosure and the permission rationale for what that
means in practice.
