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

### Removing a backup set

Four things an administrator will notice, all from the same change (issue #391), and
worth saying here rather than leaving to be found.

- "Remove set configuration" on a backup set's page now removes the set. It used to close
  its confirmation and do nothing, so a set an administrator reasonably believed was gone
  kept collecting. The removal takes the set out of the configuration and nothing else:
  every backup it collected stays on storage and stays listed under Backups, and creating
  a set with the same source and name again takes those backups back, along with their
  retention history. Nothing this operation can reach deletes a byte of backup data.
- The same operation is on the API (`DELETE /api/v1/backup-sets/{source}/{set}`) and on the
  command line (`rbm backup-set remove <source/backup-set>`). Against a server that
  is already running the command line uses that route; with nothing running it reaches the
  same code directly. See "The command line and a running server" below.
- The Backups list now includes the backups of sets whose configuration has been removed.
  Narrowing that list to one set is over configured sets only, so a removed set is refused
  there like an unknown name. The Quarantine screen lists quarantined backups under
  configured sets only, because every action it offers needs a configured set behind the
  backup; a removed set's quarantined backup stays on the Backups list, marked quarantined,
  and returns to the Quarantine screen once the set is configured again.
- A source with no backup sets under it is now a valid configuration, because removing a
  source's last set leaves the source in place, with its read-only posture. A hand-written
  configuration with an empty source used to be refused at startup and is now accepted.

### The command line and a running server

An administrator with a terminal on the machine can change the configuration while the server
is up, and the change takes effect at once rather than at the next restart. Creating, changing
and removing a backup set, and changing the deployment's retention or capacity settings, are
sent to the running server over the same API the web interface uses, with the same
administrator account, the same validation and the same audit trail, once the command has been
told where that server is. Nothing is written behind the server's back and there is nothing to
restart afterwards.

The command has to be told where the server is, and it is told through the environment.
`BACKUP_MANAGER_API_URL` is the server's address, which is `http://127.0.0.1:8080` from inside
its own container or the published web port from a shell on the machine, and
`BACKUP_MANAGER_API_USERNAME` and `BACKUP_MANAGER_API_PASSWORD` are the local administrator
account the web interface already uses. The password is held in memory for the one command and
written nowhere. These are environment variables rather than options because a password typed
as an option is visible in every process listing on the machine.

Each command that changes the configuration announces which world it found, on a `mode:` line.
`engine-attached` with an address means the change was handed to the running server.
`engine-attached` without one means the change was refused and nothing at all was written, so
there is no half-made change waiting for a restart. `direct` means nothing was running and the
file was changed here.

The commands that only read say the same kind of thing about their answers. `status`, the
source and backup set listing, the backup catalog and the retention preview report the running
server's world when they can reach it, say `unconfirmed` and answer from the configuration file
when they cannot, and refuse outright, printing nothing at all, when the running server turns
out to be holding a different configuration or to disagree about what it holds. That last one
is the failure this release exists to catch, and it is now caught before an administrator is
shown a number about a deployment nobody is running.

### Known limitations in this release

Listed because a store review reads them and an administrator deserves them before
installing, not after.

- Putting a backup set's contents back is a documented procedure rather than something
  this release performs for you; see the support materials for where it lives. There is a
  `restore` command, and it is a different thing worth not confusing with recovery: it asks
  a storage provider to make one archived copy readable again, which is what an administrator
  does before recovering from a copy that has gone cold, not the recovery itself.
- Native platform notifications are delivered only where the platform offers a local
  notification capability the app can adapt. On the targets in this release the
  administrator's path to an alert is the app's own dashboard.
- Native platform sign-on is not wired up on these targets; the app uses its own local
  account.
- Two configuration changes are never sent to a running server: replacing a backup set's whole
  retention policy, and writing the very first configuration on an installation that does not
  have one yet. Where a server is found serving that installation, both are refused with
  nothing written and the command says what it found, so there is nothing to restart into.
  Make the change in the web interface, where it takes effect at once, or stop the server, run
  the command, and start it again.
- Two of the command line's readings are answered from the configuration file on the machine
  it runs on and are not checked against a running server: the deployment's retention and
  capacity settings, and which retention policy one backup set is retained under. On an
  installation whose configuration file was hand-edited under a running server, those two
  answers can differ from what the web interface shows.
- A change sent to a running server is checked against the installation it was typed at
  before anything is sent, so an address with one character wrong, on a machine running two
  of them, is refused rather than landed in the other one. The one arrangement that check
  cannot see through is a whole state directory copied to seed a second installation: the
  copy carries the original's identity, and the two then claim to be each other.
- One engine per deployment. Starting a second `rbm daemon`, or a second web host,
  against a state database another one is already serving is refused rather than started. Two
  of them would run two schedules over one set of backups and hold two independent copies of
  one configuration, which is the divergence everything above exists to prevent.

### Upgrading

There is nothing to upgrade from. Future releases ship as a new package: this application
never replaces its own reviewed code, never pulls a floating tag, and has no self-update
path of any kind. See the privacy disclosure and the permission rationale for what that
means in practice.
