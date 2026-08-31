# Hardware acceptance: Synology DSM package install, update, uninstall

Work package B4.4 (`docs/EPIC-B-multi-nas.md` §72) wraps the exact
provider-neutral release binaries in a DSM `.spk`. Everything about how
that package is assembled is covered by automated tests
(`apps/synology/spk`). What no test in this repository can decide is
whether DSM's Package Center accepts it, whether the desktop launcher
opens the shared Web UI, and whether an in-place update and an uninstall
behave the way §72's acceptance criteria require.

§68's Provider Test Matrix says provider acceptance procedures "SHALL be
written/version-controlled before manual execution". This is that
procedure, written before the package existed, and it is the only thing
that can close the four hardware-gated acceptance criteria on issue #85.

Until it has been executed and its results recorded, every claimed
architecture below is **build-supported but uncertified**, in §68's own
words.

## Architectures claimed, and how many machines that needs

The package ships two artifacts, because the canonical release ships two
core binaries:

| SPK artifact | INFO `arch` | Go target | DSM platforms covered |
|---|---|---|---|
| `BackupManager-x86_64-<version>.spk` | `x86_64` | `linux/amd64` | apollolake, avoton, braswell, broadwell, broadwellnk, broadwellntb, broadwellntbap, bromolow, cedarview, coffeelake, denverton, geminilake, grantley, kvmx64, purley, skylaked, v1000 |
| `BackupManager-armv8-<version>.spk` | `armv8` | `linux/arm64` | rtd1296, armada37xx, rtd1619, rtd1619b |

The `arch` family names and their member platforms come from Synology's
own Appendix A platform/arch mapping table, not from inspection of a
device.

§68 requires "a representative DSM 7.x amd64 and/or arm64 model for each
architecture claimed", so executing this procedure in full needs **two
machines**: one x86_64 DSM 7.x model and one armv8 DSM 7.x model. Running
it on only one of them certifies only that one; the other stays
uncertified and must be described that way.

Explicitly **not** claimed, and not installable: `i686` (evansport),
`armv7` (alpine, alpine4k) and `armv5` (628x). The canonical release
builds no 32-bit binary, so there is nothing honest to package for them.
DSM will refuse to install an SPK whose `arch` does not match the unit,
which is the correct outcome rather than something to work around.

## What is already proven without hardware

Nothing below re-tests any of this. It is listed so the hardware run stays
narrow, and so nobody executes it expecting it to prove more than it does.

- The SPK's outer layout matches the toolkit's documented structure, and
  is an uncompressed tar exactly as `pkg_make_spk` produces
  (`apps/synology/spk/verify_test.go`).
- The packer and the verifier agree about binary-hash parity (§3.7): a
  package built from a set of binaries verifies against a manifest of
  those same binaries, and a one-byte rebuild, a wrong-architecture entry
  and a post-build tamper are each caught (`TestVerify_BinaryHashParity`,
  four controls against a passing baseline).
  **This is proven against synthetic fixtures only.** No test anywhere
  runs the parity check against a real release artifact: nothing
  cross-compiles the release binaries or builds a `.spk` in CI, which is
  blocked on #174. Step 1.6 below is where a real package's binaries are
  compared against `container/release-manifest.json`, by hand, and it is
  the only place that comparison happens at all.
- The INFO file carries every field Synology documents as necessary, and
  its `arch` is one of the two claimed families.
- No file anywhere in the package looks like a credential
  (`TestVerify_RejectsBundledSecret`, with a detector control).
- Nothing Synology-specific exists in `core/` or `ui/shared/`
  (`scripts/architecture/*.sh`, run by CI and by the pre-commit hook).

## What this procedure decides

Four questions, one per hardware-gated acceptance criterion:

1. Does Package Center install the manually-uploaded `.spk` on a real
   DSM 7.x unit of the claimed architecture?
2. Does the DSM desktop launcher open the shared Web UI, authenticated
   through the reusable `local-auth` (not DSM SSO, which is deliberately
   out of scope for this work package)?
3. Does application state survive an in-place package update?
4. Does uninstalling leave retained backup data alone?

## Preconditions

1. A DSM 7.x unit you are allowed to break. Never a production NAS, and
   never one holding backups whose loss would matter. This procedure
   deliberately includes an uninstall.
2. DSM at or above `7.0-40314`. That is the package's own `os_min_ver`,
   and it is set there because the package keeps its state in
   `/var/packages/<pkg>/var`, which Synology documents as available from
   that build.
3. **Package Center → Settings → Trust Level set to "Any publisher"**, or
   the manual install will be refused. The package is unsigned: signing
   requires a Synology-issued key that this project does not hold.
   Record the trust level you used.
