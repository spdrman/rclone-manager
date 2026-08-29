# B1.2 — minimal UPK hardware proof: acceptance procedure

Issue #91, EPIC B tracker #81, Work Package 1.2 (docs/EPIC-B-multi-nas.md §69).

This is the acceptance procedure, written before the proof-of-concept it checks existed.
It is hardware-only work per the Phase 1 TDD gate: the four criteria below cannot be
automated in CI (they require a real UGOS desktop and a developer-authorized NAS), so this
document is the "test" that gets written RED first, then run for real against the built PoC.

Nothing in this file proves the PoC works by itself. Section 4 below records the actual
command output from actually running it on `HIGARA`, the real UGREEN NAS this was built and
tested against.

## Behavioral contract

GIVEN a developer-authorized UGREEN NAS and a Debian 12 development environment with a
pinned `ugcli`,
WHEN a minimal `.UPK` (a bootstrapped React/JSSDK frontend plus a bare backend exposing
`/health/live`) is built with `ugcli pack` and installed through the App Center
manual/developer install path,
THEN the app's icon appears in the installed-app list, it opens inside the UGOS desktop
using `open_type: inner`, the React/JSSDK bootstrap initializes and obtains a UGOS session
context, and a request to `/health/live` succeeds.

## What "session context" means for the real JSSDK

Before writing any frontend code, I pulled the actual `@ugreen-nas/core` package
(published on the public npm registry, `dist-tags.latest` was `1.7.19` when this was
written) and read its shipped `.d.ts` files rather than guessing the API. The relevant
pieces:

- `@ugreen-nas/core` default-exports a `UGOSOpenCore` singleton. Its `init(): Promise<string>`
  is the actual bootstrap call: it wires up the window's `CloudWindow` channel and resolves
  once the handshake with the host completes. `UGOSOpenCore.isHost` (a public getter) is
  `true` only when the page is actually running inside a UGOS host frame, `false` in a bare
  browser tab — this is the honest, otherwise-unfakeable signal that the JSSDK is talking to
  a real UGOS desktop and not just loaded standalone.
- `@ugreen-nas/core/cloudWindow` default-exports a `CloudWindow` singleton (a
  `CloudWindowAction` subclass). Its documented `ready()` action has this exact typed
  contract in the SDK's own source (`SpecialActionSpec['ready']`):
  `{ data: boolean, result: { ucVer: number, locale: string } }` — calling it is the
  session handshake, and the host's reply (`ucVer`, its own UI-core version; `locale`, the
  signed-in user's active locale) is the concrete "session context" this work package's
  acceptance criterion is asking for. `getSizeInfo()` is a second, independent live round
  trip (real window geometry only the host can answer) used below as a second proof that
  the channel is actually live, not just resolved with a cached/default value.
