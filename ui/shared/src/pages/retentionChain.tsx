import { useState } from "react";
import type { ReactNode } from "react";
import { useApi } from "@shared/api/ApiContext";
import { BackupManagerError, LOCAL_DESTINATION_ID } from "@shared/api/contracts";
import { Banner } from "@shared/components/Banner";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";
import { CommandEcho } from "@shared/pages/CommandEcho";
import { MediumPreflightChecks } from "@shared/pages/MediumPreflightChecks";
import { S3DestinationWizard } from "@shared/pages/S3DestinationWizard";
import { testConnectionCommand, tierMediumCommand } from "@shared/pages/storageDestinationCommands";
import type {
  MediumPreflight,
  RetentionSchema,
  RetentionSettings,
  RetentionTierSetting,
  StorageMedium,
  StorageSchema
} from "@shared/api/contracts";

/**
 * The retention CHAIN editor, shared by the two places a chain is edited:
 * the deployment's own policy on the Settings page (RetentionPolicyCard)
 * and one backup set's override on its detail page
 * (BackupSetRetentionCard, issue #333).
 *
 * It lives here rather than in either card because a second tier editor
 * is a second set of rules about what a tier may be, and those two would
 * drift: `suites/equivalence` exists to catch a capability that lands on
 * one surface and not another, and two editors for one concept is the
 * same failure inside a single surface. Everything in this file is pure
 * form state and rendering; neither card's save path is here, because
 * they genuinely differ (a sparse PATCH of the deployment's settings, and
 * a whole-policy PUT of a set's override), and pretending otherwise is
 * how one of them would quietly acquire the other's semantics.
 *
 * Every rule it enforces still comes from the server: the granularity
 * list, the window units, the tier-name pattern, the reserved name, both
 * ceilings and the default chain all arrive in the schema the settings
 * response already carries. Nothing here hardcodes a closed value set.
 */

/** One tier being edited. The two numbers are held as strings so a
 *  half-typed value ("" while the operator clears the field, "1" on the
 *  way to "14") stays exactly what was typed instead of being coerced to
 *  a number and rendered back as something nobody entered. */
export interface TierDraft {
  /** Stable across re-orders and removals, so React keeps the right DOM
   *  node with the right focus; never sent anywhere. */
  key: string;
  name: string;
  granularity: string;
  keep: string;
  periodDays: string;
  windowUnit: string;
  /** The storage destination this tier names (FR-27), by id, with
   *  LOCAL_DESTINATION_ID for the drive on this machine. Held as the id
   *  rather than an index so a destination removed from the
   *  configuration between load and save cannot silently become a
   *  different one.
   *
   *  It is never empty since #622. The backend names a destination on
   *  every tier, the picker offers one on every tier, and a draft that
   *  could hold "" would be a third spelling of local sitting between two
   *  that agree. toDraft fills it in for a tier that arrived without one,
   *  which is what an older engine answers.
   *
   *  It is carried whether or not the row edits it, because a chain save
   *  REPLACES the operator's whole chain: a field the draft dropped would
   *  be deleted from their configuration file by the act of changing
   *  something else, which is precisely what service.RetentionTier.Medium's
   *  own doc calls a lossy boundary. Editing daily's keep must not
   *  quietly move monthly's artifacts back onto local disk. */
  medium: string;
}

let nextTierKey = 0;
/** Turns a saved tier into an editable row, numbers included, since the
 *  draft holds everything as typed text. The key is minted from a
 *  module-level counter rather than from the tier's name or its position,
 *  because both of those change while the operator is editing and React
 *  would then move focus out of the box being typed in. */
export function toDraft(t: RetentionTierSetting): TierDraft {
  nextTierKey += 1;
  return {
    key: "tier-" + nextTierKey,
    name: t.name,
    granularity: t.granularity,
    keep: String(t.keep),
    periodDays: t.periodDays ? String(t.periodDays) : "",
    windowUnit: t.windowUnit ?? "",
    // A tier that named no destination is on the local hard drive, which
    // is what absence meant before #622 and what an older engine still
    // answers. Resolved here, once, rather than at each place that reads
    // the draft.
    medium: t.medium || LOCAL_DESTINATION_ID
  };
}

/** The granularity value that means "a window the operator states in
 *  days" rather than a named calendar period. It is the one value that
 *  makes `periodDays` meaningful, and the only one the server accepts it
 *  alongside. */
export const CUSTOM_PERIOD = "days";