4. Two `.spk` builds of the SAME release, one per architecture, produced
   by `apps/synology/cmd/spkctl build` from the release binaries recorded
   in `container/release-manifest.json`. Do not build them from a working
   tree that differs from the release commit; the whole point of §3.7 is
   that these carry the release digest.
5. The output of `apps/synology/cmd/spkctl verify` for each `.spk`,
   printed and kept, so the hardware run starts from a package already
   known to carry the right binary hashes.
6. An SSH login to the NAS as an administrator, for reading logs and for
   the filesystem checks in steps 4 and 5. Everything else is done
   through the DSM web interface, the way a real administrator would.
7. A second, non-administrator DSM account, used once in step 3 to check
   the launcher's visibility.

Record, for every step: DSM version and build, model, architecture,
package version, wall-clock time, and the exact text of any error DSM
shows.

## Procedure

Execute steps 1 to 6 in order on each claimed architecture separately.
A result on one architecture says nothing about the other.

### 1. Manual install through Package Center

1. Package Center → Manual Install → upload the `.spk` for this unit's
   architecture.
2. Expect: DSM accepts the file, shows the package name, version and
   maintainer read out of INFO, and installs without a wizard.
3. Record whether Package Center offered a volume choice and which volume
   you picked.
4. Expect: the package appears as Installed, and Running or Stopped.
5. Confirm over SSH that the payload landed where the package framework
   says it should:
   ```sh
   ls -l /var/packages/BackupManager/target/bin/
   ```
   Expect `backup-manager` and `backup-manager-web`, both executable.
6. Confirm the packaged binaries are byte-identical to the release ones:
   ```sh
   sha256sum /var/packages/BackupManager/target/bin/backup-manager \
             /var/packages/BackupManager/target/bin/backup-manager-web
   ```
   Compare against `container/release-manifest.json` for this
   architecture. This is acceptance criterion "SPK contains the exact
   release core binary hash" checked on the installed system rather than
   in the build, and it is the one step that would catch a package
   assembled from a rebuild.
7. Record who owns the directories the package just created.
   `conf/privilege` declares `run-as: package` and the package contains
   no `chown` anywhere, so this is the one assumption nothing in the
   repository can check for itself:
   ```sh
   ls -ln /var/packages/BackupManager/var \
          /var/packages/BackupManager/var/state \
          /var/packages/BackupManager/var/log \
          /var/packages/BackupManager/var/run
   grep "Created package directories as uid" /var/log/packages/BackupManager.log
   ```
   Record the owning uid and the mode of each. Whether that uid is the
   one the daemons run as is settled in step 2.7, and the two answers
   together decide whether `postinst` needs a `chown`.
8. Now upload the OTHER architecture's `.spk` to this same unit.
   Expect: Package Center refuses it, naming an architecture mismatch.
   This is the positive control for step 1: an install path that accepts
   anything is not evidence that it accepted the right thing. Record the
   exact refusal text.

**Failure to record, not work around:** if install fails on a
`conf/resource` worker, capture `/var/log/packages/BackupManager.log` and
the DSM error verbatim before changing anything. That log names the
worker, and it is the difference between "the resource spec is wrong" and
"this model cannot host the package at all".

### 2. First start and local-auth enrollment

1. Start the package from Package Center if it is not already running.
2. Expect: it does NOT start cleanly yet, and Package Center says so.
   The seeded configuration has no backup sources, and the core refuses
   to run on a configuration it cannot validate. This is the shipped
   behavior of the generic Web host too, not something Synology-specific,
   and it is deliberately part of this procedure so nobody records it
   later as a Synology bug.
3. Over SSH, edit `/var/packages/BackupManager/etc/config.yaml`: set
   `state.database` (already seeded), and add one real source and backup
   set pointing at the shared folder DSM created for the package.
   Confirm that shared folder exists:
   ```sh
   ls -ld /volume*/backup-manager
   ```
4. Start the package again.
5. Expect: Package Center shows Running.
6. Read the one-time enrollment notice:
   ```sh
   cat /var/packages/BackupManager/var/log/engine.log
   ```
   Expect a bootstrap token, and expect it to be a token only, never a
   password. Confirm the log contains no credential, no key material and
   no path to a private key printed in full.
7. Confirm the engine is NOT reachable from the LAN. From another machine:
   ```sh
   curl -sS --max-time 5 http://<nas>:8478/api/v1/system/capabilities
   ```
   Expect: refused or timed out. The engine binds loopback only; only the
   UI host has a LAN-facing port. If this returns JSON, stop: that is a
   security finding, not a step to pass.
