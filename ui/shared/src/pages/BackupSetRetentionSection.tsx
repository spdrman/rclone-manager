import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { ErrorState } from "@shared/components/EmptyState";
import { WarningBanner } from "@shared/components/WarningBanner";
import { describeFailure } from "@shared/api/failure";
import { RetentionPolicyEditor } from "@shared/pages/RetentionPolicyCard";
import type { AppSettings, BackupSetRetention, RetentionSettings } from "@shared/api/contracts";

/**
 * Issue #333, the UI half.
 *
 * Retention was global only: every backup set was retained under the one
 * top-level policy, and an operator who wanted a database dump kept on a
 * different chain from a media share had to run a second deployment. The
 * config layer landed with #362, the CLI and the API with the rest of
 * this change, and only now is there anything for this to draw. #299's
 * rule is exactly that: a field nothing reads must not be drawn, and this
 * one is the field that rule was written about, since the wizard used to
 * show per-set retention controls that saved nowhere.
 *
 * # Whose policy, said in both cases
 *
 * The provenance line is there whether the set overrides or not, which is
 * a deliberate difference from `retention --dry-run`'s preview line
 * (which marks an override and stays silent otherwise). A preview is a
 * long list where a marker on the exception is the readable choice; this
 * is one answer about one set, and saying nothing in the common case
 * would leave a reader unable to tell "inherits, and here is the chain"
 * from "overrides with a chain that happens to match".
 *
 * # Whole-policy, so one Save
 *
 * The rest of edit mode is per-box, and each box's Save writes only that
 * box (#350). This section deliberately is not, and the reason is the
 * domain rather than the effort: an override names a WHOLE chain or it
 * does not exist. A per-tier Save would be claiming a scope the config
 * layer refuses to have, because half a chain one level down resolves its
 * missing half to the product default rather than to the deployment's
 * policy, which silently shortens retention.
 *
 * # Declaring an override writes the deployment's own chain first
 *
 * "Give this set its own policy" saves immediately, seeded from the
 * deployment's resolved chain, rather than opening an unsaved editor.
 * Two reasons. The editor's Save is armed by "differs from what was
 * loaded", so a form seeded with the deployment's chain and left alone
 * would have nothing to press. And a set that declares a chain identical
 * to the deployment's is a real, meaningful state: it says "this set is
 * pinned here", and the next edit to the deployment's policy will not
 * move it. That is the state an operator asking for this actually wants
 * as their starting point.
 */