/** The value of the picker's one unselectable, not-built row (#595).
 *
 *  A sentinel rather than "" so it can never be confused with the drive on
 *  this machine, which is the row that DOES work and is spelled by
 *  LOCAL_DESTINATION_ID since #622. It is never submitted: the option is
 *  disabled, and the id is not one a `storage_mediums` entry could declare
 *  anyway, since the config layer requires lower_snake_case starting with
 *  a letter. */
export const NOT_BUILT_LOCAL_VOLUME = "__not_built_local_volume";

/** The chain "Restore default chain" fills the form with, taken from the
 *  schema the server already serves alongside the values rather than
 *  written out here.
 *
 *  This used to be a literal 7/3/12 chain, which was a second spelling of
 *  something config.DefaultTierChain's own doc says has exactly one, and
 *  not a harmless one: restoring the default and saving writes an EXPLICIT
 *  tiers list, which clears the legacy scalars and permanently migrates a
 *  config that would have tracked the product's default onto whatever this
 *  file happened to say. A stale copy could therefore narrow a real
 *  retention window, silently, in the dangerous direction, with nothing
 *  comparing the two. Every other closed value set in this card is already
 *  served by the schema for the same reason. */
export function defaultChain(schema: RetentionSchema): TierDraft[] {
  return schema.defaultTiers.map(toDraft);
}

/**
 * The destination a NEWLY CREATED tier starts on: the one the deployment
 * marked default, or the local hard drive when nothing is marked (#622).
 *
 * Read off the served list rather than held as a preference in this form,
 * for the reason defaultChain is served rather than written out here: it
 * is a value the deployment decided and a second copy would be free to go
 * stale, and a stale one here would start somebody's next tier on a
 * destination they moved away from.
 *
 * Falling back to the local hard drive rather than to the first entry is
 * the safe direction. An engine older than #622 marks nothing, and its
 * first declared medium is a bucket: starting a new tier there would send
 * backups off the machine on the strength of a field that was not
 * answered, which is exactly the write FR-27's disclosure stands in front
 * of.
 */
export function defaultDestinationId(mediums: StorageMedium[]): string {
  return mediums.find((m) => m.isDefault)?.id ?? LOCAL_DESTINATION_ID;
}

/** Operator-facing words for the granularities the schema serves. A
 *  presentation table only: the legal SET comes from the schema, so a
 *  granularity added server-side still renders, under its own raw name,
 *  rather than disappearing from the picklist. */
export const GRANULARITY_LABELS: Record<string, string> = {
  day: "Day",
  week: "Week",
  month: "Month",
  quarter: "Quarter",
  half_year: "Half year",
  year: "Year",
  days: "Custom period"
};

/** The label for a granularity, or the raw value when this build has no
 *  word for it. Falling back to the value is deliberate: an unlabelled
 *  tier an operator can still read and keep beats one that renders blank. */
export function granularityLabel(value: string): string {
  return GRANULARITY_LABELS[value] ?? value;
}

/** The week-start options, lower-case because that is the spelling the
 *  config file and the wire use. Rendering capitalises; nothing here
 *  translates, so what is shown and what is sent cannot drift. */
export const WEEKDAYS = ["monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"];