- `getThirdToken` (the actual per-request auth token exchange, `Ugreen-Ttk` header) is
  `useCapacity('getThirdToken')` on the same `CloudWindow` singleton. That is Work Package
  1.3 (#92), deliberately out of scope here: this proof only needs to show the JSSDK
  channel to the host is alive, not stand up authenticated API calls behind it.

## Procedure

Each step names whether it is automatable from outside the UGOS desktop (checkable by SSH,
`ugcli`, `docker`, or an HTTP probe) or requires an observation only a signed-in browser
session against the real UGOS desktop can make.

### 1. Icon appears in the installed-app list

- **Automatable, indirect**: the App Center install/uninstall history lives in
  `/ugreen/.config/.appstore/appstore_app.db` (table `app_opt_records`, one row per
  install/uninstall/upgrade with `app_id`, `opt_type`, `opt_time`, `opt_client`) and the
  installed app's own tree materializes at `/ugreen/@appstore/<app_id>/` once the App
  Center has actually installed it (every existing UGOS system app — `com.ugreen.desktop`,
  `com.ugreen.docker`'s tree at `/volume1/@appstore/com.ugreen.docker`, etc. — already
  follows this layout). A row for this app's `app_id` appearing in that table, and the
  directory appearing, is real, host-recorded evidence the App Center registered the
  package — check with:
  ```sh
  sqlite3 -readonly <snapshot-of-appstore_app.db> \
    "SELECT app_id, version, opt_type, opt_time, opt_client FROM app_opt_records WHERE app_id = '<app_id>';"
  ls -la /ugreen/@appstore/<app_id>/
  ```
  (Always query a `cp`-snapshot of the db, never the live file — it's root-owned,
  WAL-mode, and belongs to the running App Center daemon.)
- **Observation-only**: the icon actually rendering in the App Center's installed-app grid
  is a rendered-UI fact a screen only shows to a signed-in browser session. Over SSH this
  step can be evidenced but not directly seen.

### 2. App opens inside the UGOS desktop using `open_type: inner`

- **Automatable**: `open_type: inner` is a `project.yaml` field. `ugcli check` validates the
  manifest structurally; the committed manifest is also grep-checked for the literal value.
- **Observation-only**: the running app actually rendering as an inner desktop window (vs.
  a new browser tab, which is what `open_type: tab` would do) is a windowing fact only a
  signed-in browser session against the desktop can observe directly.

### 3. React/JSSDK bootstrap initializes and obtains a UGOS session context

- **Automatable, partial**: the frontend bundle can be built and type-checked against the
  real `@ugreen-nas/core` types (proving the calls in "What 'session context' means" above
  are real SDK calls, not invented ones), and it can be loaded standalone (outside a UGOS
  host) to prove it degrades honestly — `UGOSOpenCore.isHost` reads `false` and the app
  says so, rather than hanging or crashing — the same "missing host is a fallback, not a
  hang" posture `apps/ugos/frontend/auth.ts` already uses for the mocked bridge.
- **Requires the real host**: `isHost: true` plus a real `{ ucVer, locale }` reply from
  `ready()` can only come from the frontend actually running inside the real UGOS desktop
  frame — nothing on this NAS can be asked for that answer except the desktop itself. This
  is captured from the browser console / the app's own on-page debug panel once installed
  and opened, or from server-side evidence if the frontend logs it to its own backend.

### 4. A request to `/health/live` succeeds

- **Fully automatable**, and the one criterion checkable end-to-end without a browser:
  - the packaged container can be started directly with the exact image tar bundled into
    the `.UPK` (`docker load` the same tar `ugcli pack` bundled, `docker compose up` the
    same `rootfs_common/docker-compose.yaml` shipped in the package);
  - `curl 127.0.0.1:<project.yaml port>/health/live` from an SSH session on the NAS itself
    proves it's this host answering (loopback-only publication per
    docs/EPIC-B-multi-nas.md §22 means nothing else could reach it);
  - `docker inspect` / `docker logs` on the running container tie the response to this
    specific image digest, so it's provably the packaged app's own backend, not something
    else already listening.
  - See `apps/ugos/docs/verify-health.sh`, the automated half of this procedure.

## RED: this procedure fails before the PoC exists

Run before any of `apps/ugos/backend/`, `apps/ugos/frontend/upk-proof/`, or
`apps/ugos/upk-proof/` existed:

```
$ bash apps/ugos/docs/verify-health.sh rom@192.168.0.10 29090 com.spdrman.upkproofb12
==> looking for an installed-app record for com.spdrman.upkproofb12
FAIL: no /ugreen/@appstore/com.spdrman.upkproofb12 directory (app not installed)
==> curling http://127.0.0.1:29090/health/live on rom@192.168.0.10
FAIL: Warning: Permanently added '192.168.0.10' (ED25519) to the list of known hosts.
** WARNING: connection is not using a post-quantum key exchange algorithm.
** This session may be vulnerable to "store now, decrypt later" attacks.
** The server may need to be upgraded. See https://openssh.com/pq.html
curl: (7) Failed to connect to 127.0.0.1 port 29090 after 0 ms: Couldn't connect to server
RED: 2 check(s) failed.
```

That is the actual, real output from running the script against the real NAS at this point
in the work (exit code 1), captured before any backend, frontend, Docker image, or ugcli
project existed under `apps/ugos/`.

## Section 4: real run against the built PoC

