/**
 * Issue #795. The Activity page showing nothing on a real UGREEN NAS
 * because the web-ui container could not reach the engine container:
 *
 *     serve-ui: dial tcp: lookup rclone-manager: no such host
 *
 * That is one deployment fault with TWO different shapes on the browser's
 * side, and the difference is the whole of why this file exists.
 *
 *   NOBODY ANSWERED. `fetch` itself rejects, the browser console says
 *   "TypeError: Failed to fetch", and client.ts labels it
 *   RequestFailure{kind:"no-response"}. There is no status and no
 *   correlation id, because there is no response.
 *
 *   THE FRONT DOOR ANSWERED FOR THE SERVICE. serve-ui is up and serves
 *   the bundle, so the browser reaches it; its reverse proxy cannot
 *   reach the engine and writes a bodyless 502 with its own
 *   X-Correlation-Id on it (apps/common/webhost/serve/ui.go's
 *   ErrorHandler, #761). This is what the reported NAS actually produced
 *   once the page itself had loaded, and it is the shape nothing was
 *   testing: a real response, a real id, and a body that is not a typed
 *   envelope.
 *
 * The second group drives the REAL httpApi with a stubbed `fetch`, not a
 * mock API, because the defect it covers lives in the translation from a
 * bodyless 502 into something an operator can act on. A test that handed
 * the page a pre-made error would have asserted the sentence it wrote
 * itself.
 *
 * What every case asserts is the same thing in the end, and it is #795's
 * actual complaint: the page must not "show nothing". A surfaced alert,
 * wording that names WHICH hop failed, no minted id, and a Try again that
 * really re-issues.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { ActivityPage } from "@shared/pages/ActivityPage";
import { DashboardPage } from "@shared/pages/DashboardPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import { httpApi } from "@shared/api/client";
import { RequestFailure } from "@shared/api/contracts";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { resetGraphForTests } from "@shared/state/graph";
import type { BackupdApi } from "@shared/api/contracts";
import type { AsyncState } from "@shared/hooks/useAsync";
import type { ActivityEvent } from "@shared/types/operation";
import type { BackupSet } from "@shared/types/backup";
import type { SystemHealth } from "@shared/types/operation";

/** What client.ts throws when `fetch` rejected: the engine-unreachable
 *  fault seen from a browser that never got a response at all. */
function noResponse() {
  return new RequestFailure({
    kind: "no-response",
    path: "/activity",
    cause: new TypeError("Failed to fetch")
  });
}

/** The other one, for the comparison. A response DID arrive and could not
 *  be read, which is a different problem for whoever is fixing it. */
function unreadableBody() {
  return new RequestFailure({
    kind: "unreadable-body",
    path: "/activity",
    status: 200,
    contentType: "text/html",
    correlationId: "cid_read42",
    cause: new SyntaxError("Unexpected token '<', \"<!doctype \"... is not valid JSON")
  });
}

/**
 * serve-ui's own answer when its upstream is unreachable: status 502, the
 * correlation id it minted for the request (webhost.RequestScope sets it
 * on the response before the proxy ever runs), and no body at all —
 * net/http's ErrorHandler writes a status line and stops.
 */
function proxyGatewayFailure(status = 502) {
  return {
    ok: false,
    status,
    headers: new Headers({ "x-correlation-id": "cid_proxy502" }),
    json: async () => {
      throw new SyntaxError("Unexpected end of JSON input");
    }
  };
}

const HEALTH: AsyncState<SystemHealth> = {
  data: null,
  error: null,
  loading: false,
  reload: () => {}
};

/** One configured set, because a dashboard with none renders its "No
 *  backup sets yet" empty state instead of the panel under test. */
const SET: BackupSet = {
  connectionUnverified: false,
  id: "production/postgres-primary",
  source: "production",
  set: "postgres-primary",
  name: "Production PostgreSQL",
  host: "prod-db-01.internal",
  port: 22,
  username: "backup-agent",
  remoteFolder: "/backups/postgresql/",
  includePatterns: ["*.dump.zst"],
  excludePatterns: ["*.tmp"],
  completionMethod: "completion-marker",
  stableForSeconds: 0,
  destination: "/data/backups/production/postgres/",
  retentionIsOverride: false,
  validations: ["transfer", "checksum"],
  state: "healthy",
  stateNote: "Verified nightly dump.",
  enabled: true,
  readOnly: false,
  readOnlyRetainedCount: 0,
  newestKnownGoodAt: "2026-08-29T02:01:01+02:00",
  lastRunAt: "2026-08-29T02:01:01+02:00",
  lastValidation: "passed",
  expectedIntervalHours: 24,
  retainedCount: 32,
  retainedBytes: 421 * 1024 ** 3,
  trustedHostKeys: [{ algorithm: "ssh-ed25519", fingerprint: "SHA256:test-fingerprint" }],
  trustedHostKeyRecordedAt: "2026-08-02T10:14:00+02:00",
  sshKeyId: "key_fixture_1"
};