export function TierRow({
  index,
  tier,
  schema,
  mediums,
  errors,
  readOnly,
  canRemove,
  onChange,
  onRemove,
  onDestinationsChanged
}: {
  index: number;
  tier: TierDraft;
  schema: RetentionSchema;
  /** Every destination this deployment has: the drive backups land on
   *  first, then whatever the configuration declares. It is never empty
   *  on a current engine (#622), and the picker is rendered whether or
   *  not anything is declared, which is the reversal that issue asks
   *  for.
   *
   *  It used to be hidden when nothing was declared, on the reasoning
   *  that there was nowhere else for a backup to go. That was right about
   *  the choices and wrong about the operator: with the control absent
   *  there was no way to see where a tier's backups DO go, and no
   *  affordance for putting them anywhere else, so the only tier that
   *  could ever be pointed at S3 was one edited outside the product.
   *
   *  An empty list is still handled rather than assumed away, because an
   *  older engine answers one: the picker then offers the local hard
   *  drive alone, which is the honest rendering of "this deployment has
   *  one destination". */
  mediums: StorageMedium[];
  errors: TierErrors;
  readOnly: boolean;
  canRemove: boolean;
  onChange(patch: Partial<TierDraft>): void;
  onRemove(): void;
  /** Called after a destination is created from inside this row, so the
   *  card above reloads the list and the new one is selectable without a
   *  page reload. Optional: the per-set editor passes it as readily as
   *  the settings one, and a row rendered without it simply offers no
   *  inline create. */
  onDestinationsChanged?(): void;
}) {
  const custom = tier.granularity === CUSTOM_PERIOD;
  const position = index + 1;

  return (
    <div
      role="group"
      aria-label={"Tier " + position}
      style={{
        display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))",
        gap: "10px 14px", alignItems: "start", padding: "12px 14px",
        border: "1px solid var(--border)", borderRadius: "var(--radius-md)",
        background: "var(--surface-2)"
      }}
    >
      <Field label="Name" help={FIELD_HELP.tierName} error={errors.name}>
        {(helpId) => (
          <input
            className="input input--mono"
            aria-describedby={helpId}
            value={tier.name}
            disabled={readOnly}
            onChange={(e) => onChange({ name: e.target.value })}
          />
        )}
      </Field>

      <Field label="Granularity" help={FIELD_HELP.tierGranularity}>
        {(helpId) => (
          <select
            className="select"
            aria-describedby={helpId}
            value={tier.granularity}
            disabled={readOnly}
            onChange={(e) => onChange({ granularity: e.target.value })}
          >
            {schema.granularities.map((g) => (
              <option key={g} value={g}>
                {granularityLabel(g)}
              </option>
            ))}
          </select>
        )}
      </Field>

      {custom ? (
        <Field label="Period (days)" help={FIELD_HELP.tierPeriodDays} error={errors.periodDays}>
          {(helpId) => (
            <input
              className="input"
              type="number"
              aria-describedby={helpId}
              min={1}
              max={schema.periodDaysMax}
              value={tier.periodDays}
              disabled={readOnly}
              onChange={(e) => onChange({ periodDays: e.target.value })}
            />
          )}
        </Field>
      ) : null}

      <Field label="Keep" help={FIELD_HELP.tierKeep} error={errors.keep}>
        {(helpId) => (
          <input
            className="input"
            type="number"
            aria-describedby={helpId}
            min={1}
            max={schema.keepMax}
            value={tier.keep}
            disabled={readOnly}
            onChange={(e) => onChange({ keep: e.target.value })}
          />
        )}
      </Field>

      {/* A custom period measures its own window, so it never carries a
          window unit — the server refuses that combination outright, so
          the control is absent rather than present and ignored. */}
      {custom ? null : (
        <Field label="Window unit" help={FIELD_HELP.tierWindowUnit}>
          {(helpId) => (
            <select
              className="select"
              aria-describedby={helpId}
              value={tier.windowUnit}
              disabled={readOnly}
              onChange={(e) => onChange({ windowUnit: e.target.value })}
            >
              <option value="">Same as granularity</option>
              {schema.windowUnits.map((u) => (
                <option key={u} value={u}>
                  {granularityLabel(u)}
                </option>
              ))}
            </select>
          )}
        </Field>
      )}

      {/* The picker is on every tier, always. The class is part of the
          choice, not decoration: one of these places cannot be read
          without a restore, and an operator picking blind would find that
          out hours later, holding a restore request they did not know
          they needed.

          A medium whose reads need a restore is listed and NOT selectable.
          The server refuses a tier bound to one when the config loads
          (#442): a copy written to an archive class is archived the
          instant it lands, so the move can never be verified and the tier
          can never take delivery. Offering the choice and then refusing
          the save is a trap, and hiding the medium is worse: an operator
          who declared it wants to know it is there, and it IS legal to
          declare one and restore from it by hand. So it stays on the list,
          greyed out, saying why. */}
      <div style={{ gridColumn: "1 / -1", display: "flex", flexDirection: "column", gap: 8 }}>
        <Field label="Stored on" help={FIELD_HELP.tierMedium}>
          {(helpId) => (
            <select
              className="select"
              aria-describedby={helpId}
              // "Storage medium", not "Storage destination", and that is a
              // deliberate hold rather than a spelling I missed. The word
              // in the visible label is "Stored on", the group beside it
              // is already named "Storage medium disclosure", and this
              // accessible name is what the black-box suite in
              // spdrman/rclone-manager-tests queries the picker by at the
              // sha this repository pins. Renaming it is invisible to a
              // sighted operator, buys nothing #622 asked for, and would
              // turn five specs over there red for a word. See #622's PR
              // for the one spec that legitimately does go red.
              aria-label={"Storage medium for tier " + position}
              value={tier.medium}
              disabled={readOnly}
              onChange={(e) => onChange({ medium: e.target.value })}
            >
              {/* The local hard drive comes off the served list rather
                  than being a literal option here, so one function names
                  a destination for every surface that shows one and they
                  cannot drift. An older engine serves no local entry at
                  all, so one is drawn from the constant instead: the tier
                  still points somewhere and the picker still works. */}
              {mediums.some((m) => m.isLocal) ? null : (
                <option value={LOCAL_DESTINATION_ID}>Local backup root</option>
              )}
              {/* A destination the list does not carry still gets an
                  option, naming itself and saying so.

                  Three ways a tier arrives pointing at one: a
                  configuration edited by hand, an engine that answers a
                  chain and a list a moment apart, and the ordinary case
                  of a destination created from this very row, where the
                  tier is pointed at it before the reloaded list comes
                  back. Without this option the select has no match for
                  its own value, renders blank, and the next thing the
                  operator touches silently replaces a destination they
                  never chose to leave. */}
              {tier.medium && !mediums.some((m) => m.id === tier.medium) ? (
                <option value={tier.medium}>{tier.medium + " (not in this deployment's list)"}</option>
              ) : null}
              {/* The third kind of destination #595 asked for, which does
                  not exist. It is here, named and disabled, rather than
                  left off the menu, and both halves of that are the
                  decision.

                  It cannot be built from here. Local means the backup
                  set's own local_path and nothing else, and the config
                  layer's medium type set is closed to s3 alone. (`local`
                  is a name a tier CAN spell on this boundary since #622,
                  and it still names that one directory: what does not
                  exist is a SECOND local destination, which is what this
                  row is about.)
                  transport.MediumTypeLocalDir exists and its own doc says
                  it is NOT configurable, because "'local' as a MEDIUM
                  would be a second answer to where local artifacts live",
                  and MediumType's doc says the set "grows only by an FR-4
                  architecture decision, never by an import line". A form
                  is not where that decision gets made.

                  And hiding it would answer the operator worse than this
                  does. Somebody who came to this picker to send monthly
                  backups to the second disk in the NAS learns nothing
                  from a menu that never mentions it, and asks again next
                  month; a row that says the shape is understood and not
                  built is a real answer. Its value can never be
                  submitted: the option is disabled, so the select refuses
                  it, and a medium id nothing declares would be refused at
                  config load anyway. */}
              <option value={NOT_BUILT_LOCAL_VOLUME} disabled>
                A saved local volume, such as a second hard disk (NOT BUILT: a second local
                destination is a new medium type, which is an architecture decision rather than
                a setting)
              </option>
              {mediums.map((m) => (
                <option key={m.id} value={m.id} disabled={m.readsRequireRestore}>
                  {destinationLabel(m)}
                </option>
              ))}
            </select>
          )}
        </Field>

        <TierDestinationActions
          tier={tier}
          readOnly={readOnly}
          onDestinationsChanged={onDestinationsChanged}
          onPick={(id) => onChange({ medium: id })}
        />
      </div>

      <div style={{ alignSelf: "end" }}>
        <button
          className="btn btn--sm"
          type="button"
          aria-label={"Remove tier " + position}
          disabled={readOnly || !canRemove}
          onClick={onRemove}
        >
          Remove
        </button>
      </div>
    </div>
  );
}