8. Record the uid the daemons actually run as, and whether they could
   write at all:
   ```sh
   ps -eo user,pid,args | grep backup-manager-web
   ls -ln /var/packages/BackupManager/var/log/engine.log \
          /var/packages/BackupManager/var/run/engine.pid
   ```
   Expect: both files exist and are owned by the uid in the `ps` output.
   If the engine "exited immediately" and `engine.log` does not exist,
   the cause is the directory ownership from step 1.7 and not the
   unconfigured sources list `start-stop-status` names in its message.
   Record which it was, because that message points at the wrong cause.
9. Power-cycle the unit, then press Stop in Package Center.
   ```sh
   sudo reboot
   # after it comes back, before touching anything else:
   cat /var/packages/BackupManager/var/run/engine.pid
   ps -eo pid,args | grep backup-manager-web
   ```
   `var/` survives a reboot, so the pid file that comes back names the
   pid space that existed before it. Expect: Package Center shows the
   right state, and Stop neither hangs nor reports success against a
   package that is not running. This is the only step that exercises a
   stale pid file, and the pid it names after a reboot may well belong to
   an unrelated DSM process, which is what makes it worth doing on
   hardware rather than reasoning about.

### 3. DSM desktop launcher opens the shared Web UI

Run this step **twice**: once from a plain-HTTP DSM session (port 5000)
and once from an HTTPS one (port 5001, which is DSM 7's own hardened
default), and record both. The launcher navigates to `http://<nas>:8477/`
unconditionally, because nothing in this package terminates TLS, so from
an HTTPS session it is a top-level https-to-http navigation. Which scheme
the tester reached DSM with is otherwise invisible in the result, and
this criterion is the one most likely to differ between the two.

1. Log in to DSM as the administrator. Open the Main Menu.
2. Expect: a "Backup Manager" entry with the package icon.
3. Click it.
4. Expect: it opens the shared Web UI, served by the package's own UI
   host on port 8477, showing the local-auth login or enrollment screen.
5. Complete enrollment with the token from step 2, choose an
   administrator password, and log in.
6. Expect: the shared UI loads and `GET /api/v1/system/capabilities`
   succeeds. Record which provider bridge the UI reports. It will report
   the generic bridge, not the Synology one; see the "Known gap" note in
   `apps/synology/README.md`. That is expected here and is not a failure
   of this step.