const SETS: AsyncState<BackupSet[]> = {
  data: [SET],
  error: null,
  loading: false,
  reload: () => {}
};

const EVENT: ActivityEvent = {
  id: "evt_recovered_1",
  at: "2026-09-11T08:15:00+02:00",
  type: "backup-committed",
  severity: "ok",
  setId: SET.id,
  setName: SET.name,
  text: "Backed up 3 files from the VPS",
  detail: "",
  correlationId: "cid_event1"
};

function withProviders(api: BackupdApi, page: "activity" | "dashboard") {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <PlatformProvider bridge={genericBridge}>
          {page === "activity" ? (
            <ActivityPage />
          ) : (
            <DashboardPage health={HEALTH} sets={SETS} readOnly={false} />
          )}
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}

function renderWithRejection(page: "activity" | "dashboard", rejectWith: unknown) {
  const api = createMockApi();
  vi.spyOn(api, "listActivity").mockRejectedValue(rejectWith);
  return withProviders(api, page);
}

/** The dashboard's Recent activity panel, by the label it carries. */
function recentActivityPanel() {
  return screen.getByRole("region", { name: "Recent activity" });
}

describe("a request that got no reply reaches the operator as one", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("surfaces the Activity page failure instead of an empty timeline", async () => {
    renderWithRejection("activity", noResponse());
    await act(async () => {});

    const alert = screen.getByRole("alert");
    // The distinction #598 typed and #795 needs on screen: this says
    // nothing came back, not that something came back unreadable.
    expect(alert.textContent).toContain("Backupd did not answer");
    expect(alert.textContent).not.toContain("could not read the answer");
    // The route that failed and the browser's own words for why, so a
    // screenshot of this banner is worth something to whoever reads it.
    expect(alert.textContent).toContain("GET /api/v1/activity");
    expect(alert.textContent).toContain("TypeError: Failed to fetch");
    // #598's rule, and the one an operator on 0.3.2 was given instead of
    // a diagnosis: there was no response, so there is no id.
    expect(document.body.textContent).not.toContain("unavailable");
  });

  it("says something different when a response did arrive and could not be read", async () => {
    renderWithRejection("activity", noResponse());
    await act(async () => {});
    const silence = screen.getByRole("alert").textContent ?? "";

    cleanup();
    resetGraphForTests();
    renderWithRejection("activity", unreadableBody());
    await act(async () => {});
    const garbled = screen.getByRole("alert").textContent ?? "";

    expect(silence).not.toBe(garbled);
    // The readable half carried an id off the response, so it offers one.
    expect(garbled).toContain("cid_read42");
    expect(silence).not.toContain("correlation id");
  });

  it("re-issues the request when Try again is pressed, and clears on success", async () => {
    const api = createMockApi();
    const listActivity = vi
      .spyOn(api, "listActivity")
      .mockRejectedValueOnce(noResponse())
      .mockResolvedValue({ events: [EVENT] });
    withProviders(api, "activity");
    await act(async () => {});

    expect(screen.getByRole("alert")).toBeTruthy();
    expect(listActivity).toHaveBeenCalledTimes(1);

    await userEvent.click(screen.getByRole("button", { name: /try again/i }));
    await act(async () => {});

    // A button that looks like it did something and did not is the
    // failure mode #598 wrote `retriedAt` for; this asserts the other
    // half, that the request really goes back out and the page recovers.
    expect(listActivity).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText(EVENT.text)).toBeTruthy();
  });

  it("does not let the dashboard's Recent activity panel draw as empty", async () => {
    renderWithRejection("dashboard", noResponse());
    await act(async () => {});

    const panel = recentActivityPanel();
    const alert = within(panel).getByRole("alert");
    expect(alert.textContent).toContain("Backupd did not answer");
    expect(alert.textContent).toContain("TypeError: Failed to fetch");
    // A NAS where nothing has ever happened and a NAS whose engine is
    // unreachable must not look the same on this panel.
    expect(panel.textContent).not.toContain("unavailable");
  });
});

/**
 * The half #795 was actually reported from, and the half nothing covered.
 *
 * The real httpApi, a stubbed `fetch`, and the exact response serve-ui
 * writes when it cannot reach the engine. The page is expected to say
 * which of the two containers failed: the front door answered, the
 * service behind it did not.
 */