/**
 * The name a destination goes by in a picker.
 *
 * The drive on this machine is "Local backup root" and nothing else, and
 * that is a decision rather than the old string surviving by inertia. A
 * picker is a list of places to choose BETWEEN, and there is exactly one
 * local root in any deployment, so a path here is a detail that says
 * nothing about the choice being made and is read past from the second
 * time onwards. #622's complaint about the path was about the
 * DESTINATIONS LIST, which is where an operator goes to see what their
 * destinations are, and that is where it now appears (see
 * StorageDestinationsCard's describeDestination).
 *
 * It is also the name the black-box suite in spdrman/rclone-manager-tests
 * pins at the sha this repository pins, which is a reason to keep a good
 * word rather than a reason to keep any word: renaming it would cost a
 * spec over there and buy an operator a path they already have one screen
 * away.
 *
 * A declared destination names its storage class, which IS part of the
 * choice rather than decoration: one of these places cannot be read
 * without a restore, and the label says so before it is picked rather
 * than after.
 */
export function destinationLabel(m: StorageMedium): string {
  if (m.isLocal) return "Local backup root";
  return (
    m.id +
    " (" +
    m.storageClass +
    (m.readsRequireRestore ? ", cannot receive backups: reads need a restore" : "") +
    // Issue #636, and it belongs on the CHOICE rather than only on the
    // destinations list. This is the moment an operator decides to send a
    // tier's backups somewhere, and "nothing has ever shown that this
    // place works" is exactly the kind of thing to know before that save
    // rather than after it. It reads as a label and not a refusal,
    // because an unproven destination is a legitimate thing to pick: the
    // operator who declared it offline is the operator picking it.
    //
    // Appended, so a destination carrying no mark keeps the label it had
    // byte for byte. An <option> can hold no markup, so this says it in
    // words the way the two clauses before it do.
    (m.connectionUnverified ? ", never proven" : "") +
    ")"
  );
}

