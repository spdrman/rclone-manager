# Privacy policy

Backup Manager, `com.iasbuilt.backupmanager`. This is the privacy disclosure
§73 Work Package 5.2 and §45.5 require, and it is the content the `privacy`
link in `distribution/packaging/compliance.json` resolves to.

Written as statements about what the shipped code does, so that each one can be
checked against the tree rather than taken on trust. Where a claim is checkable
by a test or a grep, the check is named.

## What leaves the machine

Nothing that Backup Manager itself originates.

The app sends no telemetry, no analytics, no crash reports, no usage counters
and no licence or activation call. There is no cloud account, no vendor
back end and no phone-home on first run or on any run after it. §45.5 states
the rule; `no-mandatory-telemetry` in the cross-provider conformance matrix is
where it is enforced, and it is enforced for every provider column rather than
for one.

The one HTTP client the shipped binaries construct talks to `127.0.0.1`: it is
the container health check asking the local process whether it is healthy
(`apps/generic/cmd/backup-manager-web`). It never leaves the container.

The app does open outbound network connections, and it opens exactly the ones
the operator configured: SFTP sessions to the hosts named in the operator's own
backup sets, made by the pinned rclone packages compiled into
`/backup-manager`. Those connections carry the operator's own data to the
operator's own destination. No third party is in that path, and the app adds no
destination of its own.

## What the app stores, and where

Everything stays on the machine the app runs on, under paths the operator
chooses at install time. `distribution/packaging/canonical.json` declares them
per platform, and every acceptance procedure in `docs/acceptance/` walks
through creating them.

- **Backup set configuration**: source paths, destination hosts and paths,
  schedules and retention policy. Stored in the config file
  (`/etc/backup-manager/config.yaml` inside the container).
- **SSH private key and known-hosts file**: the credential material for the
  SFTP destinations. Mounted read-only, never copied elsewhere by the app, and
  never written to a log. The host-key policy is strict: an unknown or changed
  host key stops the transfer rather than accepting it.
- **Run state and history**: the state database records what ran, when, what it
  transferred and what it retained, so the app can answer "is this backup
  healthy" without re-reading the destination.
- **One local account**: a username and a password hash for the web UI, and
  session material for a signed-in browser. The password is stored hashed, not
  recoverable, and the hash never leaves the machine.

## Personal data

Backup Manager collects no personal data about the person using it. It has no
user profile, no contact field, no identifier that follows anyone between
installations, and no analytics identity.

Personal data can nonetheless pass through the app, because the operator may
choose to back up files that contain some. In that case the app is a transport
and a scheduler, not a processor with its own purpose: it copies bytes from a
source the operator named to a destination the operator named, and it retains
what the operator's retention policy says to retain. Deciding what data is in
scope, and for how long, stays with the operator.

The two categories of data the app itself creates about a person are the local
account described above and the operator-visible run history, and both live
only in the paths listed above.

## Logs

Logs record what the app did: which set ran, how many files moved, what
succeeded and what failed. Secrets are redacted rather than written; that
redaction has its own regression suite, because "we do not log secrets" is a
claim that decays silently.

## Deleting everything

Removing the app and deleting the state, config and secrets directories the
platform's acceptance procedure created removes everything Backup Manager
stored. There is nothing held anywhere else, so there is no deletion request to
make of anybody and nobody to make it to.

## Changes

This file is versioned with the source and travels in the release provenance
bundle (`provenance/release-provenance.json`), so the wording a given
release shipped is recoverable by checking out that release's commit.