describe("a bodyless 502 from serve-ui names the hop that failed", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(proxyGatewayFailure()));
  });

  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("does not blame the backup service for a proxy that never reached it", async () => {
    withProviders(httpApi, "activity");
    await act(async () => {});

    const alert = screen.getByRole("alert");
    // What an operator got before this: "The backup service returned an
    // unexpected response." The backup service returned nothing at all —
    // the sentence names the wrong machine, and offers no next step on a
    // fault whose next step is "look at the other container".
    expect(alert.textContent).toContain("could not reach the Backupd service");
    expect(alert.textContent).toMatch(/running|reach/i);
    expect(alert.textContent).not.toContain("returned an unexpected response");
  });

  it("quotes the id the 502 really carried, which is in serve-ui's own log", async () => {
    withProviders(httpApi, "activity");
    await act(async () => {});

    const alert = screen.getByRole("alert");
    // webhost.RequestScope sets X-Correlation-Id on the response before
    // the proxy runs, so the ErrorHandler's bodyless 502 still carries
    // one, and its proxy_error line names the same id. Unlike the
    // no-response case this id is real and worth grepping for.
    expect(alert.textContent).toContain("cid_proxy502");
    expect(alert.textContent).toContain("status 502");
  });

  it("surfaces the same failure on the dashboard's Recent activity panel", async () => {
    withProviders(httpApi, "dashboard");
    await act(async () => {});

    const panel = recentActivityPanel();
    expect(within(panel).getByRole("alert").textContent).toContain("could not reach the Backupd service");
  });

  it("still shows a typed refusal's own words when the service did answer", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 503,
        headers: new Headers({ "x-correlation-id": "cid_typed503" }),
        json: async () => ({ error: { code: "INTERNAL", message: "the scheduler is restarting" } })
      })
    );
    withProviders(httpApi, "activity");
    await act(async () => {});

    // A gateway status is only this frontend's business when nothing
    // typed came back with it. A service that answered 503 WITH a reason
    // has said more than any sentence written here could.
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("the scheduler is restarting");
    expect(alert.textContent).not.toContain("could not reach the Backupd service");
  });

  it("keeps a typed 5xx's own words even for a code this bundle has never heard of", async () => {
    // #795's review, and the sharper half of the case above. The first
    // draft asked `code === "unknown" && gateway status`, and `unknown`
    // is ALSO what toApiErrorCode returns for a perfectly valid code
    // this build does not know — so a service one version newer,
    // refusing with an actionable sentence, was rewritten as a proxy
    // that could not reach it. Provenance ("a typed envelope was
    // parsed") is a different fact from "the code is recognised", and
    // this is the case that tells them apart.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 503,
        headers: new Headers({ "x-correlation-id": "cid_typed503" }),
        json: async () => ({
          error: {
            code: "SCHEDULER_MAINTENANCE",
            message: "Backupd is in a maintenance window until 04:00 and is not reading the journal."
          }
        })
      })
    );
    withProviders(httpApi, "activity");
    await act(async () => {});

    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("maintenance window until 04:00");
    expect(alert.textContent).not.toContain("could not reach the Backupd service");
    expect(alert.textContent).toContain("cid_typed503");
  });

  it("believes serve-ui's own marker rather than reading topology off a status", async () => {
    // The other direction: a refusal this proxy wrote, on a status that
    // is not one of the three gateway ones. serve-ui marks its own
    // proxy errors now (webhost/serve's ProxyErrorHeader, deleted from
    // any upstream response), so "nothing in this came from the
    // service" is a stated fact rather than an inference from 502.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        headers: new Headers({
          "x-correlation-id": "cid_marked500",
          "x-backupd-proxy-error": "upstream-unreachable"
        }),
        json: async () => {
          throw new SyntaxError("Unexpected end of JSON input");
        }
      })
    );
    withProviders(httpApi, "activity");
    await act(async () => {});

    expect(screen.getByRole("alert").textContent).toContain("could not reach the Backupd service");
  });

  it("names no hop for an untyped refusal that nothing identified", async () => {
    // An untyped 500 with no marker: the engine answered something
    // unreadable, a front proxy nobody configured answered for it, or
    // something else again. Provenance is not established, so the
    // wording that names a hop must not appear — this frontend saying
    // "the web interface could not reach the service" about a refusal it
    // cannot place is the same guess, made in the opposite direction.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        headers: new Headers({ "x-correlation-id": "cid_untyped500" }),
        json: async () => {
          throw new SyntaxError("Unexpected end of JSON input");
        }
      })
    );
    withProviders(httpApi, "activity");
    await act(async () => {});

    const alert = screen.getByRole("alert");
    expect(alert.textContent).not.toContain("could not reach the Backupd service");
    expect(alert.textContent).toContain("cid_untyped500");
  });
});
