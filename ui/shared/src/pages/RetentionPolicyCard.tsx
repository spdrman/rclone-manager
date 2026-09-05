import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type {
  ApiError,
  AppSettings,
  RetentionSchema,
  RetentionSettings,
  StorageMedium,
  StorageSchema,
  UpdateRetentionSettings
} from "@shared/api/contracts";
import { useAsync } from "@shared/hooks/useAsync";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { FieldHelp, HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import { ErrorState } from "@shared/components/EmptyState";
import { isNotConfigured } from "@shared/api/failure";
import { WarningBanner } from "@shared/components/WarningBanner";
import {
  chainKey,
  defaultChain,
  introducedMediumMappings,
  MediumDisclosure,
  settingsKey,
  tierErrors,
  toDraft,
  toTierSetting,
  TierRow,
  WEEKDAYS
} from "./retentionChain";
import type { TierDraft } from "./retentionChain";

/**
 * B3.7 (#140) — the retention policy form, the write half of what #111
 * shipped for the config file and the CLI.
 *
 * # It targets the chain, not the three scalars
 *
 * Issue #156 generalized FR-18 from a hardcoded daily/weekly/monthly
 * triple to an administrator-configured ordered chain, and this form
 * edits that chain. The old scalars still exist in the schema as sugar
 * for the default chain, but they are never rendered and never written:
 * the backend reports the RESOLVED chain either way (see
 * core/service.RetentionSettings), so one layout serves both spellings of
 * one policy, and the request omits `tiers` entirely unless the operator
 * actually edited the chain, which is what keeps a legacy file in its own
 * spelling when somebody only flips a toggle.
 *
 * # Every rule it enforces comes from the server
 *
 * The granularity list, the window-unit list, the tier-name pattern, the
 * reserved name, both ceilings and the default chain arrive in the same
 * response as the values (`schema` on AppSettings). Nothing in this file
 * hardcodes a closed value set, so a granularity added to
 * core/internal/config reaches this picker without a second copy here
 * going stale. The default chain joined that list late (PR #171's
 * mandatory finding M5): it was the one value the form did spell for
 * itself, and it is the one where a stale copy writes a policy rather
 * than merely displaying one. Client-side
 * validation is a courtesy that keeps a doomed request off the wire; the
 * server validates the whole config regardless, and its refusal is what
 * is displayed if the two ever disagree.
 *
 * # There is no way to spell "keep nothing"
 *
 * An empty chain reinstates the default 7/3/12 policy rather than
 * disabling retention (config.Retention.Tiers' own doc), so the form
 * refuses to reach that state at all: the last tier's Remove control is
 * disabled, the copy says what emptying would actually mean, and
 * "Restore default chain" is the positive affordance for the operator who
 * wanted the default back. The backend refuses an explicitly emptied
 * chain too, so this is a second line, not the only one.
 *
 * # Where a tier's backups live, and the consent in front of it (#240)
 *
 * Each tier row carries a storage-medium picker when the configuration
 * declares a medium (retentionChain's TierRow), and none at all when it
 * declares none, which is FR-35's compatibility case: a deployment that
 * never heard of storage mediums gets exactly the form it had. The first
 * save that sends a tier's backups off this machine is FR-27's consent
 * moment. The server refuses that write without an acknowledgment and
 * its refusal carries the disclosure text, so the panel this card shows
 * (MediumDisclosure, shared with the per-set editor) renders the
 * backend's own words, and disabling Save until the box is ticked is a
 * courtesy in front of a gate that holds either way.
 */
export function RetentionPolicyCard({ readOnly }: { readOnly: boolean }) {
  const api = useApi();
  const settings = useAsync<AppSettings>(() => api.getSettings(), [api]);

  return (
    <section className="card">
      <div className="card__header">
        <h2 className="eyebrow">Retention policy</h2>
      </div>
      <div className="card__body">
        {isNotConfigured(settings.error) ? (
          // #275: retention is part of a configuration this instance does
          // not have. That is a step not taken, not a read that failed, and
          // a red banner with a Try again button would be both wrong.
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
            Retention is part of the configuration this instance has not been given yet.
            It becomes editable once the first backup set has been added.
          </p>
        ) : settings.error ? (
          <ErrorState
            message={settings.error.message}
            remediation="The retention policy could not be read, so it cannot be edited here yet."
            correlationId={settings.error.correlationId}
            onRetry={settings.reload}
          />
        ) : settings.data ? (
          // Keyed remount on a reload, so the editor's own draft state is
          // initialised from freshly loaded settings rather than
          // synchronised into it afterwards.
          <RetentionPolicyEditor
            key={settingsKey(settings.data.retention)}
            loaded={settings.data.retention}
            schema={settings.data.schema.retention}
            mediums={settings.data.mediums}
            storage={settings.data.schema.storage}
            readOnly={readOnly}
          />
        ) : (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
            Loading the retention policy…
          </p>
        )}
      </div>
    </section>
  );
}





function RetentionPolicyEditor({
  loaded,
  schema,
  mediums,
  storage,
  readOnly
}: {
  loaded: RetentionSettings;
  schema: RetentionSchema;
  /** Every storage medium the configuration declares. EMPTY is the
   *  ordinary case and the compatibility one: with no medium configured
   *  there is nowhere else for a tier's backups to go, so the picker is
   *  not rendered at all and the form is exactly the form it was. */
  mediums: StorageMedium[];
  storage: StorageSchema;
  readOnly: boolean;
}) {
  const api = useApi();

  // Purely local form state: nothing outside this card reads a
  // half-edited policy, so this stays plain useState rather than going
  // through the shared causl graph (state/appNodes.ts is for state two
  // components have to agree on).
  const [baseline, setBaseline] = useState<RetentionSettings>(loaded);
  const [timezone, setTimezone] = useState(loaded.timezone);
  const [weekStartsOn, setWeekStartsOn] = useState(loaded.weekStartsOn);
  const [protect, setProtect] = useState(loaded.protectLastKnownGood);
  const [tiers, setTiers] = useState<TierDraft[]>(() => loaded.tiers.map(toDraft));

  // The operator's acknowledgment of the storage-medium disclosure
  // (FR-27). It is reset by any edit that changes which mappings are new,
  // so a tick given for one mapping cannot be spent on a different one.
  const [acknowledged, setAcknowledged] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [saveError, setSaveError] = useState<ApiError | null>(null);
  const [saved, setSaved] = useState(false);

  const namePattern = new RegExp(schema.tierNamePattern);
  const errors = tiers.map((t, i) => tierErrors(t, i, tiers, schema, namePattern));
  const invalid = errors.some((e) => Object.keys(e).length > 0);

  const chainEdited = chainKey(tiers.map(toTierSetting)) !== chainKey(baseline.tiers);
  const dirty =
    chainEdited ||
    timezone !== baseline.timezone ||
    weekStartsOn !== baseline.weekStartsOn ||
    protect !== baseline.protectLastKnownGood;

  // The one dangerous direction: FR-19 protection is on in the saved
  // config and this save would turn it off. core/internal/retention calls
  // that "a materially more dangerous configuration"
  // (LastKnownGoodDecide), and #111's acceptance criteria require the
  // operator see that before it takes effect — hence a confirmation in
  // front of the write, not a notice after it.
  const disablingProtection = baseline.protectLastKnownGood && !protect;

  // Which tiers this save would newly send off local disk (FR-27's
  // consent). Per tier, and matched by NAME against what the server says
  // is currently in effect, which is the same rule core/service applies:
  // a configuration that already sends monthly to a medium has consented
  // to monthly leaving, and to nothing else.
  //
  // This decides what the form SHOWS. It does not decide whether the
  // write is allowed: the server refuses an unacknowledged write with
  // MEDIUM_DISCLOSURE_REQUIRED whatever this computes, which is what
  // makes the disabled button below a courtesy rather than the gate.
  const introduced = chainEdited ? introducedMediumMappings(tiers.map(toTierSetting), baseline.tiers) : [];
  const needsDisclosure = introduced.length > 0;

  function update(i: number, patch: Partial<TierDraft>) {
    setSaved(false);
    // Any edit to the chain can change WHICH mappings are new, so a tick
    // given a moment ago was given for a different set of consequences.
    // Clearing it is the safe direction and costs one click.
    if (patch.medium !== undefined || patch.name !== undefined) setAcknowledged(false);
    setTiers((current) => current.map((t, n) => (n === i ? { ...t, ...patch } : t)));
  }

  function submit() {
    const retention: UpdateRetentionSettings = {};
    // Only what actually changed is sent. An absent key means "leave this
    // alone" to the endpoint, and for `tiers` in particular that is what
    // stops a config written with the legacy scalars being rewritten into
    // the general spelling by a save that never touched the chain.
    if (timezone !== baseline.timezone) retention.timezone = timezone;
    if (weekStartsOn !== baseline.weekStartsOn) retention.weekStartsOn = weekStartsOn;
    if (protect !== baseline.protectLastKnownGood) retention.protectLastKnownGood = protect;
    if (chainEdited) retention.tiers = tiers.map(toTierSetting);

    setBusy(true);
    setSaveError(null);
    setSaved(false);
    api
      .updateSettings(
        needsDisclosure ? { retention, acknowledgeMediumDisclosure: true } : { retention }
      )
      .then((next) => {
        // Re-baseline against what the server says is now running, not
        // against the draft: defaults it resolved and values it
        // canonicalised are part of the answer, and the next "has
        // anything changed" comparison has to be made against those.
        setBaseline(next.retention);
        setTimezone(next.retention.timezone);
        setWeekStartsOn(next.retention.weekStartsOn);
        setProtect(next.retention.protectLastKnownGood);
        setTiers(next.retention.tiers.map(toDraft));
        setAcknowledged(false);
        setSaved(true);
      })
      .catch((e: unknown) => {
        setSaveError(
          e instanceof BackupManagerError
            ? e.api
            : {
                code: "unknown",
                message: "Backup Manager could not save the retention policy.",
                correlationId: "unavailable"
              }
        );
      })
      .finally(() => setBusy(false));
  }

  function onSave() {
    if (readOnly || invalid || !dirty || busy) return;
    if (needsDisclosure && !acknowledged) return;
    if (disablingProtection) {
      setConfirming(true);
      return;
    }
    submit();
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "78ch" }}>
        One policy for every backup set. Each tier groups backups into calendar buckets and keeps
        the newest good backup in each one; a backup kept by any tier is kept.
      </p>

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(196px, 1fr))", gap: "15px 18px" }}>
        <HelpField label="Timezone" help={FIELD_HELP.retentionTimezone}>
          {(helpId) => (
            <input
              className="input input--mono"
              aria-describedby={helpId}
              value={timezone}
              disabled={readOnly}
              onChange={(e) => {
                setSaved(false);
                setTimezone(e.target.value);
              }}
            />
          )}
        </HelpField>
        <HelpField label="Week starts on" help={FIELD_HELP.weekStartsOn}>
          {(helpId) => (
            <select
              className="select"
              aria-describedby={helpId}
              value={weekStartsOn}
              disabled={readOnly}
              onChange={(e) => {
                setSaved(false);
                setWeekStartsOn(e.target.value);
              }}
            >
              {WEEKDAYS.map((d) => (
                <option key={d} value={d}>
                  {d.charAt(0).toUpperCase() + d.slice(1)}
                </option>
              ))}
            </select>
          )}
        </HelpField>
      </div>

      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        {tiers.map((t, i) => (
          <TierRow
            key={t.key}
            index={i}
            tier={t}
            schema={schema}
            mediums={mediums}
            errors={errors[i]}
            readOnly={readOnly}
            canRemove={tiers.length > 1}
            onChange={(patch) => update(i, patch)}
            onRemove={() => {
              setSaved(false);
              setTiers((current) => current.filter((_, n) => n !== i));
            }}
          />
        ))}
      </div>

      <div style={{ display: "flex", gap: 9, flexWrap: "wrap" }}>
        <button
          className="btn btn--sm"
          type="button"
          disabled={readOnly}
          onClick={() => {
            setSaved(false);
            setTiers((current) => [
              ...current,
              toDraft({ name: "", granularity: "day", keep: 1 })
            ]);
          }}
        >
          Add tier
        </button>
        <button
          className="btn btn--sm"
          type="button"
          disabled={readOnly}
          onClick={() => {
            setSaved(false);
            setTiers(defaultChain(schema));
          }}
        >
          Restore default chain
        </button>
      </div>

      <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)", maxWidth: "78ch" }}>
        Removing every tier is not how retention is switched off: an empty chain reinstates the
        default daily/weekly/monthly policy. Retention stops only by not running a retention pass
        at all.
      </p>

      <FieldHelp
        label="Protect the newest known-good backup"
        help={FIELD_HELP.protectLastKnownGood}
      >
        {(helpId) => (
          <label
            style={{
              display: "flex", alignItems: "center", gap: 10, padding: "11px 13px",
              border: "1px solid " + (protect ? "var(--border)" : "var(--danger)"),
              borderRadius: 7, fontSize: 13, cursor: readOnly ? "default" : "pointer"
            }}
          >
            <input
              type="checkbox"
              aria-describedby={helpId}
              checked={protect}
              disabled={readOnly}
              style={{ accentColor: "var(--accent)" }}
              onChange={(e) => {
                setSaved(false);
                setProtect(e.target.checked);
              }}
            />
            <span style={{ flex: 1 }}>
              Protect the newest known-good backup from retention (FR-19)
            </span>
          </label>
        )}
      </FieldHelp>

      {!protect ? (
        <WarningBanner tone="danger" title="Last-known-good protection is off">
          With this off, the newest known-good backup can be deleted by a retention pass purely
          because of its age. Backup Manager treats that as a materially more dangerous
          configuration.
        </WarningBanner>
      ) : null}

      {needsDisclosure ? (
        <MediumDisclosure
          introduced={introduced}
          mediums={mediums}
          storage={storage}
          acknowledged={acknowledged}
          disabled={readOnly}
          onChange={(next) => {
            setSaved(false);
            setAcknowledged(next);
          }}
        />
      ) : null}

      {saveError ? (
        <ErrorState
          message={saveError.message}
          remediation="Nothing was saved. The policy on disk is unchanged."
          correlationId={saveError.correlationId}
        />
      ) : null}

      {saved ? (
        <div className="banner banner--ok" style={{ fontSize: "var(--text-sm)" }}>
          <span aria-hidden="true" style={{ color: "var(--ok)" }}>{"✓"}</span>
          <span>
            Retention policy saved. It is in effect now, with no restart. Saving rewrites the
            server&rsquo;s configuration file, which does not preserve comments in it.
          </span>
        </div>
      ) : null}

      <div>
        <button
          className="btn btn--primary"
          type="button"
          style={{ height: 40 }}
          disabled={readOnly || invalid || !dirty || busy || (needsDisclosure && !acknowledged)}
          onClick={onSave}
        >
          {busy ? "Saving…" : "Save retention policy"}
        </button>
      </div>

      <ConfirmationDialog
        open={confirming}
        destructive
        eyebrow="Safety setting"
        title="Disable last-known-good protection"
        confirmLabel="Disable protection"
        onCancel={() => setConfirming(false)}
        onConfirm={() => {
          setConfirming(false);
          submit();
        }}
      >
        <p style={{ margin: 0 }}>
          Last-known-good protection is what stops a retention pass deleting the newest backup
          this system has actually verified. Turning it off is a materially more dangerous
          configuration: after this, age alone can select that backup for deletion.
        </p>
        <p style={{ margin: 0, color: "var(--text-2)" }}>
          Nothing is deleted by this change on its own. It widens what a later retention apply may
          delete, and every one of those still shows a preview you have to approve.
        </p>
      </ConfirmationDialog>
    </div>
  );
}