/**
 * Where the local destination actually writes, for the surfaces that show
 * a destination rather than offer a choice between them (#622).
 *
 * A deployment that cannot place it yet, because it has no backup set or
 * because its sets are on genuinely different volumes, says so rather
 * than rendering a blank path: a row ending in a colon and nothing reads
 * as a bug, and "not known yet" is the true answer.
 */
export function localDriveDescription(m: StorageMedium): string {
  return m.path ? m.path : "which drive this writes to is not known yet";
}

/**
 * The two things an operator has to be able to do WITHOUT leaving the
 * tier: prove the destination it names, and make a new one (#622).
 *
 * That is the whole point of putting a picker here rather than sending
 * somebody to the settings page and back. A destination created in the
 * middle of choosing one needs the same check before it is trusted, and a
 * detour to another page to run that check is a detour most people will
 * not take.
 *
 * The test is offered and never required, which is MediumPreflightPanel's
 * own rule one component over: an operator who is about to fix the bucket,
 * or who knows something this check does not, is not served by a form that
 * refuses. What it does is make the answer available before the
 * consequence.
 *
 * The command echo is EPIC G's standing rule: the line under the picker is
 * the one that reproduces the click, so somebody who moved one tier by
 * clicking has read the command that moves the next fifty.
 */
function TierDestinationActions({
  tier,
  readOnly,
  onDestinationsChanged,
  onPick
}: {
  tier: TierDraft;
  readOnly: boolean;
  onDestinationsChanged?(): void;
  onPick(mediumId: string): void;
}) {
  const api = useApi();
  const [busy, setBusy] = useState(false);
  // The report, and the destination it was asked about. They are held
  // together because a report about somewhere else is not a weaker answer
  // than no report, it is a wrong one.
  //
  // A check writes an object to a bucket and reads it back, so it is slow
  // by nature, and a picker sits right beside the button. Change the
  // selection while one is in flight and the old destination's answer
  // used to render under the new selection, saying "This destination is
  // ready for a backup" about a destination nobody had checked. That is
  // the sentence an operator reads before sending a tier's backups
  // somewhere.
  //
  // Keyed by MediumPreflight.Medium, which the engine already fills in
  // with the id it ran against, rather than by a separate note of what
  // was requested: the answer says who it is about, so nothing here has
  // to remember. Everything below renders only when that id is still the
  // one the tier names, so a stale response is not cancelled, it is
  // simply never anybody's answer.
  //
  // This is the shape #628 reached for the same hazard on the source side
  // (connectionTestedFor), and it is worth the two files matching: one
  // epic answering one question two ways is how the next person picks the
  // weaker one.
  const [report, setReport] = useState<MediumPreflight | null>(null);
  const [error, setError] = useState<{ medium: string; message: string } | null>(null);
  const [adding, setAdding] = useState(false);

  const shown = report && report.medium === tier.medium ? report : null;
  const shownError = error && error.medium === tier.medium ? error.message : null;

  function test() {
    const asked = tier.medium;
    setBusy(true);
    setReport(null);
    setError(null);
    api
      .preflightStorageMedium(asked)
      .then(setReport)
      .catch((e: unknown) =>
        // The failure is tagged with what was ASKED, because a rejection
        // carries no report to read an id off. Same rule either way: an
        // answer is shown under the destination it is about.
        setError({
          medium: asked,
          message:
            e instanceof BackupManagerError
              ? e.api.message
              : "Backup Manager could not test the connection to this destination."
        })
      )
      .finally(() => setBusy(false));
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
        <button className="btn btn--sm" type="button" disabled={busy} onClick={test}>
          {busy ? "Testing…" : "Test connection"}
        </button>
        {onDestinationsChanged ? (
          <button className="btn btn--sm" type="button" disabled={readOnly} onClick={() => setAdding(true)}>
            Add a destination
          </button>
        ) : null}
        {shown ? (
          <span style={{ fontSize: "var(--text-sm)", fontWeight: 600 }}>
            {shown.ok
              ? "This destination is ready for a backup."
              : "This destination is not ready. Saving is still allowed; the checks below say why."}
          </span>
        ) : null}
      </div>

      <CommandEcho
        label="the same thing from a terminal"
        commands={
          tier.name
            ? [tierMediumCommand(tier.name, tier.medium), testConnectionCommand(tier.medium)]
            : // A tier with no name yet cannot be named on a command line,
              // and a line reading "--tier-medium =offsite_s3" is not a
              // command an operator can paste. The test connection is
              // still nameable, because it is about the destination
              // rather than about the tier.
              [testConnectionCommand(tier.medium)]
        }
      />

      {shownError ? (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--danger)" }}>{shownError}</p>
      ) : null}
      {shown ? <MediumPreflightChecks report={shown} /> : null}

      {adding ? (
        <S3DestinationWizard
          onClose={() => setAdding(false)}
          onSaved={(created) => {
            setAdding(false);
            // Picked immediately, because choosing it is why they made
            // it. A wizard that saved and left the tier on its old
            // destination would make the operator repeat the choice they
            // already expressed by creating the thing.
            if (created) onPick(created.id);
            onDestinationsChanged?.();
          }}
        />
      ) : null}
    </div>
  );
}