Run on `HIGARA` after the backend, frontend, Docker image, and ugcli project all existed.
Full command transcripts are in the pull request description; this is the per-criterion
verdict.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| — | `ugcli check` | PASS | `✓ check passed` against the committed `project.yaml` + `rootfs_common/`. |
| — | `ugcli pack` produces a real `.upk` | PASS | `amd64_com.spdrman.upkproofb12_0.1.0.0001.upk`, 5,407,983 bytes, built and signed by the real pinned `ugcli` on the real NAS. |
| 4 | `/health/live` succeeds | PASS | `docker load` of the exact tar bundled into the package, `docker compose up` of the exact `docker-compose.yaml` shipped in `rootfs_common/`, then `curl 127.0.0.1:29090/health/live` from an SSH session on the NAS itself returned `{"status":"ok"}`. `docker inspect`'s image digest for the running container matched `docker images`'s digest for the tag exactly, confirming it's this specific packaged artifact answering. `GET /` returned this build's `index.html` (`<title>Backup Manager — UPK proof</title>`, referencing this build's hashed JS asset), so it's provably this app's frontend and backend, not something else already listening on that port. |
| 1 | Icon appears in the installed-app list | **BLOCKED** | Not achieved. See "What didn't work" below. |
| 2 | Opens inside UGOS desktop with `open_type: inner` | **BLOCKED** (config verified, behavior not observed) | `project.yaml` has `open_type: inner` and passed `ugcli check`; the actual windowing behavior needs criterion 1 first. |
| 3 | JSSDK obtains a UGOS session context | **BLOCKED** (code verified, live handshake not observed) | The frontend type-checks and builds against the real `@ugreen-nas/core` types and calls the real `UGOSCore.init()` / `CloudWindow.getSizeInfo()`; the actual host handshake (`isHost: true`, a real `{ucVer, locale}` reply) needs criterion 1 first. |

### What didn't work: the App Center install step needs a browser I don't have

Everything up to "install the package" is proven for real, on the real device. The literal
"App Center manual/developer install path" step — the one that would make criteria 1–3
observable — turned out to require a signed-in browser session against the UGOS desktop
web UI. I have SSH access only, no browser, and (correctly, per the task's own scope) no
admin web credentials. Concretely, I looked for a scriptable alternative before concluding
this:

- `ugcli --help` (and `create`/`pack`/`check --help`) has no `install`/`deploy` subcommand —
  confirmed, matches what the issue already flagged as likely.
- The App Center's state lives in `/ugreen/.config/.appstore/appstore_app.db` (SQLite,
  root-owned, WAL-mode — read only from a `cp` snapshot, never the live file) and installed
  apps materialize at `/ugreen/@appstore/<app_id>/` (root-owned, not writable by `rom`).
  Both are read-only-observable, not write-accessible, without root.
- There's a local Unix socket at `/run/ugreen.pub/ugreen_openapi.sock` (world read/write)
  that answers HTTP. I probed it read-only (`/openapi.json`, `/api/v1/apps`, `/apps`,
  `/version`, `/health`, and a few more — all `404`), which is consistent with it being the
  *reverse-proxy entry point installed apps' own APIs get exposed through* (matching
  `project.yaml`'s `proxy_path` field), not an app-management API. I did not attempt any
  write call against it or the app-manager's internal gRPC socket
  (`/tmp/.cache/ugreen.app.grpc.sock`): both are undocumented, root-owned-daemon-backed
  interfaces on a live shared personal device, and guessing at an install call against an
  unknown internal API is exactly the kind of hard-to-predict, possibly-hard-to-reverse
  action the task's ground rules say to stop and describe instead of attempting.

So: the `.upk` file is built, signed, and sitting on the NAS
(`~/upk-b1.2-proof/backup-manager-final/packaging/build_dir/pkgs/upk/amd64_com.spdrman.upkproofb12_0.1.0.0001.upk`),
ready for a one-time manual upload through App Center → (developer/manual install option) →
select that file. After that one click, `apps/ugos/docs/verify-health.sh` should show the
installed-app check passing too, and the frontend's on-page debug panel (see `App.tsx`)
would show the real `isHost: true` / `ucVer` / `locale` values instead of the timeout-driven
degraded state.
