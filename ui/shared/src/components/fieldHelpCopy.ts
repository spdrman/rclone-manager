/**
 * Issue #278: what every explained input in this UI actually says.
 *
 * # Three parts, and the third is the point
 *
 * Every entry answers the same three questions in the same order: what the
 * field is for, an example of something an operator could type into it, and
 * what typing that would actually cause. The third is the reason this file
 * exists. "Remote path: the path on the remote host" restates the label and
 * helps nobody; "/var/backups on api-server, so every file the VPS drops
 * there is pulled to the NAS" says what happens next. The shape is a struct
 * rather than one string so a missing third part is a compile error instead
 * of a paragraph someone forgot to finish.
 *
 * # Every effect here is read out of the code, not out of the label
 *
 * The retention entries come from core/internal/config (config.go's
 * RetentionTier and Retention, and validate.go's rules) and FR-18/FR-19 in
 * docs/EPIC.md; the auth entries from apps/common/auth and the pages'
 * own handlers; the filter entries from the pages' own filtering code; the
 * wizard entries from core/service/backupsets.go (CreateBackupSet,
 * ImportSSHKey, validateCreateRequest), core/internal/transport/rclone
 * (key parsing) and FR-7/FR-8/FR-13/FR-15 in docs/EPIC.md. Where two of
 * those disagree, the code wins, because the code is what runs.
 *
 * # What is deliberately NOT in here
 *
 * A field whose effect cannot be stated from the code gets no entry, and its
 * control gets no pop-up. That is not an oversight to be filled in later by
 * whoever notices the gap: this UI has removed invented claims three times
 * (#211, #231, #274), and a help pop-up is the easiest possible place to add
 * a fourth, because a plausible sentence about a decorative control reads
 * exactly like a true one. The controls left out today, and why:
 *
 *   - SettingsPage "Polling interval", "Log level". Neither is wired to
 *     anything (defaultValue, no handler, no save). poll_interval is a real
 *     config key but a duration (documented as 15m), not the 15/30/60
 *     SECONDS this control offers, and there is no log level in
 *     config.Config at all.
 *
 *     "Storage warning threshold" and "Storage critical threshold" left
 *     this list with issue #286, when internal/config grew the capacity
 *     block FR-21's guard had been waiting on: they are now real fields on
 *     UpdateSettingsRequest.capacity, and the storage cap beside them
 *     joined the catalogue at the same time.
 *   - SettingsPage "Webhook notifications". config.Alerts' own doc says
 *     where an alert goes is deliberately not configurable and that there is
 *     no URL for this package to validate. Explaining this control would
 *     mean describing a capability the design explicitly refused.
 *   - ActivityPage "Time range". Nothing reads it and listActivity takes no
 *     window argument.
 *   - The wizard's remaining decorative controls: exclude patterns; the
 *     four per-set retention controls (Daily/Weekly/Monthly/Week starts,
 *     plus its own always-checked "protect newest known-good" toggle,
 *     distinct from the real global one on Settings); the always-on
 *     transfer-verification toggle; the checksum-verification toggle
 *     (`newBackupSetFor` in core/service/backupsets.go sets `Hash: ""`
 *     unconditionally, so this toggle has no field to write to); the
 *     "Generate dedicated SSH key" panel's static content (a fixed sample
 *     key, and an authorized_keys path that always names "backup-agent"
 *     regardless of the username actually entered); and the "Use managed
 *     key" branch entire, whose picklist offers two hardcoded key names
 *     and a fabricated "Already installed on 2 other backup sets" count
 *     with no backend behind either. Per-set retention in particular is
 *     not merely unwired: issue #111 decided retention is one global
 *     policy and specifically warned that this UI's already-drawn per-set
 *     shape must not be mistaken for a capability. All of it is tracked as
 *     one issue (#299) rather than fixed here, since each needs its own
 *     product decision (wire it, or remove it), not a tooltip.
 */

/** One field's help copy. All three parts are required. */
export interface FieldHelpCopy {
  /** What the field is for, in the operator's terms. */
  what: string;
  /** Something they could actually type. Rendered as code. */
  example: string;
  /** What typing that would cause. Never a restatement of `what`. */
  effect: string;
}

/**
 * Keyed by a short, stable name per field. The key is referenced from the
 * page that renders the field, so an unused entry here is a field that lost
 * its pop-up and a missing one is a field that never had it.
 */