7. Also open Package Center → Backup Manager → Open, and confirm it
   reaches the same UI. Two documented routes exist (`dsmuidir` plus a
   `.url` desktop entry, and INFO's `adminport`/`adminurl`); record which
   ones actually worked, because that decides which one the package keeps.
8. Log out of DSM, log in as the non-administrator account from the
   preconditions, and open the Main Menu.
9. Record whether the launcher is visible to that account. `allUsers` is
   set true in the launcher config, so it should be; whether that account
   can then get past local-auth is a separate question and the answer must
   be no, because it has no local-auth credential of its own. If a
   non-administrator reaches an authenticated UI, stop: that is a security
   finding.

### 4. In-place update, state must persist

1. Before updating, capture the state that has to survive, into files you
   can hold the upgrade against afterwards rather than into your memory:
   ```sh
   sha256sum /var/packages/BackupManager/var/state/backup-manager.db \
             /var/packages/BackupManager/etc/config.yaml \
     | tee /tmp/before-upgrade.sha256
   find /var/packages/BackupManager/var/state -type f | sort > /tmp/before-upgrade.txt
   ```
   and, in the UI, note the logged-in session, the configured backup set,
   and at least one artifact row.
2. Build a second `.spk` of the same package at a higher `version`.
   Change something observable and harmless (the version string is
   enough); do NOT change the core binaries, so this step tests the
   package framework's upgrade path and not a core change at the same
   time.
3. Package Center → Manual Install → upload the newer `.spk` over the
   installed one.
4. Expect: DSM recognises it as an upgrade of the existing package rather
   than a conflicting second install.
5. After the upgrade completes, compare against the baseline rather than
   re-reading it by eye:
   ```sh
   sha256sum -c /tmp/before-upgrade.sha256
   find /var/packages/BackupManager/var/state -type f | sort > /tmp/after-upgrade.txt
   diff /tmp/before-upgrade.txt /tmp/after-upgrade.txt
   ```
6. Expect: every line of `sha256sum -c` says OK, the diff is empty, the
   local-auth record still exists, and the enrolled administrator can
   still log in without re-enrolling. `target/` is documented to be
   replaced on upgrade and `var/`+`etc/` to persist; this step is what
   proves the package actually put its state on the right side of that
   line.
7. Expect: the UI, after the upgrade, still shows the same backup set and
   the same artifact row.
8. Confirm the new binaries are again byte-identical to the release
   manifest for this architecture (repeat step 1.6). An upgrade that
   quietly swaps in a differently-built binary fails §3.7 just as badly as
   a bad first install.

### 5. Uninstall must not delete retained backup data

This is the destructive-safety step. Read it fully before starting.

1. Put real, identifiable data in the backup share, outside the package's
   own footprint:
   ```sh
   mkdir -p /volume1/backup-manager/acceptance
   dd if=/dev/urandom of=/volume1/backup-manager/acceptance/canary.bin bs=1M count=8
   sha256sum /volume1/backup-manager/acceptance/canary.bin | tee /tmp/canary.sha256
   find /volume1/backup-manager -type f | sort > /tmp/before-uninstall.txt
   ```
2. Also record what exists outside the share that must survive:
   ```sh
   ls -ld /volume*/ /volume1/@appstore/BackupManager \
          /var/packages/BackupManager/etc /var/packages/BackupManager/var
   ```
3. Uninstall the package through Package Center.
4. Expect: uninstall completes.
5. Now check the canary FIRST, before anything else:
   ```sh
   sha256sum -c /tmp/canary.sha256
   find /volume1/backup-manager -type f | sort > /tmp/after-uninstall.txt
   diff /tmp/before-uninstall.txt /tmp/after-uninstall.txt
   ```
   Expect: the canary verifies, and the diff is empty. Synology documents
   that a `data-share` shared folder "will not be removed after package
   uninstallation, since it might delete the user's personal data as
   well", and the package's own `postuninst` deletes nothing at all. This
   step is what turns both of those from a claim into evidence.
   **Any deletion here is a release blocker, not a finding to triage.**
6. Record what DSM removed on its own:
   ```sh
   ls -ld /volume1/@appstore/BackupManager 2>&1
   ls -ld /var/packages/BackupManager 2>&1
   ls -l  /var/packages/BackupManager/var/state 2>&1
   ```
   `target` is documented to go; `etc` and `var` are documented to stay.
   Record what actually happened for each, because a reinstall in step 6
   inherits whatever survived.
7. Confirm the DSM desktop launcher entry is gone from the Main Menu and
   that no leftover entry 404s.

### 6. Reinstall after uninstall

1. Install the same `.spk` again.
2. Expect: it installs, and it does NOT overwrite the `config.yaml` that
   survived the uninstall. `postinst` seeds a config only when none is
   present.
3. Expect: the surviving local-auth record still works, so no second
   enrollment token is issued. If a new token is printed while an old
   administrator record still exists, record it: that is a real finding
   about the enrollment path, not a Synology one.
4. Uninstall again and confirm the canary from step 5 is still intact.

## Evidence to record

- Package Center screenshots for install, upgrade, uninstall and the
  architecture-mismatch refusal in step 1.7, each with the DSM clock
  visible.
- `/var/log/packages/BackupManager.log` for every lifecycle operation.
- `/var/packages/BackupManager/var/log/engine.log` and `ui.log`.
- The `ls -ln` output from steps 1.7 and 2.8, and the `ps` line showing
  the daemons' uid.
- Step 3 run from both an HTTP and an HTTPS DSM session, recorded
  separately.
- The `sha256sum` output from steps 1.6, 4.1, 4.5, 4.8 and 5.1/5.5.
- `diff /tmp/before-uninstall.txt /tmp/after-uninstall.txt`, empty.
- The `spkctl verify` output for the exact `.spk` files installed.
- The §68 record row: provider/OS version, model, architecture, package
  version, install result, auth result, storage result, update result,
  uninstall result, retained-backup safety, evidence location.

Store the evidence with the issue this procedure is executed for. Do not
store any credential, token, key or the enrollment token alongside it.

## Accept / reject

**Accept** an architecture when, on a representative DSM 7.x model of
that architecture: step 1 installed and the on-disk binaries matched the
release manifest, step 1.8 refused the wrong architecture, step 3 opened
the shared Web UI from the DSM desktop launcher and authenticated through
local-auth, step 4 preserved state and the manifest hashes across an
in-place update, and step 5 left the canary and the whole share list
untouched.

**Reject**, and do not describe that architecture as certified, if any of
those failed. Record which, because each one points somewhere different:
install failures point at INFO/`conf`, launcher failures at
`dsmuidir`/`adminport`, update failures at the `target` versus `var`
split, and any deletion in step 5 at `postuninst` or the `data-share`
spec.

A partial result certifies nothing. §68's "build-supported but
uncertified" is the correct description of an architecture whose run was
not completed, and it is not a softer form of accept.
