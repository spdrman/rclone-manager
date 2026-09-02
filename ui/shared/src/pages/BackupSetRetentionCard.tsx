import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type {
  ApiError,
  AppSettings,
  BackupSetRetention,
  RetentionOverride,
  RetentionSchema,
  RetentionSettings
} from "@shared/api/contracts";
import { useAsync } from "@shared/hooks/useAsync";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { ErrorState } from "@shared/components/EmptyState";
import { WarningBanner } from "@shared/components/WarningBanner";
import { isNotConfigured } from "@shared/api/failure";
import {
  chainKey,
  granularityLabel,
  tierErrors,
  toDraft,
  toTierSetting,
  TierRow,
  WEEKDAYS
} from "./retentionChain";
import type { TierDraft } from "./retentionChain";

/**
 * Issue #333: which retention policy this backup set is retained under,
 * and the three operations that change that answer.
 *
 * # It says which policy is in force, on both branches
 *
 * The whole reason this card exists is that "why is this backup about to
 * be deleted" has a different answer, and a different place to go and
 * change it, depending on whether the deployment's chain or this set's
 * own decided it. So the attribution is stated for an inheriting set as
 * plainly as for an overriding one, and an overriding set is shown the
 * deployment's chain beside its own, because that is the one thing an
 * operator cannot work out from this page alone and it is exactly what
 * they would be going back to.
 *
 * # It cannot submit half a policy
 *
 * A set-level policy names the WHOLE chain, and half of one is refused by
 * the server because completing the missing half from the product
 * defaults is how a set silently ends up retaining less than the operator
 * who wrote the deployment's policy believes (see
 * config.resolveBackupSetRetention). This form cannot reach that state at
 * all: creating an override starts from the deployment's own resolved
 * chain, every edit is on top of a whole chain, and the last tier's
 * Remove is disabled so the chain can never be emptied. The server still
 * refuses independently; this is a second line, not the only one.
 *
 * # What it deliberately leaves inheriting
 *
 * The timezone, the week start and FR-19's protection decide how ANY
 * chain is reckoned rather than what the chain says, and an override that
 * omits them inherits the deployment's. That is one checkbox rather than
 * three tri-state controls, because it is one decision at the config
 * layer too, and because the alternative teaches an operator that a blank
 * timezone box means UTC. It does not: it means the deployment's, and the
 * label says which one that currently is.
 *
 * # What replaces what
 *
 * This is a whole-policy write (PUT), never a merge. A set that spelled
 * its chain with the legacy three scalars therefore comes back from a
 * save spelled as a tiers list. The resulting policy is identical, which
 * is why that is acceptable: the chain those three scalars stand for is
 * exactly the chain this form was rendering. The Save button is disabled
 * until something actually changes, so nothing is rewritten by opening
 * the editor and closing it.
 */
export function BackupSetRetentionCard({
  source,
  set,
  readOnly,
  onPreview
}: {
  source: string;
  set: string;
  readOnly: boolean;
  onPreview(): void;
}) {
  const api = useApi();
  const retention = useAsync<BackupSetRetention>(
    () => api.getBackupSetRetention(source, set),
    [api, source, set]
  );
  // The schema comes from the settings response, which is where the
  // server already serves it: the granularity list, the tier-name
  // pattern, the reserved name and both ceilings are the rules
  // core/internal/config actually applies, and a second copy in this file
  // would be free to go stale exactly where a stale copy narrows a
  // retention window.
  const settings = useAsync<AppSettings>(() => api.getSettings(), [api]);

  // #275: retention is part of a configuration this instance does not
  // have. That is a step not taken, not a read that failed, and a red
  // banner with a Try again button would be both wrong.
  if (isNotConfigured(retention.error) || isNotConfigured(settings.error))
    return (
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
        Retention is part of the configuration this instance has not been given yet.
      </p>
    );
  const failed = retention.error ?? settings.error;
  if (failed)
    return (
      <ErrorState
        message={failed.message}
        remediation="This backup set's retention policy could not be read, so it cannot be changed here yet."
        correlationId={failed.correlationId}
        onRetry={retention.error ? retention.reload : settings.reload}
      />
    );
  if (!retention.data || !settings.data)
    return (
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
        Loading this backup set&rsquo;s retention policy…
      </p>
    );

  return (
    // Keyed remount on a reload, so the panel's own state is initialised
    // from freshly loaded values rather than synchronised into a panel
    // that is already open, exactly as RetentionPolicyCard does for the
    // deployment's own policy.
    <RetentionPanel
      key={retention.data.backupSetId + ":" + String(retention.data.isOverride) + ":" + chainKey(retention.data.effective.tiers)}
      loaded={retention.data}
      schema={settings.data.schema.retention}
      source={source}
      set={set}
      readOnly={readOnly}
      onPreview={onPreview}
    />
  );
}

