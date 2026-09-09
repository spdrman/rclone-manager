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