export const FIELD_HELP = {
  // ---------------------------------------------------------------- auth

  loginUsername: {
    what: "The Backup Manager account created on this NAS. Separate from the NAS's own administrator account, and separate from any login on a server you back up.",
    example: "backup-admin",
    effect:
      "Checked against the single administrator account this instance stores locally. No NAS OS account and no remote host is contacted, so a NAS password will not work here even if it is the one you use everywhere else."
  },

  loginPassword: {
    what: "The password set for that Backup Manager account when it was created, or the one it was last rotated to.",
    example: "a passphrase of at least 12 characters",
    effect:
      "A match starts a signed-in session in this browser and replaces this form with the application. A miss is reported as one combined failure, so a wrong username and a wrong password cannot be told apart from the message, and repeated attempts from one address are rate limited rather than answered faster."
  },

  enrollUsername: {
    what: "The name for the administrator account you are creating now. This is the first-run step, and it is separate from your NAS operating-system account.",
    example: "backup-admin",
    effect:
      "Creates that account on this NAS and signs you straight in as it. Enrolment is single-shot: once an administrator exists the endpoint refuses another, so this is the name for every later sign-in unless the credential store is reset at the host."
  },

  enrollPassword: {
    what: "The password for the administrator account being created. Twelve characters is the minimum this form accepts.",
    example: "a passphrase of at least 12 characters",
    effect:
      "Stored on this NAS as a verifier for future sign-ins and never sent off the device. There is no recovery flow: losing it means recovering access at the host, not through this page."
  },

  enrollConfirm: {
    what: "The same password again.",
    example: "the identical passphrase",
    effect:
      "Nothing is submitted until the two match. It exists to catch a typo in a field whose characters you cannot see, at the one moment a typo would lock you out of the instance you are creating."
  },

  currentPassword: {
    what: "The password this account signs in with today, proving the rotation is being done by whoever holds the account rather than by whoever found the browser unlocked.",
    example: "the passphrase you signed in with",
    effect:
      "Sent with the rotation request and checked server-side. If it is wrong nothing changes at all, and the message says so rather than reporting a partial rotation."
  },

  newPassword: {
    what: "The password to switch to. Twelve characters is the minimum, the same floor first-run enrolment applies.",
    example: "a new passphrase of at least 12 characters",
    effect:
      "On success this becomes the only password that signs in, and every OTHER signed-in session for this administrator is signed out. This tab keeps working, because its session is reissued as part of the change."
  },

  confirmNewPassword: {
    what: "The new password again.",
    example: "the identical new passphrase",
    effect:
      "The Change password button stays disabled until the two match, so a mistyped new password cannot become the one you have to sign in with next time."
  },

  // ----------------------------------------------------------- retention

  retentionTimezone: {
    what: "The IANA timezone the retention chain does its calendar arithmetic in. Every tier buckets backups by calendar day, week, month, quarter, half-year or year, and something has to decide which calendar.",
    example: "America/Vancouver",
    effect:
      "Moves the boundaries every bucket is measured from. A backup taken at 23:30 UTC falls on one day in UTC and on the previous day in America/Vancouver, so switching timezone can move it into a different daily bucket, change which backup is the newest one in that bucket, and therefore change which backup the daily tier keeps. It must be a timezone the server can load; an unknown name is refused rather than quietly treated as UTC."
  },

  weekStartsOn: {
    what: "Which weekday a week-granularity bucket begins on.",
    example: "monday",
    effect:
      "Decides where the week boundary falls for every tier whose granularity or window unit is a week. With monday, a Sunday backup belongs to the week that began six days earlier and competes with the backups already in it; with sunday it opens a new week and is that week's newest by default. A backup that was the only member of its bucket can stop being kept when the boundary moves under it."
  },

  tierName: {
    what: "This tier's identifier. Lower_snake_case, starting with a letter, and unique within the chain.",
    example: "fortnightly",
    effect:
      "This is the name you are shown later when you ask why a backup survived: the retention preview badges a backup this tier keeps as Fortnightly, over a verdict that carries FORTNIGHTLY. It is a label, not a rule, so renaming a tier changes nothing about what it keeps. last_known_good is reserved, because that name already means the protected backup rather than a tier selection."
  },

  tierGranularity: {
    what: "The calendar bucket this tier groups backups into. Each bucket keeps at most one backup: the newest good one in it.",
    example: "Week",
    effect:
      "Sets how coarse this tier's survivors are. On Week, one backup survives per calendar week inside the tier's window and every other backup that week becomes a delete candidate unless some other tier claims it, because a backup kept by any tier is kept. Custom period is the escape hatch for a step the named list does not cover, and it is the one choice that needs a period length beside it."
  },

  tierPeriodDays: {
    what: "How long one bucket of a Custom period tier is, in days. It is required for Custom period and is not offered for any other granularity.",
    example: "14",
    effect:
      "Makes each bucket 14 days long, so one backup survives per fortnight inside this tier's window. The blocks are anchored to a fixed epoch rather than to today, so their boundaries do not shift depending on which day the retention pass happens to run."
  },

  tierKeep: {
    what: "How many look-back units this tier reaches back over, counting the current one.",
    example: "7",
    effect:
      "7 on a day-granularity tier covers today and the six days before it, so seven daily survivors. A backup older than that window is not kept by this tier at all, and if no other tier claims it and last-known-good protection does not, the next retention apply lists it for deletion. Lowering this number is the fastest way to widen what a later apply may delete."
  },

  tierWindowUnit: {
    what: "The unit this tier's look-back is counted in, when that is not its own granularity. Same as granularity is the ordinary case.",
    example: "Month, on a Week tier with Keep 3",
    effect:
      "Separates how often a backup is kept from how far back keeping goes. A week tier with Keep 3 and window unit Month keeps one backup per week across three calendar months, which is roughly thirteen survivors rather than the three you would get counting in weeks. This is exactly how the default weekly tier is defined, so a chain without it cannot express the product's own default."
  },

  protectLastKnownGood: {
    what: "FR-19's protection for the newest backup this system has actually verified and committed.",
    example: "leave it on",
    effect:
      "On, that backup is kept even after it has aged out of every tier window, so a system that stopped producing backups a year ago still has one. Off, age alone can select it, and the retention pass treats the configuration as materially more dangerous. Turning it off deletes nothing by itself: it widens what a later apply may delete, and every apply still shows a preview you have to approve."
  },

  // ------------------------------------------------------------- filters

  activitySetFilter: {
    what: "Which backup set's events the timeline below shows.",
    example: "production/postgres-primary",
    effect:
      "Hides every event belonging to another set. It filters the events already loaded rather than asking the server for more, so it narrows what you see without extending how far back the timeline reaches."
  },

  activitySeverityFilter: {
    what: "The lowest severity worth showing.",
    example: "Warning and above",
    effect:
      "Drops the routine traffic so what is left is what went wrong. Warning and above hides info and ok events; Errors only hides warnings too. Nothing is deleted and nothing is acknowledged by filtering: an error hidden by this control is still an error."
  },

  backupsSetFilter: {
    what: "Which backup set's retained artifacts the table lists.",
    example: "production/postgres-primary",
    effect:
      "Refetches the artifact list for that set alone, so the totals in the page header count only its backups. It also enables Preview retention, which needs one named set to plan a retention pass against."
  },

  // -------------------------------------------------------------- sets

  editSetName: {
    what: "The display name for this backup set. The set's real identity is its source and set pair, which this does not change.",
    example: "Production PostgreSQL",
    effect:
      "Nothing is written. Backup Manager has no endpoint yet for saving an edited backup set, so Save changes reports that plainly instead of appearing to succeed. What the form does still do is check, at save, whether the set changed while this dialog was open, and refuse rather than overwrite someone else's change."
  },

  // ------------------------------------------------------------- wizard

  wizardSetName: {
    what: "This backup set's own name. Combined with its source, name is the set's true identity (FR-7): retention, health and lifecycle are all tracked per set, independently of every other set.",
    example: "postgres-primary",
    effect:
      "The wizard never asks for a source, so every set it saves is filed under the same default source, api, in config.yaml. Name also becomes part of a filename this manager writes to disk, so it can't contain a slash, can't be \".\" or \"..\", and can't have leading or trailing whitespace; the server refuses the save outright if it does."
  },

  wizardHostname: {
    what: "The address of the remote server this backup set pulls artifacts from over SFTP.",
    example: "prod-db-01.internal",
    effect:
      "Used to probe and verify the host's SSH fingerprint on the next step, then to actually connect and transfer files once saved. If you already trusted a fingerprint for this host on the Verify server step, changing this and leaving the field revokes that trust, since it no longer matches what was trusted, and you'll need to fetch and trust the new address's fingerprint before you can save."
  },

  wizardSshPort: {
    what: "The TCP port the remote server's SSH/SFTP service listens on.",
    example: "22",
    effect:
      "Sent with every probe and connection attempt to that host. Leave it blank, or clear it entirely, and Backup Manager treats it as port 22 rather than refusing to proceed. Like the hostname, changing this after trusting a fingerprint on the Verify server step revokes that trust, since port is part of what was trusted."
  },

  wizardUsername: {
    what: "The account on the remote server Backup Manager signs in as over SSH.",
    example: "backup-agent",
    effect:
      "Sent as the SSH username on every connection this backup set makes. That account needs read access to the remote folder you set on the Discovery step, and, once an artifact completes its full verify-and-commit chain, delete access there too: Backup Manager removes the remote copy after that (FR-15)."
  },

  wizardKeySource: {
    what: "How this backup set authenticates to the remote server. Only one of the three choices actually lets you finish this wizard today.",
    example: "Import key",
    effect:
      "The first two options above each open their own panel below, but neither is wired to a save yet: picking either one and clicking any Save button is refused, with a message pointing you at the third option instead. That one is the one that actually lets Save succeed, once its key is imported below and the host is trusted on the next step."
  },

  wizardPrivateKey: {
    what: "The SSH private key Backup Manager will use to sign in to the remote server. Pasted once, then discarded from this screen.",
    example: "-----BEGIN OPENSSH PRIVATE KEY-----…",
    effect:
      "Sent once to the backend when you click Import key, which hands back a fingerprint and an internal reference and nothing else; the pasted text is cleared from this page immediately afterward and the key material never appears here again. It has to be an unencrypted OpenSSH or PEM key: a passphrase-protected one fails to parse and is refused, since nothing later in this flow can ask for the passphrase."
  },

  wizardRemoteFolder: {
    what: "The directory on the remote server Backup Manager watches for finished backup artifacts.",
    example: "/backups/postgresql/",
    effect:
      "Sent as this set's remote path, and it has to be an absolute one; the server refuses the save otherwise. Only files discovered directly under this one directory are ever considered for this backup set, and each is then checked against the include patterns below."
  },

  wizardIncludePatterns: {
    what: "Which filenames under the remote folder count as this backup set's artifacts.",
    example: "*.dump.zst, *.sql.gz",
    effect:
      "Split on commas into a list of glob patterns, each matched against a remote file's own name only, never its path, so a pattern can't contain a slash. A file that matches none of them is invisible to this backup set: never discovered, never transferred, never counted toward retention."
  },

  wizardCompletionMethod: {
    what: "How Backup Manager decides a remote file has finished being written, rather than still being uploaded by its producer (FR-8).",
    example: "Completion marker / manifest",
    effect:
      "Atomic rename and Completion marker both wait for a positive signal from whatever writes the file; one that never gets renamed, or never gets its marker, is never treated as complete and is never backed up, no matter how long it sits there. Stable file size / timestamp instead treats a file as done once it has looked unchanged for a period fixed at one hour, rather than waiting for a signal from the producer, which this wizard doesn't let you adjust; it exists for a producer that can't signal completion at all, and it's a weaker guarantee than the other two."
  },

  wizardNasDestination: {
    what: "The local directory on this NAS where this backup set's verified artifacts are committed and kept.",
    example: "/data/backups/production/postgres/",
    effect:
      "Sent as this set's local path, and it has to be an absolute one. Every transferred, verified artifact is written here, and retention deletes old ones from here too, so this needs to be a path Backup Manager can actually write to, with room for however much you plan to retain."
  },

  wizardValidatorId: {
    what: "An optional external check Backup Manager runs against every artifact after it's transferred and checksummed (FR-13), on top of that built-in verification.",
    example: "None (transfer and checksum verification only)",
    effect:
      "Choosing a validator sends its id with the save, and every future artifact in this set runs that check before being trusted. An artifact the validator rejects is quarantined rather than committed, and its remote copy is kept rather than deleted, permanently: FR-13 requires a required validator's failure to block deletion of the source, and that block outlives even a later reinstatement out of quarantine."
  },

  wizardAcknowledge: {
    what: "Confirms you understand when this backup set deletes the copy on the remote server, not just that a backup of it now exists here.",
    example: "check it once you've read the sequence above",
    effect:
      "Every Save button on this page stays disabled until this is checked, alongside a trusted host and an imported key: it's a structural gate, not a formality. Checking it deletes nothing by itself; the remote copy is only removed once that specific artifact has cleared every step in the sequence shown above, never earlier (FR-15)."
  },

  // ------------------------------------------------------------- capacity

  storageCap: {
    what: "A ceiling on how much space this manager may occupy, separate from how full the disk actually is.",
    example: "100 GB, or 0",
    effect:
      "0 is this product's default and means no cap: the manager uses the whole volume, and the dashboard's storage gauge reports against the disk itself. Any other value is enforced, not displayed: a transfer that would push this manager's own usage over that number is refused before it starts, exactly as one a completely full disk cannot hold already is. It must be greater than the critical threshold below, or every transfer would be refused from the moment it is saved."
  },

  storageWarningThreshold: {
    what: "The remaining headroom, in bytes, at or below which the dashboard reports a storage warning. “Headroom” is whichever is smaller of the disk's free space and any configured cap's unused allowance.",
    example: "20 GB",
    effect:
      "Never refuses a transfer by itself: it only changes what the storage panel reports. It must stay at or above the critical threshold below, since remaining headroom is expected to cross the warning line before it reaches the critical one, never the other way round."
  },

  storageCriticalThreshold: {
    what: "The headroom floor, in bytes, at or below which a transfer is refused outright, even one that would technically still fit.",
    example: "10 GB",
    effect:
      "A transfer that would itself drop the remaining headroom to or below this number never begins. Nothing is deleted to make room for it: the transfer is simply skipped and retried on a later cycle, once space has been freed some other way."
  }
} as const satisfies Record<string, FieldHelpCopy>;
