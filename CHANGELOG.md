# Changelog

## [Unreleased]

### Added

- **A backup set can name a subtree discovery must not walk into** (#737).
  `exclude_paths` on a backup set lists directories, relative to
  `remote_path`, that the listing skips: "recurse into `uploads/`, never into
  `uploads/tiles/`". It is a path list and FR-5's `include` is a basename
  pattern list, deliberately two fields — an include pattern is matched
  against a candidate's basename wherever it turned up, so it can say nothing
  about *where* to look, and teaching it to would change what every pattern
  already written means.

  The point is the cost, not the result set. Discovery's listing is fully
  recursive by design, so a set pointed at a directory an application also
  caches under walks the cache too — 65k files across 1.6k subdirectories in
  the deployment that reported this, which against a remote with no native
  recursive listing is 1.6k round trips per poll for artifacts nobody wants,
  and a pass that did not finish. An entry becomes an rclone directory
  filter, so the walk declines to descend rather than walking and discarding:
  the excluded directories are never listed at all. Each entry is a literal
  relative path, root-anchored under `remote_path` (an optional leading or
  trailing `/` is tolerated and ignored, and does not make the value
  filesystem-absolute); a traversal segment, a backslash, surrounding
  whitespace or a glob metacharacter is refused rather than half-honoured — a
  `tiles ` would become a filter matching nothing and quietly resume the full
  walk. Writing none of them is every configuration that exists today,
  unchanged.
- **The activity feed is read a page at a time** (#730). `GET /api/v1/activity`
  takes a `before` cursor and answers with a `next_cursor`, so the Activity
  page asks for a bounded first page and offers "Load older events" instead of
  loading a deployment's entire lifecycle record on open. The record is
  append-only and nothing prunes it, so the previous shape got slower every
  week a deployment stayed up, and clamping alone would have left everything
  older than the newest thousand events unreachable. The cursor is a row
  position echoed straight back from `next_cursor` — described that way rather
  than as "opaque", because a token a client is told to hand back unchanged is
  the honest description of what it is — it pages on the journal's own ordering
  rather than on an offset, so a transition recorded between two requests
  cannot make a page repeat itself, and any value the feed could not have
  issued (unparseable, negative, zero, too large to name a row) is ignored and
  answered with the newest page rather than refused, the same way an
  unparseable `limit` already was. The filters above the list apply to every
  page loaded, not only the newest one.

- **`LOG_LEVEL` is the diagnostics switch, and it reaches every process**
  (#730). It was already read by the web host's own surfaces while the engine
  built its log sink at a hard-coded `info`, so an operator who set it got the
  reverse-proxy trace and nothing from the process the trace describes.
  `core/service.Open` and the CLI's own sink now take the level from the
  environment (`obs.LevelFromEnv`), and `container/compose.yaml` passes
  `LOG_LEVEL` to BOTH services — with `container/.env.example`, the provider
  adapters and the Portainer template carrying it too — because the two halves
  of one request are recorded in two containers and one of them at `debug`
  gives half of every story. `RM_DEBUG=1` stays as the shortcut, and
  `docs/deployment.md`'s new "Turning on diagnostics" covers all three
  switches, the browser's `?debug=1`/`?debug=0` included.

- **The proxy reports what the body turned out to be, not only what it
  declared** (#730). At `debug`, `web-ui` wraps the upstream body in a
  transparent observer and emits `proxy_upstream_complete` once per request:
  bytes actually copied against the `Content-Length` declared, whether the read
  ended at EOF, and any read or close error, at `warn` when the two disagree.
  This is the shape the reported fault takes from JavaScript — a well-formed
  status line, a body that stops early, a browser that refuses the response
  before `fetch` sees any of it — and until now nothing in the container
  recorded that the transfer came apart. The header-phase line is renamed
  `proxy_upstream_headers`, since that is what it is.

- **Every response carries a correlation id, and the browser names its own
  attempt** (#730). The id is minted in one middleware at the web host's edge,
  before authentication and routing, and set on every response rather than only
  on refusals: a 200 whose body the browser could not read used to carry none,
  so "the browser could not read this" and "here is what was sent" were two
  records with nothing in common. `serve-ui` forwards it to the engine, which
  adopts a valid inbound id instead of minting a second, so the two containers'
  lines join. And because a request that gets no response carries nothing back,
  the browser now sends `X-Client-Attempt-Id` and logs it in the `[rm-debug]`
  line, which the server writes down (bounded and validated) — so a console
  screenshot of a failed `fetch` can still be matched to the server's record of
  the same request.

### Fixed

- **`scripts/api/check-client-paths.sh` runs again** (#730). The diagnostics
  commit on this branch changed `ui/shared/src/api/client.ts` to fetch through
  a local `const url = BASE + path`, and that gate refuses to trust the paths
  it reduced unless the client's single `fetch` literally reads `fetch(BASE +
  path` — a URL built any other way is one the gate never saw. It had been
  failing for every path in the file, which is a CI step red for reasons that
  have nothing to do with the change being checked.

- **A deployment that asked for no diagnostics pays for none** (#730). The
  activity feed's debug line reports the bytes a counting response writer
  actually wrote, instead of marshalling the whole payload a second time to
  guess the size; the browser client resolves the toggle once per request and
  builds its detail objects through a thunk, so nothing is constructed on the
  path every API call in the bundle takes; and the reverse proxy no longer
  allocates a per-request context value of its own for the clock, riding along
  in the one the correlation id already needs — which is also what keeps
  `proxy_error` reporting how long the browser waited on a default `info`
  deployment, where the failure it describes actually happens.
- **A destination can be a directory on another machine, reached over SSH**
  (#731). `sftp` is a registered destination backend now, with its own bundled
  manifest: a host, a port that defaults to 22, a user, a `known_hosts` file,
  the directory to write into, an optional subdirectory, and an SSH private
  key stored the way every other credential is. `known_hosts` is required, not
  optional, for the reason the source side already refuses to connect without
  one: unset, an SSH client accepts any key from any server that answers.
  This cost no new dependency and no binary growth — the sftp backend has been
  linked since FR-4, because a backup source is read over it — so what it cost
  instead is written down in `docs/adr/0005-destination-backend-decision.md`:
  a destination backend is its own decision, taken separately from the source
  backend that shares its implementation, and a fourth one fails a named test.
  Declaring one in `config.yaml` or saving one from the wizard is not wired
  yet, and every layer says so by name rather than dropping a field quietly.

## [0.4.0] - 2026-09-09

### Added

- **A destination is an instance of a registered backend** (#664, #665). What
  a backend is — its identity, the fields an instance of it declares, each
  field's rules, and the probe steps that prove one — is written in a manifest
  the engine reads, rather than in a schema per backend spread through Go.
  `core/internal/backend` is that registry, standard library only, and exactly
  two manifests are bundled: `core/internal/backend/bundled/local_volume.json`
  and `s3.json`. A malformed manifest is refused at load rather than skipped, a
  manifest naming an rclone backend this build cannot dial is refused, and none
  of them may claim the reserved id `local`. Field values are judged by shape
  and a refusal never echoes what was typed into a field, so a credential
  pasted into the wrong box cannot come back out in a message.

- **S3 is described by its manifest rather than by `config.Validate`** (#667).
  A bucket's shape, the closed sets for storage class and upload verification
  and the four rules on a key prefix belong to the manifest now, and the legal
  set of destination types is whatever the registry can express instead of a
  list written out in Go. A `config.yaml` frozen from before any registry code
  existed still loads, validates and re-marshals byte for byte, which is the
  test that would have caught this delegation drifting from the schema it
  replaced.

- **A directory on a second disk can be a destination of its own, and there
  can be more than one** (#666). A `local_volume` destination takes a `path`,
  and the local probe gains a fourth check beside reach, deliverable and
  space: `distinct_volume` compares device ids and refuses two declared local
  destinations that turn out to share a filesystem, naming the one it collides
  with — two names for one disk is two copies that are one copy.

- **`local` is a declared destination, seeded at first run** (#666, #670). The
  local hard drive was an implicit special case every layer had to remember.
  A fresh install now writes it out as instance zero of the `local_volume`
  backend — `id: local`, with the backup root it just established as its path
  — after probing that root, so it is verified without anybody pressing
  anything. Its undeletability stopped being a rule of its own: what refuses a
  removal is that the destination is in use, or holds the default, or is the
  last one left, exactly as for any other. Hand the default to something else
  and the seeded entry becomes an ordinary, removable destination.

- **`GET /api/v1/backends`** (#664, #668). The catalogue an operator chooses
  from, served read-only, carrying the two naming rules with it — the
  instance-id pattern and the reserved id — for the reason
  `tier_name_pattern` already established: a form has to refuse exactly what a
  hand-edited configuration file would be refused for, and a client holding
  its own copy of the rule goes stale in one direction only, silently
  accepting a name the engine then rejects. Storage shapes this build can dial
  that no manifest declares are served as `unregistered` — today that is
  `sftp` — which is a subtraction from what the engine requires rather than a
  second list somebody has to remember to update.

- **The add-a-destination wizard: choose a backend, then name the instance**
  (#668). Two steps, deliberately kept apart even though this release ships
  two backends and it therefore looks like ceremony. Several instances of one
  backend is the normal case — two local volumes, a hot bucket and a cold one
  — and a screen that collapses "pick S3" and "call it `cold_archive`" into
  one act has quietly re-asserted that a destination *is* a backend type. It
  also puts the uniqueness rule where it applies: step 2 lists the instances
  that already exist on the chosen backend, so an operator sees why a name is
  taken instead of meeting a validation error about it afterwards. There is no
  list of backend names in the component and no branch on which one was
  chosen; the rows come from the registry, and nothing is written by any of
  the three panes.

- **The configure-a-destination wizard is a renderer for a manifest, not a
  form per backend** (#669). One renderer per declared field kind — string,
  path, url, enum, bool, credential, key_prefix, and no eighth — with the
  per-kind rules transcribed from the engine's own validator, the probe drawn
  as one step list in three states, and a review pane. Which fields appear, in
  what order, which are required and what an unset optional one resolves to
  are the manifest's answers; local volume renders a directory and a
  subdirectory, S3 renders six fields and a credential pair, and neither list
  appears in the code. Nothing is written until the probe passes, and what is
  written is what was proven: the proof is keyed to the values it ran against,
  so editing a field afterwards withdraws it rather than carrying it over. A
  credential is written to the credential store and never read back, and the
  review pane says so where the value would otherwise be.

- **`install --cli-only`** (#689). A deployment with the scheduler and the
  command line and no browser, instead of one shape and an operator who wanted
  the other writing their own Compose file and owning it forever. The engine
  runs as `/rbm daemon`, no `web-ui` service is started, and no port is
  published, so nothing in the deployment serves HTTP. The interface is a
  wrapper at `<prefix>/bin/rbm` over `docker compose run --rm --no-deps`, not
  `exec`, because the first command a fresh host needs runs before anything is
  started. Such an install stages everything, starts nothing, exits 0 and
  prints the command that writes the first configuration — a daemon with no
  `config.yaml` is refused rather than started, so starting it would only
  produce a crash loop and an installer that claimed success over it. The
  shape is recorded in `.env` and adopted on a bare re-run, because an upgrade
  is not the place to start publishing a Web UI on the LAN of a host somebody
  deliberately installed without one; `--no-cli-only` converts it back and
  says so out loud.

- **`install_docker_host.py enroll-link` mints a fresh enrolment link and
  prints it** (#714). The link an install prints lasts thirty minutes and
  works once, and nothing reissues it while the engine keeps running, so the
  documented way back from a lapsed one was raw Docker commands. The
  subcommand restarts the engine, which is what mints a token, waits for the
  new notice, and prints the **last** one in the log — the last, because a
  container keeps its log across a restart and the dead notice is the one
  above, which is how somebody pastes an invalidated link into a browser and
  reads a refusal that does not explain itself. It says in as many words that
  every link printed before it, including any still in the scrollback, is now
  dead.

- **A licence that names this product and its copyright holder, and a
  contributor agreement** (#685). `LICENSE`'s appendix, `NOTICE` and
  `compliance.json` all said *Backup Manager*, in the one file a redistributor
  reads to find out what they have; the product is rclone-manager and the
  holder is Roman Goldmann. Both artifacts are regenerated from
  `compliance.json` rather than hand-edited, and the Apache-2.0 body itself is
  untouched — only the appendix's copyright line was ever ours to write.
  `CONTRIBUTOR-LICENSE-AGREEMENT.md` did not exist at all; it is adapted from
  the Apache ICLA and signed once, by adding `contributors/<username>.md` in a
  first pull request rather than on every one, with one clause written for
  this project: a contribution quietly carrying somebody else's code makes the
  generated licence inventory and the NOTICE false, and those are exactly the
  documents a recipient relies on. `CONTRIBUTING.md` covers the rest.

- **The site is published, and it covers the rest of the product** (#673).
  `docs/site` had been in the tree with nowhere to be read; it is deployed to
  Pages by a workflow of its own, because the branch setting can only publish
  the repository root or `/docs` and moving the site there would mix the pages
  written for people using the product in with the acceptance procedures and
  EPIC documents written for people working on it. `web-ui.html` and
  `ssh.html` are new, `index.html` and `first-run.html` are rewritten, and ten
  of the captures are moving pictures rather than stills, because most of what
  the last two epics added is a thing that happens rather than a thing that
  sits still. A clip is a list of frames with a hold time each, encoded
  through ffmpeg's concat demuxer, so the same script produces the same file on
  any machine and a diff of the output still means something. Every one of
  them was re-shot after the rename, so no capture on the site shows a command
  name this release does not have (#651).

- **A reference page that enumerates instead of walking** (#693). The site had
  a home page for the shape of the product and a tutorial for the one path a
  new install takes, and nothing that answered "what does this button do":
  three surfaces, three different non-answers. `docs/site/reference.html` lists
  every screen, control, command and installer flag, with what pressing one
  costs — the distinctions you cannot get by looking, like revalidate moving
  nothing where retry ingestion re-transfers, reinstate forfeiting the deletion
  of the remote original for good, or an emptied retention chain reinstating
  the default one rather than switching retention off. The command table is
  held to the binary's own dispatch table and the flag table to the installer's
  own parser, both by tests with positive controls, so a page that claims to
  list everything cannot quietly stop doing so.

- **`bash scripts/ci-docker.sh` runs the whole gate in a container, on a
  toolchain nothing can change under it** (#703). `scripts/ci-local.sh` is not
  modified — it is still the gate, its steps are still the steps and its
  verdict is still the verdict; what moves is what it runs on. Four red runs
  in one evening were none of them about anything in this repository: a
  Command Line Tools update landed mid-session and every cgo link on the
  machine started failing, in every repository. A red run has to mean the code
  is wrong, otherwise the reasonable response to one becomes "run it again",
  and that is the end of a gate as evidence. The container uses the host's
  Docker daemon through a mounted socket rather than starting one inside, so
  the two-machine proof creates exactly the sibling containers it always did;
  the repository is mounted at its own host path, or every path the gate hands
  to `docker run -v` names a directory that does not exist out there; and each
  JS workspace's `node_modules` is masked by a volume, because a macOS install
  of esbuild cannot execute in a linux container.

### Fixed

- **`backup-set create --no-verify` can write a first configuration again**
  (#670). Seeding the local hard drive as a declared destination at first boot
  proves the backup root first, which is right — but the probe was wired as a
  refusal rather than as a verdict, so it failed the whole first-run write even
  when the operator had explicitly asked not to verify. On a host whose backup
  root is not mounted yet, that made the first configuration unwritable by the
  one flag that exists for the situation: the same command printed
  &ldquo;`--no-verify` was given, so nothing was resolved&rdquo; for the SSH half and
  then refused for this one. The probe now runs either way and its answer is
  recorded either way — the seeded destination carries
  `connection_unverified: true` when it did not pass, which the destinations
  card already shows as **never proven** and which a passing test connection
  clears. Only the refusal is conditional: an install nobody opted out of still
  has to prove its backup root before anything is written.

- **`rbm medium remove local` explains itself with the reason it actually
  has** (#670). The CLI test still demanded the words &ldquo;cannot be
  un-declared&rdquo;, the local-specific refusal deleted when `local` became a
  declared destination; the engine had been answering the generic
  is-default refusal, correctly, and the assertion had outlived the
  behaviour. Retargeted, and strengthened to prove the refusal is about the
  mark rather than the name: move the default elsewhere and the reason goes
  away.

- **A completed local copy is no longer recorded as zero bytes** (#662). The
  durable commit measured the file it had just linked into place and then wrote
  the transfer's own reported numbers instead, so a local read-back that came
  back empty became a permanent, content-verified record of an empty backup
  over a file that was 294 bytes and perfectly good — in the journal placement
  and in the sidecar recovery manifest alike. Both are now written from the
  measurement, `verification_class: "content"` is only ever claimed over a
  digest of the bytes at the location, and a transfer report that disagrees
  with the committed file is reported as a fault on the transition an operator
  reads. The copy step no longer trusts the backend's byte count either: it
  measures the `.partial` it just wrote and refuses to record a copy shorter
  than the remote object the journal already recorded.

- **Reconciliation no longer condemns a durable copy it never opened** (#662).
  A disagreement between the recorded remote size and the recorded transfer
  size — two fields of the same journal row — quarantined an intact backup. It
  is now treated as a fault in the row: the file is checked against the remote
  identity captured at discovery, the one figure a bad local read-back cannot
  have written, and a refusal names the digest it measured. A copy that is
  genuinely wrong is still quarantined.

- **A settled record fault (#662) reached no operator, and its own content
  check was disabled forever** (#663). Reconciliation's fix for a
  self-contradictory row (recorded remote size disagreeing with recorded
  transfer size) rode a `noAction` finding that `rbm reconcile` never
  printed, and the branch it took stopped at the size question, so the
  content check FR-17 also runs never ran again on that row, on any later
  pass. A settled record fault is now flagged `NeedsInvestigation`, so it
  prints alongside the finding's reason, and `rbm reconcile`'s own summary
  line no longer says "no unresolved findings" over a run that just printed
  one. The row's durable local copy is also now hashed against the
  discovery-time remote digest (when one was recorded, and this build can
  compute it) rather than only re-checking its size, and the reason says so
  either way — content-verified, or explicitly not, never silently one or
  the other. Exit status is unchanged either way.

- **A FAILED backup has a way out** (#662). `retry` on an artifact whose own
  durable local copy occupies its final name used to loop forever against
  FR-12's collision refusal, and no other verb accepted the artifact. `rbm
  retry` now completes that ingestion in place: it compares the copy with the
  remote object, and on a byte-identical match records the measurement and
  returns the artifact to the durable state it held, through the same
  reinstatement path an operator's own `quarantine reinstate` takes (so the
  remote source is preserved from then on, permanently). `quarantine
  revalidate` and `quarantine reinstate` now also accept a FAILED artifact, so
  `retry` can no longer take away a recovery option and leave the artifact as
  stuck as before. The FR-12 collision refusal itself is unchanged.

- **A converged commit no longer certifies a damaged file, and no longer
  risks leaving an artifact without a recovery manifest to make that
  refusal** (#662, #663). Re-running `Commit` against an artifact already
  `COMMITTED` re-measures the file and rewrites the sidecar recovery
  manifest so a crash between the `COMMITTED` journal write and that write
  can never leave the manifest missing forever. When the file no longer
  matches the record, the manifest is now written from the record instead
  of from the disagreeing measurement — the record was itself measured
  from the file at commit time, so it is the trustworthy side of a
  convergence-time disagreement, not the file underneath it — and the
  disagreement is still reported as an error rather than being discarded.
  `measureCommitted`'s corrective re-hash also now refuses to pair a size
  from one read with a digest from a shorter one, instead of stamping
  `verification_class: "content"` on a hash that describes fewer bytes
  than the size beside it.
- **The browser offers a stuck backup a way out, on the backup it is actually
  stuck on** (#662). A backup's detail page had no control that reached any
  recovery verb, and the card added to answer that was gated on a combination
  no backend produces: a backup that failed an attempt carries no validation
  verdict at all, so the API reports it as `pending`, and a FAILED row is not
  quarantined either — so the card rendered for nothing real. It is gated on
  the lifecycle state now, which the API already reported and the client
  simply dropped. Pressing it re-enters the pipeline and the page re-reads the
  backup, because the verb answers before the pipeline has run: it says the
  request was accepted, never that the backup is recovered. A refused press
  carries the service's own words, what to do next, and the correlation id an
  operator can quote, instead of a bare error. Note that reaching this page at
  all still needs #677: the route matches one path segment and an artifact id
  has three.

- **The docked terminal no longer contradicts its own advice on screen**
  (#662). The suppression of a *"no rbm equivalent yet"* line printed under a
  remedy that names a command was applied to what Copy and Save produce and,
  separately, to what the panel draws — and only the first was ever exercised,
  so the window an operator reads could disagree with the file they exported
  from it. Both are now asserted against the rendered panel.

- **An artifact id can reach its own page** (#677). `App.tsx` declared
  `/backups/:artifactId`, one path segment, and a real artifact id is
  `source/set/name` — three. Nothing matched, the catch-all took it, and the
  operator landed on the Dashboard with no error, which left #662's own
  browser-side remedy — the Recovery card on the backup detail page —
  unreachable in a browser on every real deployment, even with #663 merged.
  The route takes three segments now and the page reassembles the id from
  them, exactly as `/sets/:source/:set` has done since #285, whose explanation
  was sitting four lines above the line that still had the bug. The URL is
  built by `artifactPath()`, escaping each part on its own, which is the half
  concatenation cannot do: a filename may contain a `#`, and a raw `#` in a
  path starts a fragment and takes the rest of the id out of the URL
  entirely. Nothing caught it because the fixture's artifact ids are
  slash-free, so every fixture-driven test navigated a URL a real id can never
  produce, and the detail page's own test declared the broken route itself.

- **A first configuration can be written on a host whose backup root does not
  exist yet** (#670). The seeded local destination's probe ran before the
  directory this deployment owns had been created, and `reach` correctly
  reported nothing there — so a create was refused although every connection
  step had just passed, and the three-container end-to-end rig could not stand
  a deployment up at all. The leaf is created first, with `Mkdir` and
  deliberately not `MkdirAll`: `reach` is the only check that catches a volume
  that never mounted, and building the whole chain would put it on the system
  disk and let every later check pass against an empty directory behind an
  empty mount point.

- **The enrolment link names the machine the deployment is on** (#688).
  `http://localhost:8080/enroll?token=…` is read on a different machine from
  the one it names: the install is run over SSH from a laptop, the browser is
  on the laptop, and `localhost` there is the laptop. The token is single use
  and dies in thirty minutes, so this was the first thing anybody did with a
  fresh install, failing. The hostname was not the fix and was already the
  default — it resolves on the box it names and, without mDNS or a record
  somebody set up, nowhere else. What works from anywhere on the LAN with
  nothing configured is an address, so the installer prints one: the local end
  of a socket connected to TEST-NET-1, which is the interface the default
  route would leave by, found without sending a packet. A loopback answer is
  treated as no answer, no default route falls back to the hostname rather
  than refusing, and `--public-base-url` still wins when it is given.

- **The add-a-destination wizard no longer prints a command that cannot be
  run** (#668). Its confirm step creates nothing — the single create happens
  in the configure step, once the probe passes — and it was echoing `medium
  add <id> --backend <backend>` at that moment anyway, advertising a write
  that does not happen with a flag the CLI does not take. The line is gone and
  its absence is stated where a reader looks for the command: a recorded gap
  with its reason beats a plausible line that fails on execution.

- **The configure wizard's echoed `medium add --type` names what the CLI
  wants** (#81). It passed the manifest's rclone backend where `--type` takes
  the registry key, so the one destination a fresh install has came out as
  `--type local`.

- **The installer stops telling operators that the version it installs cannot
  be proven** (#681). `preflight` and `install` both reported that 0.3.3 "is
  cut and not pushed" and that whatever the tag resolves to "goes in on the
  registry's word" — false since publication, from the one message whose whole
  job is to say whether the thing being installed can be proven, and the first
  thing a new operator meets. The carried pin and the recorded manifest are
  filled in together; one present and the other missing is exactly what the
  test beside them catches.

- **The reference page describes the product this release ships** (#664, #706,
  #707). Its four drift checks were green, which is the more interesting
  failure mode: EPIC I added no route and no command, so nothing structural
  was missing and everything substantive was. The Settings table still called
  `local` "the drive backups already land on and is always there", which is
  what a destinations list looked like when there could only ever be one of
  them; the list, the add flow and the manifest renderer now have sections of
  their own, with the costs of each control. The page's route map had also
  gone stale against #677's three-segment route, and two of its own
  enumerations against the sections added with it. Three more holes of the
  same shape closed with the install output (#714): the page said the
  installer has "six subcommands" and listed six, where `enroll-link` is the
  seventh — every drift check was green, because the flag table is held to the
  parser's options and the command table to the CLI's dispatch table and
  nothing had an opinion about subcommands, which is now checked in both
  directions *and* against the number in the heading; the exit-code table was
  missing `53`, under a sentence promising every refusal has its own code; and
  one in-site link pointed at a fragment that did not exist, which is now
  walked for every page.

- **The compliance documents name the application id this project declares**
  (#687). `privacy-policy.md`, `source-offer.md` and `support.md` said
  `com.iasbuilt.backupmanager` where `compliance.json` says
  `com.iasbuilt.rclonemanager`. The check that should have caught it only
  asked whether a phrase appeared somewhere in a document, and a stale id
  sitting beside nothing that contradicts it has nothing to disagree with; the
  new one reads every `com.iasbuilt.*` id a document names and refuses one
  that is not the declared id.

- **The web host introduces itself by the name everything else calls it**
  (#652). The image ships `/rbm-web`, every compose file runs it, the packaging
  manifest names it and the CLI's own exit-code table sends an operator to
  `rbm-web serve`, while the binary went on saying `backup-manager-web` in 37
  places, including `usage: backup-manager-web <command> [flags]`. So a fresh
  install answered `docker compose logs` with a name no compose file, no
  document and no other binary in the image still uses. The first-run
  enrolment notice — usually the first line a new deployment prints — had
  drifted further: it claimed the CLI's name for a line the CLI has never
  printed.

### Changed

- `rbm status` names the artifacts that need intervention and the commands
  that act on them, instead of only reporting that intervention is needed
  (#662).
- `rbm fetch --dry-run` marks an already-known object whose artifact a cycle
  will not attempt again, and says how many there are, instead of printing an
  empty plan that read exactly like a settled backup set (#662).
- `rbm retry` opens a transport, because completing an ingestion in place has
  to ask the remote for its hash.
- The Web UI's backup detail page offers a FAILED backup a **Recovery** control
  wired to the retry verb; the docked terminal no longer prints "no rbm
  equivalent yet" about an operation the same window has just handed the
  operator a command for (#662).
- The Web UI's storage destinations list can hand the **default** from one
  destination to another, and says what that costs before it does it (#671).
  Every destination now carries **Make default**; the one that holds the mark
  keeps the control and its **Remove**, both disabled, each pointing at the
  sentence that says why it is off and what would turn it back on — a control
  that is simply missing sends an operator hunting for a screen that does not
  exist. The confirmation names both halves of what one click does, because
  only one of them was asked for: the destination picked takes the mark and
  stops being removable, and the one that had it becomes removable. It also
  says what does not change — no tier is rewritten and no copy already
  written is moved or deleted. A destination nobody has proven cannot take
  the mark, and says so before the click rather than failing after it.
- Removing the local destination is now reachable from the browser once
  another destination holds the default (#670, #671). The list withheld
  **Remove** from any entry flagged `isLocal`, which stopped being a proxy
  for "not declared" when #670 made `local` a declared destination the engine
  removes like any other; the button now follows the engine's own rule, which
  is the default mark and nothing else.
- The machine-tier end-to-end case for #662 (`--case empty-record`) asserts the
  fix instead of the defect, and its own `--help` no longer tells operators it
  is red on purpose. It was written to fail and said so in four places,
  including the rendered help and the golden that is compared to it byte for
  byte; with #662 fixed, the first person to see it fail would have read that
  text and dismissed a regression as expected. It now requires the planted
  empty record to leave the copy at a durable restore point, the recorded fault
  to still reach an operator in words, and the file to be untouched. The
  dead-end walk it replaced is retained rather than deleted, and runs if
  reconciliation ever leaves the artifact outside a durable state.
- "Installing it" opens with the two commands that install the product, and
  both now carry the output they really produce, under an **Output** label so
  it cannot be mistaken for something to type (#683, #714). The enrolment link
  is highlighted in the block and annotated where it is: single use, thirty
  minutes, and an address that is this machine's own rather than `localhost`.
  Being honest about it took one extra step — the installer's success epilog
  does not carry the link at all, it prints the command that greps it out of
  the engine's log — so the page shows that command and its result rather than
  a line the installer never emits. The `--cli-only` block deliberately has no
  link, because there is no Web UI to enrol into, which is the whole point of
  the flag. None of those blocks is typed out any more: each one is rendered
  from the epilog the installer itself prints and compared line for line, and
  the enrolment sentence inside them is read out of the engine's own format
  string rather than a constant in a test, so a page still showing
  `http://localhost:8080/enroll?token=…` fails a check instead of waiting to
  be noticed. Three of the hand-typed blocks were already wrong when they
  landed — an elided Compose argv and an invented line continuation — and an
  abbreviation under an **Output** label is read character by character
  against a real screen, which is the whole reason the page shows output
  rather than prose.
- Sixty-odd shell scripts begin becoming one Python package, `scripts/rcmtools`
  (#672). The `perf` domain is ported in full and no longer needs `jq` to read
  a baseline record; the two entry points a gate test fabricates a stand-in for
  keep real shims at their old paths, and the one script that is sourced rather
  than executed is untouched, so there is no third copy of the host-id rule.
  The port closed a collision it found on the way: a malformed baseline record
  made `jq`'s exit status the script's own, which this repository elsewhere
  reserves for "this machine could not perform the proof" — a corrupt
  committed record is a real defect and has to fail the gate rather than be
  ledgered as incomplete.
- The product end-to-end gate stands up the three-container rig and drives it
  over `RM_BASE_URL`, instead of starting the UI workspace's own dev server
  against a mock API (#687). The tests-repo pin moves with it, read off that
  repository's merged `main` rather than a branch, and the eight browser cases
  that were required to fail on the route #677 fixed now run for real: 191
  passed, 69 skipped, nothing failed and nothing expected-failed.

### Removed

- `rclone_backend` is off the `/api/v1` contract (#81). EPIC I's backend
  catalogue put it on the wire twice, as a property of a manifest and as the
  whole of an unregistered entry, and the contract-drift gate was right to
  refuse it: what a public schema may not be made of is the implementation
  vocabulary a client codes against. The manifest no longer carries it, and an
  unregistered entry names its `transport`, which is a protocol and can
  therefore be spelled at product level at all. Prose is still allowed to tell
  the truth about rclone — a description may say what a thing is not — but a
  property may not be named for it. Internally the manifest keeps its own
  `RcloneBackend`, which is what the engine dials.
- `RemoveStorageMedium`'s local-specific refusal, and the browser's `isLocal`
  gate on **Remove** (#670, #671). Both said the local drive "was never
  declared", which stopped being true.
- The first-run page's opening note about the development mock, its
  "Re-shooting these" section, and the reference page's screenshot-provenance
  callout (#675, #678, #706). All three were true and written for the wrong
  audience — the first thing a reader met on a page they came to in order to
  look something up, or instructions for somebody with the repository checked
  out. The footer of every page still carries the provenance claim, the
  capture scripts keep their own header comments, and the notes that survive
  are the ones where believing the data is real would actually mislead: the
  dashboard that shows five backup sets where a real first run has one, the
  host fingerprint that is not yours, and the line saying no screenshot here
  is evidence about a running engine.
- `scripts/perf/capture-baseline.sh` (#672), replaced by `python3
  scripts/rcmtools/perf/capture_baseline.py`. Nothing fabricates a stand-in
  for it and no gate calls it — it is a by-hand driver on a dedicated
  benchmark host — and every caller and both documents now name the Python
  one.

### What you may need to do

Nothing is required, and nothing already written has to be changed. If a backup
set is already stuck in the state #662 describes — `rbm status` reporting
FAILING with a FAILED artifact whose local copy is intact — run `rbm retry
<artifact-id>`; it now resolves that state instead of looping. Note that an
artifact recovered this way keeps its remote source for good: this manager
never deletes the original of a backup it trusted on evidence rather than
re-fetched.

EPIC I asks nothing of a deployment that already exists. A `config.yaml`
written before this release never declared `local`, and it still loads and
resolves exactly as it always did: the local hard drive is synthesised on
every read rather than materialised, the same decision the effective default
destination and a tier's effective medium already make, so there is no
migration and no rewrite nobody asked for. Only a fresh install writes `local`
out as a declared destination, and the reserved id stays shut on every
operator-facing path — the API, the CLI and the wizard cannot manufacture it.

If you install with `--cli-only`, nothing is running when the installer
finishes, and that is what the flag means: use the `rbm` wrapper it stages at
`<prefix>/bin/rbm` to write the first configuration, and the deployment starts
serving from there. There is no Web UI on such a host and therefore no
enrolment link to open.
