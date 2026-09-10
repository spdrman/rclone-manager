# Changelog

## [Unreleased]

### Fixed

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

### What you may need to do

Nothing is required, and no configuration or data format changed. If a backup
set is already stuck in the state #662 describes — `rbm status` reporting
FAILING with a FAILED artifact whose local copy is intact — run `rbm retry
<artifact-id>`; it now resolves that state instead of looping. Note that an
artifact recovered this way keeps its remote source for good: this manager
never deletes the original of a backup it trusted on evidence rather than
re-fetched.