/**
 * One labelled control, its help pop-up (#278) and its validation message.
 *
 * The message is a SIBLING of the <label>, never inside it. A wrapping
 * label's accessible name is its whole text content, so an error rendered
 * inside it renames the control from "Keep" to "KeepKeep at least 1
 * look-back unit." — which breaks assistive technology and every
 * label-based query alike, silently, and only once the field is invalid.
 *
 * The help copy is kept out of the label for the same reason and reaches
 * the control the other way round, through its aria-describedby: a
 * description is announced after the name rather than becoming part of it,
 * which is exactly the difference between "Keep, edit, 7, how many
 * look-back units..." and a control whose name is three sentences long.
 * That is why `children` is a function: the control has to be handed the
 * id, and there is no honest way to attach a description to a control this
 * component cannot see.
 */
export function Field({
  label,
  help,
  error,
  children
}: {
  label: string;
  help: FieldHelpCopy;
  error?: string;
  children: (helpId: string) => ReactNode;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
      <HelpField label={label} help={help}>
        {children}
      </HelpField>
      {error ? (
        <span style={{ fontSize: "var(--text-sm)", color: "var(--danger)" }}>{error}</span>
      ) : null}
    </div>
  );
}

/** The problems one row can have, keyed by the box they belong in, so a
 *  message renders beside the field it is about rather than as a summary
 *  the operator has to map back onto a chain of several tiers. */
export interface TierErrors {
  name?: string;
  keep?: string;
  periodDays?: string;
}

/**
 * The same rules core/internal/config's validateRetentionTiers applies,
 * checked here against the schema the server itself served so the two
 * cannot drift. A duplicate is reported on the LATER tier only (the one
 * that claimed a name an earlier tier already holds), matching the
 * backend's own message and keeping one mistake to one message.
 */
export function tierErrors(
  tier: TierDraft,
  index: number,
  all: TierDraft[],
  schema: RetentionSchema,
  namePattern: RegExp
): TierErrors {
  const errors: TierErrors = {};

  const firstWithName = all.findIndex((t) => t.name === tier.name);
  if (tier.name === "") {
    errors.name = "Name this tier before saving.";
  } else if (!namePattern.test(tier.name)) {
    errors.name = "Tier names are lower_snake_case: letters, digits and underscores, starting with a letter.";
  } else if (tier.name === schema.reservedTierName) {
    errors.name = "“" + schema.reservedTierName + "” is reserved for last-known-good protection.";
  } else if (firstWithName !== index) {
    errors.name = "“" + tier.name + "” is already used by tier " + (firstWithName + 1) + ".";
  }

  const keep = Number(tier.keep);
  if (tier.keep.trim() === "" || !Number.isInteger(keep) || keep < 1) {
    errors.keep = "Keep at least 1 look-back unit.";
  } else if (keep > schema.keepMax) {
    errors.keep = "Keep must not exceed " + schema.keepMax + " look-back units.";
  }

  if (tier.granularity === CUSTOM_PERIOD) {
    const period = Number(tier.periodDays);
    if (tier.periodDays.trim() === "" || !Number.isInteger(period) || period < 1) {
      errors.periodDays = "A custom period needs a length of at least 1 day.";
    } else if (period > schema.periodDaysMax) {
      errors.periodDays = "A custom period must not exceed " + schema.periodDaysMax + " days.";
    }
  }

  return errors;
}

/** Turns an edited row back into what the wire expects, which is where
 *  the text-versus-number split is finally paid off. It is also where the
 *  custom-period rule is applied, because `period_days` is legal only on
 *  that granularity and sending it otherwise is a refusal. */
