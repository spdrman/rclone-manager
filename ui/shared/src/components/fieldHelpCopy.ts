/**
 * Issue #278 — what every explained input in this UI actually says.
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
 * own handlers; the filter entries from the pages' own filtering code. Where
 * two of those disagree, the code wins, because the code is what runs.
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
 *   - SettingsPage "Polling interval", "Log level", "Storage warning
 *     threshold", "Storage critical threshold". None is wired to anything
 *     (defaultValue, no handler, no save), and UpdateSettingsRequest carries
 *     only `retention`. poll_interval is a real config key but a duration
 *     (documented as 15m), not the 15/30/60 SECONDS this control offers;
 *     there is no log level in config.Config at all; and FR-21's capacity
 *     thresholds have no config field yet.
 *   - SettingsPage "Webhook notifications". config.Alerts' own doc says
 *     where an alert goes is deliberately not configurable and that there is
 *     no URL for this package to validate. Explaining this control would
 *     mean describing a capability the design explicitly refused.
 *   - ActivityPage "Time range". Nothing reads it and listActivity takes no
 *     window argument.
 *   - The wizard's decorative controls (exclude patterns, the four per-set
 *     retention controls, the two verification toggles, the managed-key
 *     picker). Per-set retention in particular is not merely unwired: issue
 *     #111 decided retention is one global policy and specifically warned
 *     that this UI's already-drawn per-set shape must not be mistaken for a
 *     capability.
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
      "A match starts a signed-in session in this browser and opens the dashboard. A miss is reported as one combined failure, so a wrong username and a wrong password cannot be told apart from the message."
  },

  enrollUsername: {
    what: "The name for the administrator account you are creating now. This is the first-run step, and it is separate from your NAS operating-system account.",
    example: "backup-admin",
    effect:
      "Creates that account on this NAS and signs you in as it. This is the name you will type on every later sign-in, so it is worth picking one you will recognise months from now in an audit trail."
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
      "This is the name you will be shown later when you ask why a backup survived: a kept backup is reported as kept by FORTNIGHTLY in the retention preview and in every KEEP verdict. It is a label, not a rule, so renaming a tier changes nothing about what it keeps. last_known_good is reserved, because that name already means the protected backup rather than a tier selection."
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
  }
} as const satisfies Record<string, FieldHelpCopy>;
