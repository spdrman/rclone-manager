/**
 * Issue #662 in the browser: the two halves of it an operator meets without
 * a terminal, which on a NAS is the only way they meet anything.
 *
 * # Defect 4, the remedy the console refuses
 *
 * The gate refuses a run and the product answers with a command:
 *
 *   [you] [browser] This deployment will not start a backup run.
 *     Destructive operations are disabled until the trusted-proxy
 *     authentication gate has been verified for this deployment, so
 *     retrying will not help. Run rbm fetch --backup-set
 *     cicd-pipeline/var-backups from a shell on this host instead.
 *   $ rbm fetch --backup-set cicd-pipeline/var-backups
 *   [you] post /operations refused: DESTRUCTIVE_OPERATIONS_DISABLED
 *   # no rbm equivalent yet · POST /api/v1/operations
 *
 * The advice is sound; the operator ran that command and it repaired the
 * set. What is broken is that the same window, about the same command,
 * prints "no rbm equivalent yet" and names the route that just refused.
 * One surface says "run this" and "this does not exist" in four lines.
 *
 * The contract pinned is the property, not the wording: a remedy a message
 * offers must be runnable where it is offered, or the surface must not
 * contradict it. Rewording the gap line, dropping it when a remedy is on
 * screen, running fetch locally, or saying "not here -- on the host's
 * shell" in the gap line itself all pass.
 *
 * # Defect 3, on the surface where it hurts most
 *
 * A quarantined backup has three buttons: Revalidate, Retry ingestion,
 * Reinstate. A FAILED one has none, anywhere. `retryFailedIngestion` is
 * declared in the API contract and implemented in the client, and no
 * component calls it. So the browser can show an operator that a set is
 * Failing and cannot offer them a single thing to do about it, which is
 * defect 3 on the one surface the product's own premise says has to work.
 *
 * The quarantined artifact is the positive control in both cases: the same
 * page, the same fixture shape, with the recovery route present.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QuarantinePage } from "@shared/pages/QuarantinePage";
import { BackupDetailPage } from "@shared/pages/BackupDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { dockText } from "@shared/components/ActivityDock";
import type { DockEntry } from "@shared/components/ActivityDock";
import { describeRunRefusal } from "@shared/hooks/useRunControls";
import { resetGraphForTests } from "@shared/state/graph";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { BackupArtifact } from "@shared/types/backup";
import type { SetActivityEvent } from "@shared/types/activity";

const SET_ID = "cicd-pipeline/var-backups";

/** The artifact from the issue: 294 bytes on disk, recorded as the sha256
 *  of nothing (defect 1), and stuck. `quarantine: null` is the state
 *  `retry` leaves it in and the state the browser has no answer for. */
const STUCK: BackupArtifact = {
  id: "art_dpkg_diversions_5",
  setId: SET_ID,
  setName: "var-backups",
  filename: "dpkg.diversions.5.gz",
  remoteOriginalPath: "/var/backups/dpkg.diversions.5.gz",
  localPath: "/data/backups/cicd-pipeline/dpkg.diversions.5.gz",
  producedAt: "2026-09-06T23:00:00Z",
  receivedAt: "2026-09-07T01:18:00Z",
  sizeBytes: 0,
  checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  checksumAlgorithm: "sha256",
  validation: "failed",
  retentionClasses: [],
  retentionPolicy: "configured",
  remoteSourceRemovedAt: null,
  quarantine: null,
  placements: [
    {
      medium: "local",
      mediumType: "local",
      location: "/data/backups/cicd-pipeline/dpkg.diversions.5.gz",
      sizeBytes: 0,
      storageClass: "",
      verificationClass: "content",
      verifiedAt: null,
      access: "immediate",
      status: "ACTIVE"
    }
  ]
};

/** The same artifact one state earlier, before `retry` moved it out of
 *  QUARANTINED. This is the control: here the product does offer verbs. */
const QUARANTINED: BackupArtifact = {
  ...STUCK,
  quarantine: {
    reason: "checksum-mismatch",
    detail:
      "recorded remote size 294 disagrees with recorded transfer size 0",
    detectedAt: "2026-09-07T01:18:48Z",
    remoteSourceRetained: true
  }
};

function event(over: Partial<SetActivityEvent> = {}): SetActivityEvent {
  return {
    sequence: 1,
    at: "2026-09-07T13:03:25Z",
    level: "warn",
    event: "api_action",
    scope: "deployment",
    message: "post /operations refused: DESTRUCTIVE_OPERATIONS_DISABLED",
    fields: {},
    ...over
  };
}

afterEach(() => {
  cleanup();
  resetGraphForTests();
  vi.restoreAllMocks();
});