function RetentionPanel({
  loaded,
  schema,
  source,
  set,
  readOnly,
  onPreview
}: {
  loaded: BackupSetRetention;
  schema: RetentionSchema;
  source: string;
  set: string;
  readOnly: boolean;
  onPreview(): void;
}) {
  const api = useApi();
  // The answer the server most recently gave. Held here rather than
  // re-fetched after every write, because each of the three operations
  // ANSWERS with the state that is now deciding: re-reading would be a
  // second observation of something that could have moved in between,
  // and rendering the request back would be worse still.
  const [r, setR] = useState(loaded);
  const [editing, setEditing] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);

  function apply(action: () => Promise<BackupSetRetention>, done: () => void) {
    setBusy(true);
    setError(null);
    action()
      .then((next) => {
        // Re-baseline against what the server says is now deciding, never
        // against the draft: a chain it resolved and a calendar it
        // inherited are part of the answer, and the next comparison has
        // to be made against those.
        setR(next);
        done();
      })
      .catch((e: unknown) => {
        setError(
          e instanceof BackupManagerError
            ? e.api
            : {
                code: "unknown",
                message: "Backup Manager could not change this backup set's retention policy.",
                correlationId: "unavailable"
              }
        );
      })
      .finally(() => setBusy(false));
  }

  if (editing)
    return (
      <RetentionOverrideEditor
        // Remounted whenever the persisted answer changes, so the draft is
        // initialised from freshly loaded values rather than synchronised
        // into an editor that is already open.
        key={chainKey(r.effective.tiers) + ":" + String(r.isOverride)}
        current={r}
        schema={schema}
        busy={busy}
        error={error}
        onCancel={() => {
          setError(null);
          setEditing(false);
        }}
        onSave={(policy) =>
          apply(() => api.setBackupSetRetention(source, set, policy), () => setEditing(false))
        }
      />
    );

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 11, fontSize: 13 }}>
      <div
        className={r.isOverride ? "banner" : "banner banner--ok"}
        style={{ fontSize: "var(--text-sm)" }}
      >
        <span>
          {r.isOverride
            ? "Retained under this backup set's own policy. Editing the deployment's retention policy will not change it."
            : "Retained under the deployment's retention policy. Editing that policy changes this set too."}
        </span>
      </div>

      <PolicyChain policy={r.effective} />

      {r.isOverride ? (
        <details>
          <summary style={{ cursor: "pointer", color: "var(--text-2)" }}>
            The deployment's policy, which this set would go back to
          </summary>
          <div style={{ marginTop: 9 }}>
            <PolicyChain policy={r.deployment} />
          </div>
        </details>
      ) : null}

      {error ? (
        <div className="banner banner--danger" style={{ fontSize: "var(--text-sm)" }} role="alert">
          <span>{error.message}</span>
        </div>
      ) : null}

      <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
        <button className="btn btn--sm" disabled={readOnly || busy} onClick={() => setEditing(true)}>
          {r.isOverride ? "Edit this set's policy" : "Give this set its own policy"}
        </button>
        {r.isOverride ? (
          <button
            className="btn btn--sm btn--caution"
            disabled={readOnly || busy}
            onClick={() => setClearing(true)}
          >
            Return to the deployment's policy
          </button>
        ) : null}
      </div>

      <button className="btn btn--caution" disabled={readOnly} onClick={onPreview}>
        Preview retention plan
      </button>

      <ConfirmationDialog
        open={clearing}
        destructive
        eyebrow="Retention policy"
        title="Return this set to the deployment's policy"
        confirmLabel="Return to the deployment's policy"
        onCancel={() => setClearing(false)}
        onConfirm={() => {
          setClearing(false);
          apply(() => api.clearBackupSetRetention(source, set), () => undefined);
        }}
      >
        <p style={{ margin: 0 }}>
          This backup set will be retained under the deployment's policy again, and the chain it
          declares now is removed rather than kept as a copy.
        </p>
        {/* The direction that is not obvious, and the reason this
            confirmation exists at all. Where a set's own chain is WIDER
            than the deployment's, going back retains LESS, and the
            operator has to see both chains before deciding rather than
            afterwards in a preview. */}
        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          <div>
            <div className="eyebrow" style={{ fontSize: 10.5 }}>This set&rsquo;s own policy, now</div>
            <PolicyChain policy={r.effective} />
          </div>
          <div>
            <div className="eyebrow" style={{ fontSize: 10.5 }}>The deployment&rsquo;s policy, after</div>
            <PolicyChain policy={r.deployment} />
          </div>
        </div>
        <p style={{ margin: 0, color: "var(--text-2)" }}>
          Nothing is deleted by this change on its own. If the deployment&rsquo;s policy keeps less
          than this set&rsquo;s own does, it widens what a later retention apply may delete, and
          every one of those still shows a preview you have to approve.
        </p>
      </ConfirmationDialog>
    </div>
  );
}

