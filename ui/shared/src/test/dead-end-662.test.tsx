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
import { act, cleanup, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { App } from "@shared/App";
import { QuarantinePage } from "@shared/pages/QuarantinePage";
import { BackupDetailPage } from "@shared/pages/BackupDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { createMockApi } from "@shared/api/mock";
import { ActivityDock, dockText } from "@shared/components/ActivityDock";
import type { DockEntry } from "@shared/components/ActivityDock";
import { clearBrowserNoticesForTests, emitBrowserNotice } from "@shared/state/browserNotices";
import { describeRunRefusal } from "@shared/hooks/useRunControls";
import { resetGraphForTests } from "@shared/state/graph";
import { BackupManagerError, RequestFailure } from "@shared/api/contracts";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";
import type { BackupArtifact } from "@shared/types/backup";
import type { VersionInfo } from "@shared/types/operation";
import type { DeploymentActivity, LiveActivity, SetActivityEvent } from "@shared/types/activity";

const SET_ID = "cicd-pipeline/var-backups";

/** The artifact from the issue: 294 bytes on disk, recorded as the sha256
 *  of nothing (defect 1), and stuck.
 *
 *  Every field here is what the backend actually produces for this state,
 *  which is the correction that makes the case below mean anything. The
 *  earlier version of this fixture hard-coded `validation: "failed"`, a
 *  value no code path in the product writes for a FAILED artifact:
 *  `state=FAILED` is reached with `ValidationPassed` still nil (nothing on
 *  the collision path records a verdict), and core/service/artifacts.go
 *  maps a nil verdict to "pending". Staging the issue's own sequence
 *  (core/cmd/backup-manager's stage662DeadEnd + drive662ToFailed, whose
 *  own assertion is `stateOf(...) == FAILED`) and reading the journal back
 *  gives exactly the three values below.
 *
 *  It matters because the browser's recovery card was gated on
 *  `validation === "failed" && quarantine === null`, so it rendered for
 *  this fixture and for no real artifact: a fixture that lies passes a
 *  test the product fails. `quarantine: null` is honest and is the half
 *  the Quarantine page cannot see. */
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
  state: "FAILED",
  validation: "pending",
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
  // The state has to move with the record. `quarantine` and `state` are
  // two readings of one journal row, and a fixture where they disagree is
  // a fixture no backend can produce (core/service/artifacts.go sets
  // `quarantined` FROM the state, for QUARANTINED and QUARANTINED_LOST
  // and nothing else).
  state: "QUARANTINED",
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
  // cleanup() first, and it has to be first: clearing the notice ring is a
  // commit, a commit wakes every subscriber, and a dock still mounted
  // would be re-rendered by it after this test's providers have gone
  // (activity-dock.test.tsx learned this the same way).
  cleanup();
  clearBrowserNoticesForTests();
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

  /** A deployment whose feed carries exactly one line: the api_action the
   *  engine logged for the POST that was refused, gap and all. */
  function dockApi(events: SetActivityEvent[]): BackupManagerApi {
    const deployment: DeploymentActivity = {
      events,
      truncated: false,
      dropped: false,
      oldestSequence: events[0].sequence,
      latestSequence: events[events.length - 1].sequence
    };
    const answer: LiveActivity = {
      observedAt: "2026-09-07T13:03:30Z",
      epoch: "one-process",
      pollAfterMs: 10_000,
      sets: [],
      deployment
    };
    return { getLiveActivity: () => Promise.resolve(answer) } as unknown as BackupManagerApi;
  }

  /**
   * The same contradiction, on the panel rather than in the export.
   *
   * `withoutContradictedGaps` is applied twice — inside `dockText`, which
   * is Copy and Save, and on ActivityDock's own window, which is what is
   * DRAWN. Only the first was ever exercised, so reverting the second
   * left the whole suite green while the panel went on printing "no rbm
   * equivalent yet" four lines under a remedy it contradicts. That panel
   * is the surface #662 reported: the operator was reading a window, not
   * a clipboard, and a Copy that quietly disagreed with the screen is the
   * drift this function's own doc says it exists to prevent.
   *
   * The before/after is on ONE mounted panel, so the gap line is proven
   * to be drawn in the first place. A case that only asserted the absence
   * would pass on a dock that never printed gaps at all.
   */
  it("does not print the contradiction on the panel either, which is the window the operator was reading", async () => {
    const refusal = describeRunRefusal("DESTRUCTIVE_OPERATIONS_DISABLED", "", { kind: "set", id: SET_ID });
    const remedy = "rbm fetch --backup-set " + SET_ID;
    const detail = "`rbm fetch` starts a cycle in your own shell, not in this engine";

    render(
      <MemoryRouter>
        <PlatformProvider bridge={genericBridge}>
          <ApiProvider
            api={dockApi([
              event({
                fields: {
                  actor: "rom",
                  route: "POST /api/v1/operations",
                  command_gap: "no rbm equivalent yet",
                  command_gap_detail: detail
                }
              })
            ])}
          >
            <ActivityDock />
          </ApiProvider>
        </PlatformProvider>
      </MemoryRouter>
    );

    // Control, and the whole point of doing this on a live panel: with no
    // remedy on screen the gap line is legitimate and IS drawn.
    expect(await screen.findByText(/no rbm equivalent yet/)).toBeInTheDocument();

    // Now the browser's own refusal arrives, carrying the command. This is
    // the window the issue describes, assembled the way the operator got
    // it: the engine's line was already there and the press produced the
    // remedy.
    act(() => {
      emitBrowserNotice({
        outcome: "refused",
        code: "DESTRUCTIVE_OPERATIONS_DISABLED",
        message: refusal.message,
        remediation: refusal.remediation,
        backupSetIds: [SET_ID],
        command: remedy
      });
    });
    expect(await screen.findByText(new RegExp(remedy.replace(/\//g, "\\/")))).toBeInTheDocument();

    await waitFor(() =>
      expect(
        screen.queryByText(/no rbm equivalent yet/),
        "#662 defect 4, on the surface it was reported from: the panel offers `" +
          remedy +
          "` as the remedy and still prints \"no rbm equivalent yet\" for the same operation, in the same window. " +
          "withoutContradictedGaps is applied to ActivityDock's own window for exactly this, and nothing watched " +
          "that application — only its use inside dockText, which is Copy and Save."
      ).toBeNull()
    );

    // And nothing was hidden: the route is still named and the precise
    // truth is on the line in place of the short claim, which is the half
    // of the fix that makes it a rewrite rather than a suppression.
    const panel = within(screen.getByRole("log"));
    expect(panel.getByText(/POST \/api\/v1\/operations/)).toBeInTheDocument();
    expect(panel.getByText(/not in this engine/)).toBeInTheDocument();
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

  /** The detail page on its own, over a named artifact. The api double is
   *  returned as well as the render, because what this feature IS is a
   *  call: a button that renders and reaches nothing is the defect, not
   *  the fix. */
  function renderDetail(artifact: BackupArtifact) {
    const api = createMockApi();
    const getArtifact = vi.spyOn(api, "getArtifact").mockResolvedValue(artifact);
    const retry = vi.spyOn(api, "retryFailedIngestion").mockResolvedValue(undefined);
    render(
      <MemoryRouter initialEntries={["/backups/" + artifact.id]}>
        <ApiProvider api={api}>
          <Routes>
            <Route path="/backups/:artifactId" element={<BackupDetailPage />} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );
    return { api, getArtifact, retry };
  }

  /**
   * The case #662 is about, asserted as a call rather than as a label.
   *
   * The first version of this test filtered the page's buttons by name
   * and asserted the list was non-empty. That cannot fail for the reason
   * it names: a button wired to the wrong verb, or to nothing, passes it,
   * and so does a card that renders for every artifact in the product. So
   * it is pressed here, and what is asserted is the verb, the artifact it
   * was asked about, the re-read that follows, and the sentence the
   * operator is left with.
   */
  it("presses the FAILED backup's one control and it reaches the retry verb for that backup", async () => {
    const { getArtifact, retry } = renderDetail(STUCK);

    // Control: the page rendered THIS artifact. Without it every assertion
    // below is equally true of a page that rendered nothing.
    await screen.findByText(STUCK.filename);
    const readsBefore = getArtifact.mock.calls.length;

    const button = screen.getByRole("button", { name: /retry ingestion/i });
    await userEvent.click(button);

    expect(
      retry.mock.calls,
      "#662 defect 3, in the browser: the detail page's recovery control does not reach retryFailedIngestion. " +
        "A control that renders and calls nothing an operator wanted is the same dead end the issue reported, " +
        "moved behind a button. Calls seen: " +
        JSON.stringify(retry.mock.calls)
    ).toEqual([[STUCK.id]]);

    // The re-read, which is what turns the press into a visible change:
    // the verb answers 204 and says nothing about the artifact, so the
    // page has to go and look.
    await waitFor(() =>
      expect(
        getArtifact.mock.calls.length,
        "the press did not re-read the artifact, so the page still shows the state the backup has left"
      ).toBeGreaterThan(readsBefore)
    );

    // And the answer, in words that say what the retry actually does about
    // the collision this backup is stuck behind.
    expect(await screen.findByRole("status")).toHaveTextContent(/Re-attempted/);
  });

  /**
   * The discriminator the existence assertion never had.
   *
   * A card gated on nothing at all satisfies every "the control is there"
   * case in this file. It would also tell an operator looking at a
   * perfectly good backup that it "failed an attempt and is not
   * quarantined", which is a false alarm on the one screen whose job is
   * to say whether a backup is safe.
   */
  it("control: a healthy backup is offered no Recovery card and no retry control", async () => {
    const healthy: BackupArtifact = {
      ...STUCK,
      state: "COMPLETE",
      validation: "verified",
      sizeBytes: 294,
      remoteSourceRemovedAt: "2026-09-07T01:19:00Z"
    };
    renderDetail(healthy);
    await screen.findByText(healthy.filename);

    expect(screen.queryByRole("heading", { name: /recovery/i })).toBeNull();
    expect(
      screen.queryByRole("button", { name: /retry ingestion/i }),
      "the Recovery card is not gated on anything that distinguishes a stuck backup from a good one, so every " +
        "artifact in the product is now labelled as needing intervention"
    ).toBeNull();
    // Named precisely, because the sentence is the half of the remedy: it
    // must not appear over a backup that is fine.
    expect(screen.queryByText(/failed an attempt and is not quarantined/i)).toBeNull();
  });

  /**
   * The wiring, watched.
   *
   * `readOnly` reaches this page from App.tsx and from nowhere else, and
   * every case above renders the page directly, so the prop being dropped
   * from the route was invisible. It is not a cosmetic prop: a browser
   * talking to a service it is not compatible with is exactly where a
   * write must not be offered, and re-entering the pipeline is a write.
   *
   * This goes through the real App so the derivation is included:
   * `readOnly` is `!version.compatible` (state/appNodes.ts), not a literal
   * a test can hand in.
   */
  it("read-only deployment: the control is drawn and refuses to fire", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getArtifact").mockResolvedValue(STUCK);
    const version = await api.getVersion();
    const incompatible: VersionInfo = { ...version, service: "1.2.0", api: "v0", compatible: false };
    vi.spyOn(api, "getVersion").mockResolvedValue(incompatible);
    const retry = vi.spyOn(api, "retryFailedIngestion").mockResolvedValue(undefined);

    const authenticated: AuthContext = { authenticated: true, username: "rom", mode: "local-account" };
    const bridge: PlatformBridge = { ...genericBridge, getAuthContext: () => Promise.resolve(authenticated) };

    render(
      <MemoryRouter initialEntries={["/backups/" + STUCK.id]}>
        <ApiProvider api={api}>
          <PlatformProvider bridge={bridge}>
            <App />
          </PlatformProvider>
        </ApiProvider>
      </MemoryRouter>
    );

    const button = await screen.findByRole("button", { name: /retry ingestion/i }, { timeout: 4000 });
    expect(
      button,
      "App.tsx's route does not pass readOnly to BackupDetailPage, so a browser incompatible with the service " +
        "it is talking to offers a live write anyway"
    ).toBeDisabled();

    // Pressed anyway, because "disabled" is an attribute and the claim is
    // about the call: a control that looks refused and fires is worse than
    // one that fires openly.
    await userEvent.click(button);
    expect(retry).not.toHaveBeenCalled();
  });

  /**
   * A refused press, answered.
   *
   * The card rendered `e.message` bare, so a refusal arrived as one
   * sentence with no next step and no way to tie it to anything the
   * engine logged. That is the complaint #662 makes about the docked
   * terminal, reproduced by the control added to answer #662: the product
   * is a NAS with no shell, so quoting a correlation id into an issue is
   * the entire diagnostic move available to the person who pressed it.
   *
   * ARTIFACT_NOT_FAILED is the refusal chosen because it is the one this
   * route can actually produce that an operator can act on: the row moved
   * while the page was open, so the page is stale rather than the backup
   * broken.
   */
  it("says why a refused retry was refused, and names the id the engine logged", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getArtifact").mockResolvedValue(STUCK);
    vi.spyOn(api, "retryFailedIngestion").mockRejectedValue(
      new BackupManagerError({
        code: "ARTIFACT_NOT_FAILED",
        message: "this backup is not failed",
        correlationId: "cid_NrcGcTMo"
      })
    );

    render(
      <MemoryRouter initialEntries={["/backups/" + STUCK.id]}>
        <ApiProvider api={api}>
          <Routes>
            <Route path="/backups/:artifactId" element={<BackupDetailPage />} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );
    await screen.findByText(STUCK.filename);
    await userEvent.click(screen.getByRole("button", { name: /retry ingestion/i }));

    const answer = await screen.findByRole("status");
    // The engine's own words, unparaphrased.
    expect(answer).toHaveTextContent(/this backup is not failed/);
    // What to do about it, which the engine's sentence cannot say because
    // it is a fact about this page.
    expect(
      answer,
      "a refusal with no next step leaves the operator exactly where #662 left them: a control that says no and " +
        "nothing else"
    ).toHaveTextContent(/[Rr]eload/);
    // And the one token that ties this screen to the engine's log.
    expect(
      answer,
      "the correlation id came back on the refusal and the card dropped it, so the person who pressed the button " +
        "has nothing to quote"
    ).toHaveTextContent(/cid_NrcGcTMo/);
  });

  /**
   * The refusal that is not a refusal.
   *
   * `fetch` rejecting means nothing came back, so whether the re-attempt
   * was started is genuinely unknown (RequestFailure's own doc makes that
   * the point of the label). Telling somebody to press again when it may
   * already have run is advice with a cost, and this is a product where
   * the cost is a re-transfer over a link the operator is watching.
   */
  it("does not claim a retry failed when nothing came back to say so", async () => {
    const api = createMockApi();
    vi.spyOn(api, "getArtifact").mockResolvedValue(STUCK);
    vi.spyOn(api, "retryFailedIngestion").mockRejectedValue(
      new RequestFailure({
        kind: "no-response",
        path: "/backups/" + STUCK.id + "/retry",
        cause: new TypeError("Failed to fetch")
      })
    );

    render(
      <MemoryRouter initialEntries={["/backups/" + STUCK.id]}>
        <ApiProvider api={api}>
          <Routes>
            <Route path="/backups/:artifactId" element={<BackupDetailPage />} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );
    await screen.findByText(STUCK.filename);
    await userEvent.click(screen.getByRole("button", { name: /retry ingestion/i }));

    const answer = await screen.findByRole("status");
    expect(answer).toHaveTextContent(/did not answer/);
    expect(
      answer,
      "the card reported a request that got no reply as a failed retry, so an operator presses again over a " +
        "re-attempt that may already be running"
    ).toHaveTextContent(/unknown/);
  });
});