export function toTierSetting(t: TierDraft): RetentionTierSetting {
  const custom = t.granularity === CUSTOM_PERIOD;
  return {
    name: t.name,
    granularity: t.granularity,
    keep: Number(t.keep),
    // period_days is legal only on the custom period, and a window unit
    // only on everything else, so each is dropped rather than sent as a
    // stray value the server refuses.
    periodDays: custom ? Number(t.periodDays) : undefined,
    windowUnit: custom || !t.windowUnit ? undefined : t.windowUnit,
    // Carried back out by name, the local hard drive included. The
    // backend is where that becomes the absence a configuration file
    // spells local with, in one place, so sending it here is what keeps
    // the read and the write agreeing about where this tier points
    // (#622). Sending undefined would work too and would be worse: it
    // would put a second spelling of local on the wire and leave the
    // server unable to tell "on local disk" from "this client is too old
    // to have an opinion".
    medium: t.medium || LOCAL_DESTINATION_ID
  };
}

/** A stable string for "is this the same chain", used both to decide
 *  whether to send `tiers` at all and to remount the editor when the
 *  loaded policy changes. Built from a fixed field order, so it never
 *  depends on object key ordering. */
export function chainKey(tiers: RetentionTierSetting[]): string {
  return tiers
    .map((t) =>
      [t.name, t.granularity, t.periodDays ?? 0, t.keep, t.windowUnit ?? "", t.medium || LOCAL_DESTINATION_ID].join(":")
    )
    .join("|");
}

export function settingsKey(r: RetentionSettings): string {
  return [r.timezone, r.weekStartsOn, String(r.protectLastKnownGood), chainKey(r.tiers)].join("~");
}

/**
 * Which tiers a chain about to be saved would NEWLY send off local disk
 * (FR-27's consent), relative to the chain currently in effect.
 *
 * Per tier, and matched by NAME, which is the same rule core/service
 * applies on the server: a chain that already sends monthly to a medium
 * has consented to monthly's backups leaving, and to nothing else, so a
 * tier that is new or that moves to a different medium asks again while
 * an edit to an unrelated number does not. A product that asked every
 * time would train an operator to tick the box without reading it, which
 * is worse than not asking.
 *
 * This decides what a form SHOWS. It never decides whether the write is
 * allowed: the server refuses an unacknowledged write with
 * MEDIUM_DISCLOSURE_REQUIRED whatever this computes.
 */
export function introducedMediumMappings(
  next: RetentionTierSetting[],
  current: RetentionTierSetting[]
): RetentionTierSetting[] {
  return next.filter((t) => {
    // The local hard drive is where the backups already are, so naming it
    // discloses nothing and asks nobody to acknowledge anything. It is
    // spelled by its reserved id since #622, and by absence on an older
    // engine, and both mean the same thing here.
    if (!t.medium || t.medium === LOCAL_DESTINATION_ID) return false;
    const was = current.find((b) => b.name === t.name);
    return !was || (was.medium || LOCAL_DESTINATION_ID) !== t.medium;
  });
}

/**
 * The storage-medium disclosure (FR-27), shared by the two chain editors
 * for the reason TierRow is: the sentence an operator reads before their
 * backups leave this machine must be the same sentence on both surfaces.
 *
 * Every tier is named, by name and by destination, never as a count. "2
 * tiers" would send an operator off to work out which two, and the whole
 * point of this panel is that they know what they are agreeing to before
 * they agree to it. The paragraphs are the backend's own words, served
 * alongside the settings: the server refuses an unacknowledged write with
 * this same text, so what the form shows and what the server enforces
 * cannot come apart.
 */