describe("the remedy a blocked run offers, and the console it is offered in (#662 defect 4)", () => {
  it("does not tell the operator to run a command the same window calls nonexistent", () => {
    // The remediation is taken from the product rather than retyped, so
    // this case cannot pass by agreeing with a copy of the sentence that
    // has drifted from the one an operator sees.
    const refusal = describeRunRefusal("DESTRUCTIVE_OPERATIONS_DISABLED", "", {
      kind: "set",
      id: SET_ID
    });
    const remedy = "rbm fetch --backup-set " + SET_ID;
    expect(refusal.remediation).toContain(remedy);

    // The window as the operator had it: the browser's own refusal line
    // carrying that remedy, and the engine's api_action line for the very
    // POST that was refused.
    const entries: DockEntry[] = [
      {
        kind: "notice",
        notice: {
          id: "n1",
          at: Date.parse("2026-09-07T13:03:25Z"),
          outcome: "refused",
          code: "DESTRUCTIVE_OPERATIONS_DISABLED",
          message: refusal.message,
          remediation: refusal.remediation,
          correlationId: "cid_NrcGcTMo",
          backupSetIds: [SET_ID],
          command: remedy
        }
      },
      {
        kind: "event",
        event: event({
          fields: {
            actor: "rom",
            route: "POST /api/v1/operations",
            command_gap: "no rbm equivalent yet",
            command_gap_detail:
              "`rbm fetch` starts a cycle in your own shell, not in this engine"
          }
        })
      }
    ];

    const text = dockText(entries, "rom");

    // Controls first. Both halves have to be on screen, or the
    // contradiction below is not being measured at all.
    expect(text).toContain(remedy);
    expect(text).toContain("POST /api/v1/operations");

    expect(
      text,
      "#662 defect 4: the docked terminal offers `" +
        remedy +
        "` as the remedy and, four lines later, says the same operation has no rbm equivalent and maps to the " +
        "route that just refused. One window, two contradictory statements about one command. The operator " +
        "reported being told to run the only thing this console cannot do.\n\n" +
        text
    ).not.toContain("no rbm equivalent yet");
  });

  it("control: a refusal that offers no command carries no contradiction to find", () => {
    // The discriminator. Without this, the assertion above could be
    // satisfied by a dock that simply never prints the gap line, and this
    // case would notice.
    const refusal = describeRunRefusal("OPERATION_ALREADY_RUNNING", "", { kind: "all" });
    expect(refusal.message).not.toContain("will not start a backup run");

    const text = dockText(
      [
        {
          kind: "event",
          event: event({
            fields: {
              actor: "rom",
              route: "POST /api/v1/operations",
              command_gap: "no rbm equivalent yet",
              command_gap_detail: "`rbm run` starts a cycle in your own shell, not in this engine"
            }
          })
        }
      ],
      "rom"
    );

    // The gap line is legitimate here and must keep being printed: nothing
    // on screen claimed this route had a command an operator could type.
    expect(text).toContain("# no rbm equivalent yet · POST /api/v1/operations");
    expect(text).not.toContain("rbm fetch --backup-set");
  });
});

describe("what the browser offers an operator whose backup is stuck (#662 defect 3)", () => {
  function renderQuarantine(artifacts: BackupArtifact[], api: BackupManagerApi = createMockApi()) {
    const quarantine: AsyncState<BackupArtifact[]> = {
      data: artifacts,
      error: null,
      loading: false,
      reload: vi.fn()
    };
    return render(
      <ApiProvider api={api}>
        <QuarantinePage readOnly={false} quarantine={quarantine} />
      </ApiProvider>
    );
  }

  it("control: a QUARANTINED backup is offered every recovery verb the product has", () => {
    renderQuarantine([QUARANTINED]);

    const actions = within(screen.getAllByRole("row")[1])
      .getAllByRole("button")
      .map((b) => b.textContent);
    // This is the shape the case below is asking for, proving it is a
    // shape this UI can produce. It also fences the fix: whatever is done
    // for a FAILED artifact must not cost the quarantined one its verbs.
    expect(actions).toContain("Revalidate");
    expect(actions).toContain("Retry ingestion");
    expect(actions).toContain("Reinstate…");
  });

  it("offers a FAILED backup no route to any of them", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getArtifact").mockResolvedValue(STUCK);

    render(
      <MemoryRouter initialEntries={["/backups/" + STUCK.id]}>
        <ApiProvider api={api}>
          <Routes>
            <Route path="/backups/:artifactId" element={<BackupDetailPage />} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );

    // Control: the page rendered this artifact. Without it, "no button"
    // below would be equally true of a page that rendered nothing.
    await screen.findByText(STUCK.filename);

    const labels = screen.getAllByRole("button").map((b) => (b.textContent ?? "").trim());
    const recovery = labels.filter((l) =>
      /retry|reinstate|revalidate|validate/i.test(l)
    );

    expect(
      recovery,
      "#662 defect 3, in the browser: the detail page for a FAILED backup offers no control that reaches any " +
        "recovery verb.\n" +
        "  buttons on the page: " +
        JSON.stringify(labels) +
        "\n" +
        "`retryFailedIngestion` is declared in the API contract and implemented in the client, and nothing in " +
        "src/pages, src/components or src/hooks calls it. The product's own premise is a NAS with no shell, so a " +
        "backup the browser cannot act on is a backup nobody can act on."
    ).not.toHaveLength(0);
  });
});
