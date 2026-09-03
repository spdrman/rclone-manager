import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { RetentionPreviewDialog } from "@shared/pages/RetentionPreviewDialog";
import { ApiProvider } from "@shared/api/ApiContext";
import { BackupManagerError } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { resetGraphForTests } from "@shared/state/graph";
import { commitRetentionRevisions } from "@shared/state/appNodes";
import type { RetentionPlan } from "@shared/types/backup";

/** A live plan: expires_at is ten minutes out from whenever this suite
 *  runs, not a frozen literal that quietly falls into the past and turns
 *  every apply here into an expiry refusal (the dialog now enforces
 *  expiry — see handleApply). The expired case gets its own fixture. */
const EXPIRES_AT = new Date(Date.now() + 10 * 60 * 1000).toISOString();

const PLAN: RetentionPlan = {
  planId: "retplan_test_1",
  backupSetId: "production/postgres-primary",
  inventoryRevision: "inv_1",
  configRevision: "cfg_1",
  expiresAt: EXPIRES_AT,
  keepCount: 1,
  deleteCount: 1,
  // Issue #333: which policy decided these verdicts. Deliberately the
  // set's OWN policy, and a chain that is not the product default, so a
  // dialog that lost the attribution or fell back to a hardcoded chain
  // shows something visibly wrong rather than something plausible.
  retention: {
    timezone: "Europe/Berlin",
    weekStartsOn: "monday",
    protectLastKnownGood: true,
    tiers: [{ name: "daily", granularity: "day", keep: 4 }]
  },
  retentionIsOverride: true,
  reclaimBytes: 2048,
  verdicts: [
    { artifact: "a.dump", action: "KEEP", reason: "GFS daily tier", tiers: [{ tier: "DAILY", selectedBy: "BOTH" }, { tier: "LAST_KNOWN_GOOD", selectedBy: "PROTECTION" }] },
    { artifact: "refused.dump", action: "REFUSE", reason: "sibling-prefix directory at computed path", tiers: [] },
    { artifact: "b.dump", action: "DELETE", reason: "Not selected by current retention policy", tiers: [] }
  ]
};

/** RetentionVerdict.tiers is an open set: FR-18's chain is operator-defined
 *  (core/internal/config's Retention.Tiers), so any lower_snake_case tier
 *  name a config file spells arrives here upper-cased. This plan holds one
 *  artifact kept by a tier no build of this UI has ever heard of, alongside
 *  one kept with no tier at all. */
const OPEN_TIER_PLAN: RetentionPlan = {
  ...PLAN,
  keepCount: 2,
  deleteCount: 0,
  reclaimBytes: 0,
  verdicts: [
    { artifact: "half-year.dump", action: "KEEP", reason: "GFS semi-annual tier", tiers: [{ tier: "SEMI_ANNUAL", selectedBy: "DISCOVERY" }] },
    { artifact: "no-tier.dump", action: "KEEP", reason: "", tiers: [] }
  ]
};

/** Issue #218. Since #215 a tier can have selected an artifact through
 *  either of FR-18's two placements: the timestamp this manager
 *  discovered it, or the producer's own timestamp on the remote object.
 *  FR-8 trusts one and not the other, so the confirm-before-delete dialog
 *  has to say which, per tier. This plan holds the case a single
 *  per-verdict attribution cannot express: one artifact whose DAILY came
 *  from the discovery pass and whose MONTHLY came from the producer pass.
 *  LAST_KNOWN_GOOD is FR-19's protection rather than a bucket selection,
 *  so it carries no placement at all. */
const MIXED_PLACEMENT_PLAN: RetentionPlan = {
  ...PLAN,
  keepCount: 1,
  deleteCount: 0,
  reclaimBytes: 0,
  verdicts: [
    {
      artifact: "backdated.dump",
      action: "KEEP",
      reason: "kept by the DAILY and MONTHLY tiers",
      tiers: [
        { tier: "DAILY", selectedBy: "DISCOVERY" },
        { tier: "MONTHLY", selectedBy: "PRODUCER" },
        { tier: "LAST_KNOWN_GOOD", selectedBy: "PROTECTION" }
      ]
    },
    {
      artifact: "half-year.dump",
      action: "KEEP",
      reason: "GFS semi-annual tier",
      tiers: [{ tier: "SEMI_ANNUAL", selectedBy: "BOTH" }]
    }
  ]
};

function apiWith(overrides: Partial<ReturnType<typeof createMockApi>>) {
  return { ...createMockApi(), previewRetention: () => Promise.resolve(PLAN), ...overrides };
}

/** B3.1 (#96). RetentionPlan carries no `stale` field (types/backup.ts) —
 *  these tests are the issue's own required proof that the dialog asserts
 *  staleness from the graph's own evidence (state/appNodes.ts's
 *  retentionPlanStaleNode), not from a boolean the wire hands over. */