/** One resolved policy, rendered the way `backup-manager backup-set
 *  retention` prints it: the calendar, then the chain, in chain order. */
function PolicyChain({ policy }: { policy: RetentionSettings }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 4 }}>
        {policy.tiers.map((t) => (
          <li key={t.name} style={{ display: "flex", justifyContent: "space-between", gap: 12 }}>
            <span className="mono">{t.name}</span>
            <span style={{ color: "var(--text-2)" }}>
              {"keep " + t.keep + " " + granularityLabel(t.windowUnit ?? t.granularity).toLowerCase() +
                (t.keep === 1 ? "" : "s") +
                " of " + granularityLabel(t.granularity).toLowerCase() +
                (t.granularity === "days" && t.periodDays ? " (" + t.periodDays + ")" : "") +
                " buckets"}
            </span>
          </li>
        ))}
      </ul>
      <div style={{ color: "var(--text-2)" }}>
        {policy.timezone + " · weeks start " + policy.weekStartsOn + " · " +
          (policy.protectLastKnownGood
            ? "newest known-good backup protected"
            : "newest known-good backup NOT protected")}
      </div>
    </div>
  );
}

/**
 * The override editor. It always submits a WHOLE policy, which is what
 * the endpoint's PUT means and what stops this surface producing half a
 * chain.
 *
 * The starting chain is the deployment's own when there is no override
 * yet. That is the honest pre-fill: an operator giving a set its own
 * policy is starting from what the set is retained under right now, and a
 * form that started empty would make the first save a chain nobody chose.
 */