export function MediumDisclosure({
  introduced,
  mediums,
  storage,
  acknowledged,
  disabled,
  onChange
}: {
  introduced: RetentionTierSetting[];
  mediums: StorageMedium[];
  storage: StorageSchema;
  acknowledged: boolean;
  disabled: boolean;
  onChange(acknowledged: boolean): void;
}) {
  const archiveAhead = introduced.some(
    (t) => mediums.find((m) => m.id === t.medium)?.readsRequireRestore
  );
  return (
    // Not dismissible (#620). The acknowledgement checkbox at the foot of
    // this banner is what arms Save, so putting the banner away would
    // leave Save permanently off with nothing on screen saying why.
    <Banner
      tone="warn"
      role="group"
      ariaLabel="Storage medium disclosure"
      dismissible={false}
      style={{ flexDirection: "column", gap: 10 }}
    >
      <div style={{ fontWeight: 600 }}>Saving this sends backups off this machine.</div>
      <ul style={{ margin: 0, paddingLeft: 20, fontSize: "var(--text-sm)" }}>
        {introduced.map((t) => (
          <li key={t.name}>
            <span className="mono">{t.name}</span>
            {" keeps its backups on "}
            <span className="mono">{t.medium}</span>
            {" from now on."}
          </li>
        ))}
      </ul>
      <p style={{ margin: 0, fontSize: "var(--text-sm)", maxWidth: "78ch" }}>
        {storage.mediumDisclosure}
      </p>
      {archiveAhead ? (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", maxWidth: "78ch" }}>
          At least one of those mediums is on a storage class that cannot be read on demand at
          all. A backup there has to be restored before anything can read it.
        </p>
      ) : null}
      <p style={{ margin: 0, fontSize: "var(--text-sm)", maxWidth: "78ch" }}>
        {storage.retrievalDisclosure}
      </p>
      <MediumPreflightPanel
        mediumIds={[...new Set(introduced.map((t) => t.medium).filter((id): id is string => !!id))]}
        disabled={disabled}
      />
      <label style={{ display: "flex", gap: 10, alignItems: "flex-start", fontSize: "var(--text-base)" }}>
        <input
          type="checkbox"
          checked={acknowledged}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span>
          I understand that backups these tiers keep will be deleted from this machine after
          they upload, and that reading them back costs money and, on an archive class, hours.
        </span>
      </label>
    </Banner>
  );
}

/**
 * The medium preflight, offered exactly where an operator is about to
 * send backups somewhere for the first time (issue #443).
 *
 * This is the point in the product where "does that bucket actually work"
 * stops being idle curiosity: one click from here, a retention pass starts
 * uploading real backups to a place nothing has ever touched, and the
 * first thing to find out that the region is wrong or that the policy
 * denies PutObject would be a move, mid-cycle, after a backup had already
 * been chosen to leave local disk.
 *
 * It is deliberately NOT a gate. The button is offered, never required,
 * and a failing preflight does not block the save: an operator who is
 * about to fix the bucket, or who knows something this check does not, is
 * not served by a form that refuses. What it does is make the answer
 * available before the consequence, which is the whole difference between
 * finding out today and finding out in a month.
 */
export function MediumPreflightPanel({
  mediumIds,
  disabled
}: {
  mediumIds: string[];
  disabled: boolean;
}) {
  if (mediumIds.length === 0) return null;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
      {mediumIds.map((id) => (
        <MediumPreflightRow key={id} mediumId={id} disabled={disabled} />
      ))}
    </div>
  );
}

function MediumPreflightRow({ mediumId, disabled }: { mediumId: string; disabled: boolean }) {
  const api = useApi();
  const [busy, setBusy] = useState(false);
  const [report, setReport] = useState<MediumPreflight | null>(null);
  const [error, setError] = useState<string | null>(null);

  function run() {
    setBusy(true);
    setReport(null);
    setError(null);
    api
      .preflightStorageMedium(mediumId)
      .then(setReport)
      .catch((e: unknown) =>
        setError(
          e instanceof BackupManagerError
            ? e.api.message
            : "Backup Manager could not check this storage medium."
        )
      )
      .finally(() => setBusy(false));
  }

  // Every check is rendered, passed ones included, and that is the point
  // rather than noise: an operator reading "ready" needs to see WHICH
  // things were established, because the one that matters to them may be
  // the delete or the storage class rather than the write.
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <div style={{ display: "flex", gap: 9, alignItems: "center", flexWrap: "wrap" }}>
        <button
          className="btn btn--sm"
          type="button"
          disabled={disabled || busy}
          onClick={run}
        >
          {busy ? "Checking..." : "Check " + mediumId + " now"}
        </button>
        {report ? (
          <span style={{ fontSize: "var(--text-sm)", fontWeight: 600 }}>
            {report.ok
              ? "This medium is ready for a backup."
              : "This medium is not ready. Saving is still allowed; the checks below say why."}
          </span>
        ) : null}
      </div>
      {error ? (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--danger)" }}>{error}</p>
      ) : null}
      {report ? (
        <ul style={{ margin: 0, paddingLeft: 20, fontSize: "var(--text-sm)" }}>
          {report.checks.map((c) => (
            <li key={c.step}>
              <span className="mono">{c.step}</span>
              {": "}
              <strong>{c.outcome}</strong>
              {c.category ? " (" + c.category + ")" : ""}
              {". "}
              {c.detail}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