describe("RetentionPreviewDialog", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("renders the plan's KEEP/REFUSE/DELETE verdicts, REFUSE styled calmly rather than as an error", async () => {
    const api = apiWith({});
    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);

    expect(screen.getByText("a.dump")).toBeTruthy();
    expect(screen.getByText("b.dump")).toBeTruthy();
    expect(screen.getByText("refused.dump")).toBeTruthy();
    expect(screen.getByText(/sibling-prefix directory/)).toBeTruthy();
    // KEEP's LAST_KNOWN_GOOD tier renders as the "Protected" badge.
    expect(screen.getByText("Protected")).toBeTruthy();
    // The refuse row is not an alert — it is the plan working as intended.
    const refuseRow = screen.getByText(/sibling-prefix directory/).closest("li");
    expect(refuseRow?.getAttribute("role")).not.toBe("alert");
  });

  // Issue #333: which policy produced these verdicts. This is the dialog
  // that asks an operator to authorise a deletion, and "why is this
  // backup on the delete list" has a different answer, and a different
  // place to go and change it, depending on whether this set's own chain
  // or the deployment's decided it.
  //
  // Both branches are driven, because a dialog that had hardcoded either
  // sentence would pass a test that only checked the other one.
  it("names the policy the verdicts were decided under, on both branches", async () => {
    const own = apiWith({});
    const { unmount } = render(
      <ApiProvider api={own}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );
    await screen.findByText(/Decided under this backup set's own retention policy/);
    // And the chain itself, not only the attribution: "this set's own"
    // beside the wrong chain is still wrong.
    expect(screen.getByText(/daily 4/)).toBeTruthy();
    unmount();
    resetGraphForTests();

    const inherited = apiWith({
      previewRetention: () =>
        Promise.resolve({ ...PLAN, retentionIsOverride: false })
    });
    render(
      <ApiProvider api={inherited}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );
    await screen.findByText(/Decided under the deployment's retention policy/);
  });

  it("badges a KEEP verdict from an operator-defined tier under its own name, never as \"unclassified\"", async () => {
    const api = apiWith({ previewRetention: () => Promise.resolve(OPEN_TIER_PLAN) });
    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);

    // "unclassified" is the DELETE condition's own wording ("no tier claims
    // this"), and the Keep row carries no reason text to contradict it, so
    // rendering it here would tell the operator the opposite of what the
    // backend decided — on the screen whose next button deletes files.
    const kept = screen.getByText("half-year.dump").closest("li");
    expect(kept?.textContent).toContain("Semi Annual (discovery)");
    expect(kept?.textContent).not.toContain("unclassified");

    // The control: a verdict with a genuinely empty tier list still says
    // so, which is what makes the assertion above a measurement.
    const untiered = screen.getByText("no-tier.dump").closest("li");
    expect(untiered?.textContent).toContain("unclassified");
  });

  it("says which of the two placements selected each tier, per tier and not per verdict", async () => {
    const api = apiWith({ previewRetention: () => Promise.resolve(MIXED_PLACEMENT_PLAN) });
    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);

    // One artifact, two tiers, two different placements. Reading this row
    // is the whole of issue #218: without it an operator cannot tell a
    // KEEP this manager's own clock produced from one an untrusted
    // producer timestamp produced.
    const kept = screen.getByText("backdated.dump").closest("li");
    expect(kept?.textContent).toContain("Daily (discovery)");
    expect(kept?.textContent).toContain("Monthly (producer)");
    // FR-19's protection is not a placement, so it is not dressed as one.
    expect(kept?.textContent).toContain("Protected");
    expect(kept?.textContent).not.toContain("Protected (");

    // A tier this build has never heard of still carries its placement.
    const openTier = screen.getByText("half-year.dump").closest("li");
    expect(openTier?.textContent).toContain("Semi Annual (both)");
  });

  it("disables Continue the moment the graph learns of an inventory change, from that commit alone — before any apply request reaches the API", async () => {
    const applyRetention = vi.fn(() => Promise.reject(new Error("must not be called")));
    const api = apiWith({ applyRetention });

    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);
    const continueBtn = screen.getByRole("button", { name: "Continue…" });
    expect(continueBtn).toBeEnabled();
    expect(screen.queryByText("Retention preview changed")).toBeNull();

    // GIVEN plan P was previewed, WHEN the backup set's inventory changes:
    // simulated here as a direct commit to the graph, standing in for
    // whatever later learns of the real change — the derived node is what
    // is under test, not any particular producer of the commit.
    act(() => {
      commitRetentionRevisions({ inventoryRevision: "inv_2", configRevision: PLAN.configRevision });
    });

    expect(continueBtn).toBeDisabled();
    expect(screen.getByText("Retention preview changed")).toBeTruthy();

    // Clicking a disabled button is a no-op in the DOM, but the real
    // guarantee under test is that the dialog never even tries.
    expect(applyRetention).not.toHaveBeenCalled();
  });

  it("stays enabled across a config-revision-only match and only disables when a revision genuinely diverges", async () => {
    const api = apiWith({});
    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);

    act(() => {
      // Same revisions the plan itself carries — re-affirms the baseline,
      // must not be mistaken for staleness.
      commitRetentionRevisions({ inventoryRevision: PLAN.inventoryRevision, configRevision: PLAN.configRevision });
    });

    expect(screen.getByRole("button", { name: "Continue…" })).toBeEnabled();
  });

  it("applying the exact reviewed plan_id closes the dialog on success", async () => {
    const onClose = vi.fn();
    const applyRetention = vi.fn(() => Promise.resolve({ ...PLAN, operationId: "op_test_1" }));
    const api = apiWith({ applyRetention });

    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={onClose} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);
    fireEvent.click(screen.getByRole("button", { name: "Continue…" }));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Delete \d+ backups$/ }));
    });

    expect(applyRetention).toHaveBeenCalledWith("production", "postgres-primary", PLAN.planId);
    expect(onClose).toHaveBeenCalled();
  });

  it("a RETENTION_PLAN_STALE apply rejection hides the plan detail and offers to review a fresh one, instead of silently failing", async () => {
    const applyRetention = vi.fn(() =>
      Promise.reject(
        new BackupManagerError({
          // The literal code apps/common/webhost/handlers_retention.go
          // writes and handlers_retention_test.go asserts on
          // (TestApplyRetention_StalePlanReturns409WithItsOwnCode), not a
          // spelling that exists only in this frontend: the dialog's stale
          // branch is the one place in the whole UI that reads an error
          // code, and it used to compare against a value no backend has
          // ever sent (issue #96's review, mandatory finding M2).
          code: "RETENTION_PLAN_STALE",
          message: "The backup inventory changed after this preview was created.",
          remediation: "No files were deleted. Review the updated retention plan before continuing.",
          correlationId: "cid_test_stale"
        })
      )
    );
    const api = apiWith({ applyRetention });

    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);
    fireEvent.click(screen.getByRole("button", { name: "Continue…" }));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Delete \d+ backups$/ }));
    });

    expect(await screen.findByText(/No files were deleted/)).toBeTruthy();
    // Plan detail (the verdict lists) is hidden until a fresh plan is
    // reviewed — never left showing a now-rejected plan as if still live.
    expect(screen.queryByText("a.dump")).toBeNull();
    expect(screen.getByRole("button", { name: "Continue…" })).toBeDisabled();
  });

  it("renders no plan, and never applies one, while the resource still holds another backup set's plan", async () => {
    const applyRetention = vi.fn(() => Promise.reject(new Error("must not be called")));
    // The dialog is mounted for `billing`, but the shared retention
    // resource node still holds the plan previewed for `postgres-primary`
    // — exactly what a preview of one set followed by opening retention
    // for another leaves behind while the second request is in flight.
    const api = apiWith({ applyRetention, previewRetention: () => Promise.resolve(PLAN) });

    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="billing" open onClose={() => {}} />
      </ApiProvider>
    );

    expect(await screen.findByText(/Requesting plan/)).toBeTruthy();
    // None of the other set's detail is on screen under this set's identity.
    expect(screen.queryByText(/Plan retplan_test_1/)).toBeNull();
    expect(screen.queryByText("a.dump")).toBeNull();
    expect(screen.queryByText("b.dump")).toBeNull();
    expect(screen.getByRole("button", { name: "Continue…" })).toBeDisabled();
    expect(applyRetention).not.toHaveBeenCalled();
  });

  it("renders and enables the same plan once it does name this backup set (positive control)", async () => {
    const api = apiWith({
      previewRetention: () => Promise.resolve({ ...PLAN, backupSetId: "production/billing" })
    });

    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="billing" open onClose={() => {}} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);
    expect(screen.getByText("b.dump")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Continue…" })).toBeEnabled();
  });

  it("does not apply when the graph learns of a change while the confirmation is already open", async () => {
    const applyRetention = vi.fn(() => Promise.resolve(PLAN));
    const api = apiWith({ applyRetention });

    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);
    // The confirmation opens while the plan is still fresh, which is what
    // makes this the window the Continue-button gate does not cover.
    fireEvent.click(screen.getByRole("button", { name: "Continue…" }));
    act(() => {
      commitRetentionRevisions({ inventoryRevision: "inv_2", configRevision: PLAN.configRevision });
    });

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Delete \d+ backups$/ }));
    });

    expect(applyRetention).not.toHaveBeenCalled();
    expect(await screen.findByText(/No files were deleted/)).toBeTruthy();
  });

  it("does not apply a plan that expired while the confirmation was open", async () => {
    const applyRetention = vi.fn(() => Promise.resolve(PLAN));
    const api = apiWith({
      applyRetention,
      previewRetention: () =>
        Promise.resolve({ ...PLAN, expiresAt: new Date(Date.now() - 1000).toISOString() })
    });

    render(
      <ApiProvider api={api}>
        <RetentionPreviewDialog source="production" set="postgres-primary" open onClose={() => {}} />
      </ApiProvider>
    );

    await screen.findByText(/Plan retplan_test_1/);
    fireEvent.click(screen.getByRole("button", { name: "Continue…" }));
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: /^Delete \d+ backups$/ }));
    });

    expect(applyRetention).not.toHaveBeenCalled();
    expect(await screen.findByText(/No files were deleted/)).toBeTruthy();
  });
});
