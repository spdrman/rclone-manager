/**
 * What retention is about to do, shown in full before any of it happens.
 *
 * The plan is issued by the server and immutable. This dialog never
 * recomputes it, never filters it and never applies a subset: an operator
 * confirms the exact deletion set they were shown, or nothing happens.
 * That is why staleness matters enough to have a mechanism of its own,
 * and why a stale plan disables apply rather than quietly fetching a
 * fresh one, which would mean confirming a list nobody read.
 *
 * Everything shown here is per artifact, with the tiers that kept it and
 * what selected it for each. A summary count would be smaller and would
 * remove the only thing that lets an operator notice that the one backup
 * they care about is on the wrong side of the line.
 */
import { useEffect, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import type { ApiError } from "@shared/api/contracts";
import {
  commitRetentionRevisions,
  retentionPlanNode,
  retentionPlanStaleNode
} from "@shared/state/appNodes";
import { useCausl } from "@shared/state/graph";
import { useResource } from "@shared/state/resource";
import type { RetentionPlan, RetentionVerdict } from "@shared/types/backup";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { WarningBanner } from "@shared/components/WarningBanner";
import { RetentionTierBadges } from "@shared/components/RetentionBadge";
import { bytes } from "@shared/utilities/format";

function describeApplyError(e: unknown): ApiError {
  return e instanceof BackupManagerError
    ? e.api
    : {
        code: "unknown",
        message: "Backup Manager could not complete that request.",
        correlationId: "unavailable"
      };
}

/** §17 — the plan is server-issued and immutable. If it goes stale we refuse to
 *  apply and never silently recalculate a different deletion set.
 *
 * `source`/`set` are BackupSetID's own two-part identity (core/internal/
 * model/ids.go), matching apps/common/webhost/router.go's
 * `/backup-sets/{source}/{set}/retention/...` routes — see BackupSet.source/
 * BackupSet.set's own doc (types/backup.ts). */
export function RetentionPreviewDialog({
  source,
  set,
  open,
  onClose
}: {
  source: string;
  set: string;
  open: boolean;
  onClose(): void;
}) {
  const api = useApi();
  const [confirming, setConfirming] = useState(false);
  // Local and ephemeral, exactly like `confirming` above (issue #96's own
  // "State management" section) — a failed apply's error is this ONE
  // dialog session's concern, not shared graph state.
  const [applyError, setApplyError] = useState<ApiError | null>(null);

  const plan = useResource<RetentionPlan>(
    retentionPlanNode,
    () => (open ? api.previewRetention(source, set) : Promise.resolve(null as never)),
    [api, source, set, open]
  );
  // The graph's own evidence, not a field trusted off the wire — see
  // retentionPlanStaleNode's own doc (state/appNodes.ts).
  const stale = useCausl(retentionPlanStaleNode);

  // retentionPlanNode is one process-wide node, and fetchResource
  // deliberately keeps the previous `data` visible while a new fetch is in
  // flight (state/resource.ts). Both are right for a list that should not
  // flicker, and both are wrong for a destructive confirmation: previewing
  // set A and then opening retention for set B would otherwise render A's
  // plan id, A's counts and A's verdicts under B's identity, with Continue
  // live for the whole round trip. So a plan is this dialog's plan only
  // while nothing is in flight AND it names this dialog's own backup set
  // — the plan carries backupSetId, so the evidence to refuse is in hand
  // (issue #96's review, mandatory finding M3). The cost is that the
  // dialog shows its empty state on every open instead of stale content,
  // which is the right trade here.
  const settled = plan.loading ? null : plan.data;
  const planForThisSet = settled && settled.backupSetId === source + "/" + set ? settled : null;

  const planId = planForThisSet?.planId;
  useEffect(() => {
    // A freshly read plan is, by definition, not stale against itself: seed
    // the graph's own "current revisions" baseline to match it — syncing
    // local component state with the external causl graph is exactly what
    // an effect is for. Anything that later moves retentionRevisionsNode
    // away from this baseline (a re-preview, or — future wiring — a live
    // poll/push) is what retentionPlanStaleNode actually asserts on.
    if (planForThisSet) {
      commitRetentionRevisions({
        inventoryRevision: planForThisSet.inventoryRevision,
        configRevision: planForThisSet.configRevision
      });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [planId]);

  // applyError needs to reset whenever a genuinely new plan arrives (a
  // fresh "Review new plan" or dialog reopen), but this is adjusting local
  // state from a prop-derived value, not synchronizing with an external
  // system — react.dev's own "storing information from previous renders"
  // pattern: setState during render, never inside an effect, so it cannot
  // cascade an extra render.
  const [applyErrorForPlanId, setApplyErrorForPlanId] = useState<string | undefined>(planId);
  if (planId !== applyErrorForPlanId) {
    setApplyErrorForPlanId(planId);
    if (applyError) setApplyError(null);
  }

  if (!open) return null;

  // While an apply attempt was refused, the plan detail is hidden until the
  // operator reviews a fresh one — the canvas's State 4. Continue is
  // disabled by `!p` alone; no separate flag needed.
  const p: RetentionPlan | null = applyError ? null : planForThisSet;

  const keepVerdicts: RetentionVerdict[] = p?.verdicts.filter((v) => v.action === "KEEP") ?? [];
  const refuseVerdicts: RetentionVerdict[] = p?.verdicts.filter((v) => v.action === "REFUSE") ?? [];
  const deleteVerdicts: RetentionVerdict[] = p?.verdicts.filter((v) => v.action === "DELETE") ?? [];
  const lastKnownGood = keepVerdicts.find((v) => v.tiers.some((t) => t.tier === "LAST_KNOWN_GOOD"));

  function handleApply() {
    // The gate above only ever disabled the Continue button. Once the
    // confirmation is open, this handler is the last thing between the
    // operator and a deletion, and it is exactly the window where the
    // evidence is most likely to move under it — so both refusals are
    // re-checked here rather than trusted from render time (issue #96's
    // review, mandatory finding M7). The server refuses both cases too;
    // this is what turns that refusal into a sentence the operator can act
    // on instead of a request that should never have been sent.
    if (!p) return;
    if (stale || Date.parse(p.expiresAt) <= Date.now()) {
      setConfirming(false);
      setApplyError({
        code: "RETENTION_PLAN_STALE",
        message: "This retention plan is no longer current.",
        remediation:
          "No files were deleted. Review the updated retention plan before continuing.",
        correlationId: "unavailable"
      });
      return;
    }
    api
      .applyRetention(source, set, p.planId)
      .then(() => {
        setConfirming(false);
        onClose();
      })
      .catch((e: unknown) => {
        setConfirming(false);
        setApplyError(describeApplyError(e));
      });
  }

  return (
    <>
      <div className="dialog-scrim" onClick={(e) => e.target === e.currentTarget && onClose()}>
        <div
          role="dialog"
          aria-modal="true"
          aria-label="Retention preview"
          className="dialog"
          style={{ maxWidth: 620, maxHeight: "84vh", overflow: "auto" }}
        >
          <div style={{ padding: "18px 22px", borderBottom: "1px solid var(--border)" }}>
            <h2>Retention preview</h2>
            <p style={{ margin: "4px 0 0", fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
              {applyError
                ? "Plan rejected"
                : p
                  ? "Plan " + p.planId + " · issued by the backup service"
                  : "Requesting plan…"}
            </p>
          </div>

          {applyError ? (
            <div style={{ padding: "16px 22px 0" }}>
              <WarningBanner
                tone="danger"
                title={
                  applyError.code === "RETENTION_PLAN_STALE"
                    ? "Retention plan rejected — nothing was deleted"
                    : "Could not apply retention"
                }
                actions={
                  applyError.code === "RETENTION_PLAN_STALE" ? (
                    <button
                      className="btn btn--sm"
                      onClick={() => {
                        setApplyError(null);
                        plan.reload();
                      }}
                    >
                      Review new plan
                    </button>
                  ) : undefined
                }
              >
                {applyError.remediation ?? applyError.message}
              </WarningBanner>
            </div>
          ) : stale ? (
            <div style={{ padding: "16px 22px 0" }}>
              <WarningBanner
                tone="warn"
                title="Retention preview changed"
                actions={
                  <button className="btn btn--sm" onClick={plan.reload}>Review new plan</button>
                }
              >
                The backup inventory changed after this preview was created. No files
                were deleted. Review the updated retention plan before continuing.
              </WarningBanner>
            </div>
          ) : null}

          {p ? (
            <>
              <div
                style={{
                  margin: "18px 22px 0", display: "grid", gridTemplateColumns: "repeat(3, 1fr)",
                  gap: 1, background: "var(--border)", border: "1px solid var(--border)",
                  borderRadius: "var(--radius-lg)", overflow: "hidden"
                }}
              >
                <Stat label="Keep" value={String(p.keepCount)} />
                <Stat label="Delete" value={String(p.deleteCount)} tone="var(--danger)" />
                <Stat label="Reclaim" value={bytes(p.reclaimBytes)} />
              </div>

              {/* Issue #333: which policy produced these verdicts, and
                  what that policy says.
                  "Why is this backup about to be deleted" has a different
                  answer, and a different place to go and change it,
                  depending on whether this set's own chain or the
                  deployment's decided it, and this is the dialog that
                  asks an operator to authorise a deletion. It is read
                  off the plan rather than fetched beside it: a plan is
                  pinned to the configuration revision it was computed
                  against, so a separately-fetched policy could describe
                  a chain that did not decide the list underneath it. */}
              <p
                style={{ margin: "12px 22px 0", fontSize: "var(--text-sm)", color: "var(--text-2)" }}
              >
                {(p.retentionIsOverride
                  ? "Decided under this backup set's own retention policy: "
                  : "Decided under the deployment's retention policy: ") +
                  p.retention.tiers.map((t) => t.name + " " + t.keep).join(", ") +
                  " · " + p.retention.timezone}
              </p>

              <div style={{ padding: "18px 22px", display: "flex", flexDirection: "column", gap: 16 }}>
                <div>
                  <div className="eyebrow" style={{ color: "var(--ok)", marginBottom: 8 }}>
                    {"Keep · " + keepVerdicts.length}
                  </div>
                  <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 6 }}>
                    {keepVerdicts.map((v) => (
                      <li key={v.artifact} style={{ display: "flex", justifyContent: "space-between", gap: 12, fontSize: "var(--text-sm)" }}>
                        <span className="mono">{v.artifact}</span>
                        <RetentionTierBadges tiers={v.tiers} />
                      </li>
                    ))}
                  </ul>
                </div>

                {refuseVerdicts.length > 0 ? (
                  <div>
                    {/* Deliberately the calmest tone of the three (§96 design
                        pass): a refused delete is the plan working correctly,
                        a candidate policy did not select AND that failed an
                        FR-20 safety check — not an error to alarm over. */}
                    <div className="eyebrow" style={{ color: "var(--text-2)", marginBottom: 8 }}>
                      {"Refuse · " + refuseVerdicts.length}
                    </div>
                    <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 6 }}>
                      {refuseVerdicts.map((v) => (
                        <li key={v.artifact} className="banner banner--info" style={{ padding: "8px 10px", fontSize: "var(--text-sm)" }}>
                          <span aria-hidden="true" style={{ color: "var(--text-3)" }}>i</span>
                          <span>
                            <span className="mono">{v.artifact}</span>
                            <span style={{ color: "var(--text-2)" }}>{" — " + v.reason}</span>
                          </span>
                        </li>
                      ))}
                    </ul>
                  </div>
                ) : null}

                <div>
                  <div className="eyebrow" style={{ color: "var(--danger)", marginBottom: 8 }}>
                    {"Delete · " + deleteVerdicts.length}
                  </div>
                  <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 6 }}>
                    {deleteVerdicts.map((v) => (
                      <li key={v.artifact} style={{ display: "flex", justifyContent: "space-between", gap: 12, fontSize: "var(--text-sm)" }}>
                        <span className="mono">{v.artifact}</span>
                        <span style={{ color: "var(--text-2)" }}>{v.reason}</span>
                      </li>
                    ))}
                  </ul>
                </div>
              </div>
            </>
          ) : null}

          <div className="card__footer" style={{ display: "flex", justifyContent: "flex-end", gap: 9, borderRadius: "0 0 10px 10px" }}>
            <button className="btn" onClick={onClose}>Cancel</button>
            <button
              className="btn btn--destructive"
              disabled={!p || stale || p.deleteCount === 0}
              onClick={() => setConfirming(true)}
            >
              Continue…
            </button>
          </div>
        </div>
      </div>

      {p ? (
        <ConfirmationDialog
          open={confirming}
          destructive
          eyebrow="Destructive action"
          title="Apply retention"
          confirmLabel={"Delete " + p.deleteCount + " backups"}
          onCancel={() => setConfirming(false)}
          onConfirm={handleApply}
        >
          <p style={{ margin: 0 }}>
            {p.deleteCount + " retained backup files will be permanently removed from NAS storage."}
          </p>
          <p style={{ margin: 0, color: "var(--text-2)" }}>
            {bytes(p.reclaimBytes) + " will be reclaimed. This applies exactly plan " + p.planId + " — it will not be recalculated."}
          </p>
          {lastKnownGood ? (
            <div className="banner banner--ok" style={{ fontSize: "var(--text-sm)" }}>
              <span aria-hidden="true" style={{ color: "var(--ok)" }}>✓</span>
              <span>The newest known-good backup is protected.</span>
            </div>
          ) : null}
        </ConfirmationDialog>
      ) : null}
    </>
  );
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div style={{ background: "var(--surface)", padding: "13px 15px" }}>
      <div className="eyebrow" style={{ fontSize: 10.5 }}>{label}</div>
      <div style={{ marginTop: 5, fontSize: 20, fontWeight: 600, color: tone }}>{value}</div>
    </div>
  );
}