function RetentionOverrideEditor({
  current,
  schema,
  busy,
  error,
  onCancel,
  onSave
}: {
  current: BackupSetRetention;
  schema: RetentionSchema;
  busy: boolean;
  error: ApiError | null;
  onCancel(): void;
  onSave(policy: RetentionOverride): void;
}) {
  const [tiers, setTiers] = useState<TierDraft[]>(() => current.effective.tiers.map(toDraft));
  // "Inherit the calendar" is the state an override with no timezone,
  // week start or protection key is in. A set that is not overriding yet
  // starts there, because inheriting is what it is doing right now and
  // pinning the calendar is a separate decision from pinning the chain.
  const declared = current.override;
  const [inheritCalendar, setInheritCalendar] = useState(
    () =>
      declared === undefined ||
      (declared.timezone === undefined &&
        declared.weekStartsOn === undefined &&
        declared.protectLastKnownGood === undefined)
  );
  const [timezone, setTimezone] = useState(current.effective.timezone);
  const [weekStartsOn, setWeekStartsOn] = useState(current.effective.weekStartsOn);
  const [protect, setProtect] = useState(current.effective.protectLastKnownGood);

  const namePattern = new RegExp(schema.tierNamePattern);
  const errors = tiers.map((t, i) => tierErrors(t, i, tiers, schema, namePattern));
  const invalid = errors.some((e) => Object.keys(e).length > 0);

  const baselineInherits =
    declared === undefined ||
    (declared.timezone === undefined &&
      declared.weekStartsOn === undefined &&
      declared.protectLastKnownGood === undefined);
  const dirty =
    !current.isOverride ||
    chainKey(tiers.map(toTierSetting)) !== chainKey(current.effective.tiers) ||
    inheritCalendar !== baselineInherits ||
    (!inheritCalendar &&
      (timezone !== current.effective.timezone ||
        weekStartsOn !== current.effective.weekStartsOn ||
        protect !== current.effective.protectLastKnownGood));

  function submit() {
    const policy: RetentionOverride = { tiers: tiers.map(toTierSetting) };
    if (!inheritCalendar) {
      policy.timezone = timezone;
      policy.weekStartsOn = weekStartsOn;
      policy.protectLastKnownGood = protect;
    }
    onSave(policy);
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 14, fontSize: 13 }}>
      <WarningBanner tone="warn">
        A policy written here replaces the deployment&rsquo;s whole chain for this backup set. Once
        it exists, editing the deployment&rsquo;s retention policy no longer changes this set.
      </WarningBanner>

      <label style={{ display: "flex", gap: 9, alignItems: "flex-start" }}>
        <input
          type="checkbox"
          checked={inheritCalendar}
          disabled={busy}
          onChange={(e) => setInheritCalendar(e.target.checked)}
        />
        <span>
          Reckon this policy in the deployment&rsquo;s calendar and follow its last-known-good
          protection
          <span style={{ display: "block", color: "var(--text-2)" }}>
            {"Currently " + current.deployment.timezone + ", weeks start " +
              current.deployment.weekStartsOn + ", newest known-good backup " +
              (current.deployment.protectLastKnownGood ? "protected" : "not protected") +
              ". Leaving this on means a later change to the deployment's calendar moves this set too."}
          </span>
        </span>
      </label>

      {inheritCalendar ? null : (
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(180px, 1fr))", gap: "12px 16px" }}>
          <label style={{ display: "flex", flexDirection: "column", gap: 5 }}>
            <span className="eyebrow" style={{ fontSize: 10.5 }}>Timezone</span>
            <input
              className="input input--mono"
              value={timezone}
              disabled={busy}
              onChange={(e) => setTimezone(e.target.value)}
            />
          </label>
          <label style={{ display: "flex", flexDirection: "column", gap: 5 }}>
            <span className="eyebrow" style={{ fontSize: 10.5 }}>Week starts on</span>
            <select
              className="select"
              value={weekStartsOn}
              disabled={busy}
              onChange={(e) => setWeekStartsOn(e.target.value)}
            >
              {WEEKDAYS.map((d) => (
                <option key={d} value={d}>
                  {d.charAt(0).toUpperCase() + d.slice(1)}
                </option>
              ))}
            </select>
          </label>
          <label style={{ display: "flex", gap: 9, alignItems: "center" }}>
            <input
              type="checkbox"
              checked={protect}
              disabled={busy}
              onChange={(e) => setProtect(e.target.checked)}
            />
            <span>Protect the newest known-good backup</span>
          </label>
        </div>
      )}

      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        {tiers.map((t, i) => (
          <TierRow
            key={t.key}
            index={i}
            tier={t}
            schema={schema}
            errors={errors[i]}
            readOnly={busy}
            // There is no way to spell "keep nothing" in this schema at
            // all: an empty chain reinstates the default daily/weekly/
            // monthly policy rather than disabling retention, so the last
            // tier cannot be removed. The server refuses an emptied chain
            // too, so this is a second line rather than the only one.
            canRemove={tiers.length > 1}
            onChange={(patch) => setTiers((cur) => cur.map((x, n) => (n === i ? { ...x, ...patch } : x)))}
            onRemove={() => setTiers((cur) => cur.filter((_, n) => n !== i))}
          />
        ))}
        <div>
          <button
            className="btn btn--sm"
            type="button"
            disabled={busy}
            onClick={() =>
              setTiers((cur) => [
                ...cur,
                toDraft({ name: "", granularity: schema.granularities[0] ?? "day", keep: 1 })
              ])
            }
          >
            Add tier
          </button>
        </div>
      </div>

      {error ? (
        <div className="banner banner--danger" style={{ fontSize: "var(--text-sm)" }} role="alert">
          <span>{error.message}</span>
        </div>
      ) : null}

      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn btn--primary" disabled={busy || invalid || !dirty} onClick={submit}>
          {busy ? "Saving…" : "Save this set's policy"}
        </button>
        <button className="btn" disabled={busy} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}