export function BackupSetRetentionSection({
  source,
  setName,
  editing,
  readOnly
}: {
  source: string;
  setName: string;
  /** Whether the page is in issue #350's inline edit mode. */
  editing: boolean;
  readOnly: boolean;
}) {
  const api = useApi();
  const retention = useAsync<BackupSetRetention>(
    () => api.getBackupSetRetention(source, setName),
    [api, source, setName]
  );
  // The schema the editor builds its pickers and bounds from, served
  // beside the deployment's settings rather than written out here, for
  // the reason RetentionPolicyCard's own defaultChain records: a second
  // copy of the product's rules is free to drift, and drifting in the
  // retention direction shortens a window.
  const settings = useAsync<AppSettings>(() => api.getSettings(), [api]);

  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  // What the editor's own save just persisted, so the summary above it
  // stops describing the policy this section loaded. It is held here
  // rather than solved by reloading, because a reload changes the key the
  // editor is mounted under, which would remount it mid-edit and take
  // away the confirmation it had just shown. Cleared by the two actions
  // that DO reload, since after those the loaded value is authoritative
  // again.
  const [saved, setSaved] = useState<RetentionSettings | null>(null);

  if (retention.error) {
    return (
      <ErrorState
        message={retention.error.message}
        remediation="This backup set's retention policy could not be read, so it is not shown."
        correlationId={retention.error.correlationId}
        onRetry={retention.reload}
      />
    );
  }
  if (!retention.data) {
    return <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Loading the retention policy…</p>;
  }
  const current = retention.data;

  const run = (work: Promise<unknown>) => {
    setBusy(true);
    setActionError(null);
    setSaved(null);
    work
      .then(() => retention.reload())
      .catch((e: unknown) => {
        setActionError(
          describeFailure(e, "Backup Manager could not change this backup set's retention policy.").message
        );
      })
      .finally(() => setBusy(false));
  };

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "78ch" }}>
        {current.isOverride
          ? "This backup set is retained under its own policy. Editing the deployment's policy does not move it."
          : "This backup set is retained under the deployment's policy, along with every other set that does not declare its own."}
      </p>

      <PolicySummary policy={saved ?? current.policy} />

      {actionError ? (
        <WarningBanner tone="warn" title="Could not change this set's retention policy">
          {actionError}
        </WarningBanner>
      ) : null}

      {editing && current.isOverride ? (
        <>
          {/* The whole editor, not a second one: same validation, same
              schema-driven pickers, same confirmation in front of turning
              FR-19 protection off. Keyed on the chain so a reload
              re-initialises its draft rather than synchronising into it. */}
          {settings.data ? (
            <RetentionPolicyEditor
              key={policyKey(current.policy)}
              loaded={current.policy}
              schema={settings.data.schema.retention}
              readOnly={readOnly || busy}
              saver={{
                write: (policy: RetentionSettings) =>
                  api
                    .setBackupSetRetention(source, setName, {
                      tiers: policy.tiers,
                      timezone: policy.timezone,
                      weekStartsOn: policy.weekStartsOn,
                      protectLastKnownGood: policy.protectLastKnownGood
                    })
                    .then((r) => {
                      setSaved(r.policy);
                      return r.policy;
                    }),
                intro: (
                  <>
                    This set&rsquo;s own chain. It is a whole policy rather than a change to the
                    deployment&rsquo;s, so it is saved in one piece: half a chain would resolve
                    its missing half to the product default rather than to the deployment&rsquo;s
                    policy, which shortens retention without saying so.
                  </>
                ),
                saveLabel: "Save this set's retention policy",
                savedNote: (
                  <>
                    Saved. This backup set is now retained under this chain, and editing the
                    deployment&rsquo;s policy will not move it.
                  </>
                )
              }}
            />
          ) : null}
          <div>
            <button
              className="btn"
              type="button"
              disabled={readOnly || busy}
              onClick={() => run(api.clearBackupSetRetention(source, setName))}
            >
              Use the deployment&rsquo;s policy
            </button>
            <p style={{ margin: "8px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
              Removes this set&rsquo;s own chain. Nothing is deleted by the change itself; from
              then on this set is retained under whatever the deployment&rsquo;s policy says,
              including later edits to it.
            </p>
          </div>
        </>
      ) : null}

      {editing && !current.isOverride ? (
        <div>
          <button
            className="btn"
            type="button"
            disabled={readOnly || busy}
            onClick={() =>
              run(
                api.setBackupSetRetention(source, setName, {
                  tiers: current.deploymentPolicy.tiers,
                  timezone: current.deploymentPolicy.timezone,
                  weekStartsOn: current.deploymentPolicy.weekStartsOn,
                  protectLastKnownGood: current.deploymentPolicy.protectLastKnownGood
                })
              )
            }
          >
            Give this set its own policy
          </button>
          <p style={{ margin: "8px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
            Starts from the chain above, so nothing about what is retained changes today. From
            then on this set keeps that chain even when the deployment&rsquo;s policy is edited,
            and you can change it here.
          </p>
        </div>
      ) : null}
    </div>
  );
}

/** The chain as it actually decides, read-only. Every tier in chain
 *  order, because that order is what will decide where a kept artifact
 *  lives once storage mediums act on it, so a summary that reordered or
 *  elided tiers would stop being the chain. */
function PolicySummary({ policy }: { policy: RetentionSettings }) {
  return (
    <dl
      style={{
        margin: 0, display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))",
        gap: "15px 18px", fontSize: 13
      }}
    >
      <div>
        <dt className="eyebrow" style={{ fontSize: 10.5, letterSpacing: "0.06em" }}>Chain</dt>
        <dd style={{ margin: "4px 0 0" }}>
          <ul style={{ margin: 0, paddingLeft: 16 }}>
            {policy.tiers.map((t) => (
              <li key={t.name}>
                {t.name} &middot; keep {t.keep} {t.windowUnit || t.granularity}
                {t.granularity === "days" && t.periodDays ? " of " + t.periodDays + " days" : ""}
                {t.medium ? " on " + t.medium : ""}
              </li>
            ))}
          </ul>
        </dd>
      </div>
      <div>
        <dt className="eyebrow" style={{ fontSize: 10.5, letterSpacing: "0.06em" }}>Reckoned in</dt>
        <dd style={{ margin: "4px 0 0" }}>
          {policy.timezone}, weeks from {policy.weekStartsOn}
        </dd>
      </div>
      <div>
        <dt className="eyebrow" style={{ fontSize: 10.5, letterSpacing: "0.06em" }}>
          Last-known-good protection
        </dt>
        <dd style={{ margin: "4px 0 0" }}>{policy.protectLastKnownGood ? "On" : "Off"}</dd>
      </div>
    </dl>
  );
}

/** A value that changes whenever the resolved policy does, so the editor
 *  remounts with a fresh draft after a save or a clear rather than
 *  holding one initialised from a policy that is no longer in force. */
function policyKey(p: RetentionSettings): string {
  return [
    p.timezone,
    p.weekStartsOn,
    String(p.protectLastKnownGood),
    ...p.tiers.map((t) => [t.name, t.granularity, t.keep, t.periodDays ?? "", t.windowUnit ?? "", t.medium ?? ""].join(":"))
  ].join("|");
}
