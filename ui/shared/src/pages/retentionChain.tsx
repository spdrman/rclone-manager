import type { ReactNode } from "react";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";
import type {
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
  /** The storage medium this tier names (FR-27), or "" for the local
   *  backup root. Held as the id rather than an index so a medium removed
   *  from the configuration between load and save cannot silently become
   *  a different one.
   *
   *  TierRow edits it when the deployment declares a medium (#240), and
   *  it is held even when it does not, because a chain save REPLACES the
   *  operator's whole chain: a field the draft dropped would be deleted
   *  from their configuration file by the act of changing something
   *  else, which is precisely what service.RetentionTier.Medium's own doc
   *  calls a lossy boundary. Editing daily's keep must not quietly move
   *  monthly's artifacts back onto local disk. */
  medium: string;
}

let nextTierKey = 0;
export function toDraft(t: RetentionTierSetting): TierDraft {
  nextTierKey += 1;
  return {
    key: "tier-" + nextTierKey,
    name: t.name,
    granularity: t.granularity,
    keep: String(t.keep),
    periodDays: t.periodDays ? String(t.periodDays) : "",
    windowUnit: t.windowUnit ?? "",
    medium: t.medium ?? ""
  };
}

export const CUSTOM_PERIOD = "days";

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

export const GRANULARITY_LABELS: Record<string, string> = {
  day: "Day",
  week: "Week",
  month: "Month",
  quarter: "Quarter",
  half_year: "Half year",
  year: "Year",
  days: "Custom period"
};

export function granularityLabel(value: string): string {
  return GRANULARITY_LABELS[value] ?? value;
}

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
  onRemove
}: {
  index: number;
  tier: TierDraft;
  schema: RetentionSchema;
  /** Every storage medium the configuration declares. Empty when it
   *  declares none, which is when the picker below is not rendered at
   *  all (FR-35): a configuration that never heard of storage mediums
   *  gets the row it already had, with no extra control to read past and
   *  no new way to get its policy wrong. */
  mediums: StorageMedium[];
  errors: TierErrors;
  readOnly: boolean;
  canRemove: boolean;
  onChange(patch: Partial<TierDraft>): void;
  onRemove(): void;
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

      {/* The picker exists only where there is somewhere else to put a
          backup. The class is part of the choice, not decoration: one of
          these places cannot be read without a restore, and an operator
          picking blind would find that out hours later, holding a restore
          request they did not know they needed. */}
      {mediums.length > 0 ? (
        <Field label="Stored on" help={FIELD_HELP.tierMedium}>
          {(helpId) => (
            <select
              className="select"
              aria-describedby={helpId}
              aria-label={"Storage medium for tier " + position}
              value={tier.medium}
              disabled={readOnly}
              onChange={(e) => onChange({ medium: e.target.value })}
            >
              <option value="">Local backup root</option>
              {mediums.map((m) => (
                <option key={m.id} value={m.id}>
                  {m.id + " (" + m.storageClass + (m.readsRequireRestore ? ", needs a restore to read" : "") + ")"}
                </option>
              ))}
            </select>
          )}
        </Field>
      ) : null}

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
    // Carried back out unchanged. See TierDraft.medium.
    medium: t.medium ? t.medium : undefined
  };
}

/** A stable string for "is this the same chain", used both to decide
 *  whether to send `tiers` at all and to remount the editor when the
 *  loaded policy changes. Built from a fixed field order, so it never
 *  depends on object key ordering. */
export function chainKey(tiers: RetentionTierSetting[]): string {
  return tiers
    .map((t) => [t.name, t.granularity, t.periodDays ?? 0, t.keep, t.windowUnit ?? "", t.medium ?? ""].join(":"))
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
    if (!t.medium) return false;
    const was = current.find((b) => b.name === t.name);
    return !was || (was.medium ?? "") !== t.medium;
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
    <div
      className="banner banner--warn"
      role="group"
      aria-label="Storage medium disclosure"
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
    </div>
  );
}
