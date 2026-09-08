import { afterEach, describe, expect, it, vi } from "vitest";

import { httpApi } from "./client";
import * as contracts from "./contracts";
import {
  API_BASE_PATH,
  API_ERROR_CODES,
  API_OPERATIONS,
  CAPABILITY_FIELDS,
  UI_ERROR_CODES,
  WIRE_ERROR_CODES
} from "./generated/contract";
import type { WireMediumPreflightCheck } from "./generated/contract";
import type { PlatformCapabilities } from "@shared/types/platform";

/**
 * Issue #166 (B6.2): the TypeScript half of the /api/v1 contract gate.
 *
 * scripts/api/check-contract-drift.sh proves the checked-in generated
 * module still matches api/v1/openapi.json. This file proves the rest of
 * ui/shared actually CONSUMES it: a generated module nobody imports is a
 * second source of truth with extra steps, which is the exact failure the
 * issue prohibits.
 */

describe("the error-code registry has one source", () => {
  it("re-exports the generated array itself, not a copy of it", () => {
    // toBe, not toEqual, and that distinction is the whole test: a
    // hand-maintained list transcribed from the contract would satisfy
    // toEqual on the day it was written and go stale silently afterwards.
    expect(contracts.API_ERROR_CODES).toBe(API_ERROR_CODES);
  });

  it("can tell a copy from the real thing", () => {
    // The positive control for the assertion above. Without it, `toBe`
    // passing would be equally consistent with `toBe` comparing by value.
    const copy = [...API_ERROR_CODES];
    expect(copy).toEqual([...API_ERROR_CODES]);
    expect(copy).not.toBe(API_ERROR_CODES);
  });

  it("splits every registered code into exactly one of wire or UI", () => {
    expect([...WIRE_ERROR_CODES, ...UI_ERROR_CODES].sort()).toEqual([...API_ERROR_CODES].sort());
    const overlap = WIRE_ERROR_CODES.filter((c) => (UI_ERROR_CODES as readonly string[]).includes(c));
    expect(overlap).toEqual([]);
  });

  it("narrows an unregistered code to the sentinel rather than asserting it through", () => {
    expect(contracts.toApiErrorCode("RETENTION_PLAN_STALE")).toBe("RETENTION_PLAN_STALE");
    expect(contracts.toApiErrorCode("A_CODE_THE_CONTRACT_DOES_NOT_REGISTER")).toBe("unknown");
  });
});

describe("the capability model is the contract's, not a second one", () => {
  it("declares exactly the capabilities GET /system/capabilities reports", () => {
    // The object literal is typed, so a field added to PlatformCapabilities
    // fails to compile here until it is listed; the runtime comparison
    // then fails until the contract lists it too. Both directions are
    // needed: a type-only check cannot see the contract, and a runtime
    // check alone cannot see the interface.
    const capabilities: PlatformCapabilities = {
      nativeAuth: false,
      nativeNotifications: false,
      storagePicker: false,
      embeddedWindow: false,
      appStorePackaging: false
    };
    const camel = (wire: string) =>
      wire.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase());

    expect(Object.keys(capabilities).sort()).toEqual([...CAPABILITY_FIELDS].map(camel).sort());
  });
});

describe("the preflight step list is the contract's, not a second one", () => {
  it("names every step the contract names and no others", () => {
    // Found while fixing #633. `MediumPreflightCheck["step"]` used to
    // restate the list instead of consuming it, and the copy had already
    // gone stale: #622 gave the local drive a report with a ninth step,
    // `space`, which the engine emits and the contract carries, and the
    // hand-written union never got it. So the one report shape every
    // deployment has could not be typed here, which is what would have
    // stopped anyone teaching the fixture to produce it.
    //
    // A type-level assertion because a TypeScript union has no runtime
    // form to compare, and mutual assignability because one direction is
    // not enough: `extends` alone passes a union that quietly LOST a
    // member as readily as one that gained one, and losing a member is
    // exactly what happened. `[A] extends [B]` rather than `A extends B`
    // keeps the check off the distributive path, where a per-member test
    // answers a different question.
    type SameUnion<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;
    const stepsAgree: SameUnion<
      contracts.MediumPreflightCheck["step"],
      WireMediumPreflightCheck["step"]
    > = true;

    // Both assertions below are trivially true at runtime, and saying so
    // is the honest version: the check IS the annotation, and the gate
    // that runs it is `npm run lint`, not `vitest run`. They are here so
    // the case is a case rather than two dangling declarations, which
    // `noUnusedLocals` would reject anyway. A green vitest run is not
    // evidence about this one, and a reader deciding whether the step
    // list is still one list should look at the tsc step.
    expect(stepsAgree).toBe(true);
    const local: contracts.MediumPreflightCheck["step"] = "space";
    expect(local).toBe("space");
  });
});

// ---------------------------------------------------------------------------
// Every request the client makes is an operation the contract declares.
// ---------------------------------------------------------------------------

/**
 * The paths `httpApi` calls that the canonical runtime does not serve and
 * the contract therefore does not declare.
 *
 * EMPTY, and it must stay that way.
 *
 * It used to hold fourteen entries, described here as "recorded debt, not
 * an exemption mechanism". That description was accurate and the list was
 * still the reason this suite stayed green while four of the six shipped
 * pages failed against a real backend (#211): an allowlist asserted
 * exactly is a gate that reports the drift it was built to stop as a
 * passing test. Issue #211 closed every entry, and
 * scripts/api/check-client-paths.sh now enforces the same rule statically,
 * with no allowlist at all, on every commit.
 *
 * Nothing may be added here. A path the contract does not declare is a
 * path a real backend answers with a 404 or a 405.
 */
const UNIMPLEMENTED_CLIENT_PATHS: string[] = [];

/**
 * Turns a contract path into a matcher for a concrete URL.
 *
 * A path parameter matches one segment, except `getBackupSet`'s, which the
 * contract documents as a two-part `source/name` identity that spans
 * segments (which is why the router registers it as a catch-all). That is
 * one named exception with a reason, not a general loosening: every other
 * parameter stays segment-bounded, so `POST /backup-sets/{id}/run` does
 * NOT quietly match `POST /backup-sets/test-connection`.
 */
function matcherFor(operation: (typeof API_OPERATIONS)[number]): RegExp {
  const segment = operation.id === "getBackupSet" ? ".+" : "[^/]+";
  const pattern = operation.path
    .split("/")
    .map((part) => (part.startsWith("{") ? segment : part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")))
    .join("/");
  return new RegExp(`^${API_BASE_PATH}${pattern}$`);
}

/**
 * Every method on `api` that no entry in `driven` names.
 *
 * This is what makes the hand-written call list above self-checking: it
 * is a second list in a second file, and the documented guarantee that a
 * new unbacked path fails CI on the commit that adds it only holds if
 * this list is complete. It is a free function rather than an inline
 * expression so it can be driven directly by its own positive control.
 */
function undrivenMethods(driven: string[], api: object): string[] {
  const named = new Set(driven);
  return Object.keys(api)
    .filter((method) => !named.has(method))
    .sort();
}

describe("every request the shared client makes is a declared operation", () => {
  const observed: string[] = [];
  /** Every recorded call, with the headers it actually sent. Kept beside
   *  `observed` rather than replacing it so the path assertions below
   *  keep reading the simple strings they were written against. */
  const observedCalls: Array<{ method: string; url: string; headers: Record<string, string> }> = [];

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("matches each call against the contract, and pins the ones that match nothing", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string, init?: RequestInit) => {
        const method = (init?.method ?? "GET").toUpperCase();
        observed.push(`${method} ${url}`);
        observedCalls.push({
          method,
          url,
          headers: (init?.headers ?? {}) as Record<string, string>
        });
        return {
          ok: true,
          status: 200,
          headers: { get: () => null },
          json: async () => ({
            backup_sets: [],
            validators: [],
            verdicts: [],
            include: [],
            retention: { tiers: [] },
            schema: { retention: { default_tiers: [] } }
          })
        };
      })
    );

    // Every method on the client, driven once, each named by the httpApi
    // key it drives so the coverage assertion below can prove this list
    // is complete. The response above is deliberately thin: what is being
    // recorded is the REQUEST, and a mapper that throws on a thin body
    // has still already made its call.
    const calls: Array<[string, () => Promise<unknown>]> = [
      ["getVersion", () => httpApi.getVersion()],
      ["getHealth", () => httpApi.getHealth()],
      ["getFirstRunStatus", () => httpApi.getFirstRunStatus()],
      // Issue #176. Driven with the same spec createBackupSet is driven
      // with, deliberately: both operations declare the contract's
      // BackupSetSpec, and this list is what proves each one's REQUEST
      // reaches a path the contract declares.
      ["completeFirstRun", () => httpApi.completeFirstRun({
        name: "n", host: "h", port: 22, user: "u", sshKeyId: "k",
        knownHostsLine: "l", remotePath: "/r", localPath: "/l",
        include: [], completionStrategy: "rename"
      })],
      ["listSets", () => httpApi.listSets()],
      ["getSet", () => httpApi.getSet("src/set-1")],
      ["runCycle", () => httpApi.runCycle("rev-1", "idem-run-cycle")],
      // The second run action on the same route (issue #597, G1.4). Same
      // reason restoreCopy is listed below: a client method nobody drives
      // from this list is invisible to the whole file.
      ["runBackupSet", () => httpApi.runBackupSet("src/set-1", "rev-1", "idem-run-set")],
      // The other action on the same /operations route (EPIC E, FR-34).
      // It is listed here rather than left out because a client method
      // nobody drives from this list is invisible to the whole file: it
      // could call any path it liked and nothing would notice, which is
      // the hole M5 on #194 closed.
      ["restoreCopy", () => httpApi.restoreCopy({
        artifactId: "src/set-1/a.tar.gz", medium: "cold-store",
        windowDays: 3, acknowledged: true, configRevision: "rev-1",
        idempotencyKey: "idem-restore"
      })],
      ["testConnection", () => httpApi.testConnection("set-1")],
      ["setEnabled", () => httpApi.setEnabled("src", "set-1", true)],
      ["setReadOnly", () => httpApi.setReadOnly("src", "set-1", true)],
      // Issue #350: the edit path and its hold. Every one of them keys on
      // the same {source}/{set} pair setEnabled and setReadOnly do.
      ["updateBackupSet", () => httpApi.updateBackupSet("src", "set-1", { remoteFolder: "/r" })],
      // Issue #391: the removal, on the same {source}/{set} pair, told
      // apart from the PATCH above by its method.
      ["removeSet", () => httpApi.removeSet("src", "set-1")],
      ["getEditHold", () => httpApi.getEditHold("src", "set-1")],
      ["takeEditHold", () => httpApi.takeEditHold("src", "set-1")],
      ["releaseEditHold", () => httpApi.releaseEditHold("src", "set-1")],
      ["createBackupSet", () => httpApi.createBackupSet({
        name: "n", host: "h", port: 22, user: "u", sshKeyId: "k",
        knownHostsLine: "l", remotePath: "/r", localPath: "/l",
        include: [], completionStrategy: "rename"
      })],
      ["listValidators", () => httpApi.listValidators()],
      ["importSSHKey", () => httpApi.importSSHKey("pem")],
      ["probeHostKey", () => httpApi.probeHostKey("h", 22)],
      // Issue #592: the two reads and the second import. Listed here for
      // the reason restoreCopy's own comment gives: a client method
      // nobody drives from this list is invisible to the whole file, so
      // it could call any path it liked and nothing would notice. That
      // matters more than usual for importSSHKeyCandidate, which is the
      // one method here whose route changed after it shipped:
      // `POST /ssh-keys/from-candidate` exists because /ssh-keys may not
      // stop requiring private_key_pem, and this list is what proves the
      // client moved with it.
      ["listSSHKeys", () => httpApi.listSSHKeys()],
      ["listSSHKeyCandidates", () => httpApi.listSSHKeyCandidates()],
      ["importSSHKeyCandidate", () => httpApi.importSSHKeyCandidate("cand_1")],
      ["testCandidateConnection", () => httpApi.testCandidateConnection({
        host: "h", port: 22, user: "u", sshKeyId: "k", knownHostsLine: "l"
      })],
      ["listArtifacts", () => httpApi.listArtifacts()],
      // The five below take a backup's id, which is three parts
      // (source/set/name, model.NewArtifactID) and is spelled that way in
      // the route the contract publishes. They were driven with a
      // single-segment "artifact-1" while the contract still spelled the
      // path as one {id} that spans segments, and nothing noticed,
      // because a one-part id and a three-part one both pasted into one
      // placeholder. A fixture that feeds this file a shape production
      // never produces cannot check what production does, so they take
      // the same id restoreCopy above already takes.
      ["getArtifact", () => httpApi.getArtifact("src/set-1/a.tar.gz")],
      ["listOperations", () => httpApi.listOperations()],
      ["listActivity", () => httpApi.listActivity()],
      // Driven with no options, which is the shape the dashboard actually
      // sends on its first look: every set, from wherever the service's
      // buffer still starts.
      ["getLiveActivity", () => httpApi.getLiveActivity()],
      ["listQuarantine", () => httpApi.listQuarantine()],
      ["revalidate", () => httpApi.revalidate("src/set-1/a.tar.gz")],
      ["retryIngestion", () => httpApi.retryIngestion("src/set-1/a.tar.gz")],
      ["retryFailedIngestion", () => httpApi.retryFailedIngestion("src/set-1/a.tar.gz", "the NAS came back")],
      ["reinstate", () => httpApi.reinstate("src/set-1/a.tar.gz")],
      ["previewRetention", () => httpApi.previewRetention("src", "set-1")],
      ["applyRetention", () => httpApi.applyRetention("src", "set-1", "plan-1")],
      ["getBackupSetRetention", () => httpApi.getBackupSetRetention("src", "set-1")],
      ["setBackupSetRetention", () =>
        httpApi.setBackupSetRetention("src", "set-1", {
          tiers: [{ name: "daily", granularity: "day", keep: 7 }]
        })],
      ["clearBackupSetRetention", () => httpApi.clearBackupSetRetention("src", "set-1")],
      ["getSettings", () => httpApi.getSettings()],
      ["updateSettings", () => httpApi.updateSettings({ retention: { timezone: "UTC" } })],
      ["preflightStorageMedium", () => httpApi.preflightStorageMedium("offsite_s3")],
      // G2.2 (#594). The candidate spec here is the wizard's own step-3
      // submission: a whole destination with a credential REFERENCE and
      // no material, which is what makes checking one before it is saved
      // possible at all.
      ["importStorageCredentials", () =>
        httpApi.importStorageCredentials("EXAMPLE-NOT-A-REAL-KEY", "EXAMPLE-NOT-A-REAL-SECRET")],
      ["listStorageMediums", () => httpApi.listStorageMediums()],
      ["getStorageMedium", () => httpApi.getStorageMedium("offsite_s3")],
      ["getStorageMediumUsage", () => httpApi.getStorageMediumUsage("offsite_s3")],
      ["preflightStorageMediumCandidate", () =>
        httpApi.preflightStorageMediumCandidate({
          id: "offsite_s3", type: "s3", bucket: "nas-backups",
          credentials: { credentialsId: "cred-1" }
        })],
      ["createStorageMedium", () =>
        httpApi.createStorageMedium({
          id: "offsite_s3", type: "s3", bucket: "nas-backups",
          credentials: { credentialsId: "cred-1" }
        })],
      ["updateStorageMedium", () =>
        httpApi.updateStorageMedium("offsite_s3", {
          id: "offsite_s3", type: "s3", bucket: "nas-backups"
        })],
      ["removeStorageMedium", () => httpApi.removeStorageMedium("offsite_s3")],
      ["setDefaultStorageMedium", () => httpApi.setDefaultStorageMedium("offsite_s3")],
      ["getStorage", () => httpApi.getStorage()],
      ["scanCatalog", () => httpApi.scanCatalog()],
      ["rebuildCatalog", () => httpApi.rebuildCatalog()],
      ["login", () => httpApi.login("u", "p")],
      ["enrollAdministrator", () => httpApi.enrollAdministrator("u", "p")],
      ["rotatePassword", () => httpApi.rotatePassword("a", "b")],
      ["logout", () => httpApi.logout()]
    ];
    for (const [, call] of calls) {
      await call().catch(() => undefined);
    }

    // One request per method, except the two that join the per-set health
    // report onto the sets list (issue #245). Named exactly rather than
    // relaxed into ">= calls.length": a method that quietly grew a second
    // request is worth seeing in a diff, which is the whole reason this
    // count is asserted at all.
    // updateBackupSet joins the same per-set health report onto its
    // answer that getSet does, and for the same reason: an edit can
    // genuinely change the freshness verdict, and a page showing a new
    // value beside the old verdict would be showing two moments at once.
    const MULTI_REQUEST_METHODS = ["listSets", "getSet", "updateBackupSet"];
    expect(observed.length).toBe(calls.length + MULTI_REQUEST_METHODS.length);

    // Nothing above asserted that `calls` covers httpApi, so a client
    // method nobody added here was simply invisible: it called whatever
    // path it liked and this file never saw it (M5, #194 review). The Go
    // side has never had that hole, because it enumerates the real chi
    // route table rather than a hand-written list.
    expect(undrivenMethods(calls.map(([name]) => name), httpApi)).toEqual([]);

    const matchers = API_OPERATIONS.map((op) => ({ op, re: matcherFor(op) }));
    const unmatched = observed.filter(
      (call) =>
        !matchers.some(({ op, re }) => {
          const [method, url] = call.split(" ");
          return op.method === method && re.test(url.split("?")[0]);
        })
    );

    expect(unmatched.sort()).toEqual(UNIMPLEMENTED_CLIENT_PATHS);
  });

  /**
   * Issue #597's third layer, turned into a gate.
   *
   * Both generated contracts have always declared Idempotency-Key
   * required on POST /operations, and the handler has always refused
   * without it. The client sent no header at all, on every build this
   * project has shipped, because the `post` helper had no parameter for
   * one. Nothing noticed, because the assertions in this file were about
   * PATHS: a request that reached the right URL with the wrong headers
   * matched every one of them.
   *
   * So this reads the requirement off the contract rather than naming the
   * operation, which is what makes it a rule instead of a patch: a second
   * operation that grows `idempotencyKey: "required"` is covered on the
   * commit that adds it, with no edit here.
   */
  it("sends Idempotency-Key on every operation whose contract row requires it", () => {
    const required = API_OPERATIONS.filter((op) => op.idempotencyKey === "required");
    // An empty scan is not a clean scan. If the field were renamed, or
    // the generator stopped emitting it, "no operation requires a key"
    // would be true and worthless.
    expect(required.length).toBeGreaterThan(0);
    expect(observedCalls.length).toBeGreaterThan(0);

    const matchers = required.map((op) => ({ op, re: matcherFor(op) }));
    const missing: string[] = [];
    let checked = 0;
    for (const call of observedCalls) {
      const match = matchers.find(
        ({ op, re }) => op.method === call.method && re.test(call.url.split("?")[0])
      );
      if (!match) continue;
      checked += 1;
      const header = Object.entries(call.headers).find(
        ([name]) => name.toLowerCase() === "idempotency-key"
      );
      if (!header || header[1] === "") missing.push(`${match.op.id}: ${call.method} ${call.url}`);
    }
    // The positive control for the loop itself: with no call reaching a
    // key-requiring operation, `missing` is empty for the wrong reason.
    expect(checked).toBeGreaterThan(0);
    expect(missing).toEqual([]);
  });

  it("would notice a request that reached the route without the header", () => {
    // The positive control for the assertion above. Without it, the
    // header check passing would be equally consistent with the header
    // lookup matching anything at all.
    const header = Object.entries({ "content-type": "application/json" }).find(
      ([name]) => name.toLowerCase() === "idempotency-key"
    );
    expect(header).toBeUndefined();
    expect(
      Object.entries({ "Idempotency-Key": "k" }).find(([name]) => name.toLowerCase() === "idempotency-key")
    ).toEqual(["Idempotency-Key", "k"]);
  });

  it("would notice a client method that nothing in the list drives", () => {
    // The positive control for the coverage assertion above. Without it,
    // `toEqual([])` passing would be equally consistent with
    // undrivenMethods always returning an empty array.
    expect(undrivenMethods(["a"], { a: () => undefined, b: () => undefined })).toEqual(["b"]);
    expect(undrivenMethods(["a", "b"], { a: () => undefined, b: () => undefined })).toEqual([]);
  });

  /**
   * The reverse direction (issue #87, B5.1): a route the SERVER declares
   * and the client never calls.
   *
   * A dead client path 404s and somebody notices. A dead SERVER route is
   * silent: it is reachable, authenticated, and exercised by nothing but
   * its own Go unit tests, so nothing the shared UI does can ever
   * discover that it drifted. That matters most for the two operations
   * the contract marks `destructiveGate: true`, because the gate's
   * behaviour in front of a real browser is only observable through a
   * client that actually calls the route.
   *
   * `submitOperation` was the sharpest case and the reason this assertion
   * exists. POST /api/v1/operations is the most destructive route in the
   * API, and the shared UI's own "run now" used to call POST
   * /api/v1/backup-sets/{id}/run, which no runtime has ever served, so the
   * destructive gate had never refused anything a browser asked for and
   * never could. Issue #211 pointed "run now" at the real operation, which
   * is why it has left this list.
   *
   * `getSystemVersion` left for the same reason: the version banner used
   * to request /api/v1/version.
   *
   * `listStorageStatus` left with issue #286: GET /system/storage existed
   * on the contract with no client method reaching it at all, so nothing
   * here could ever have noticed a mismatch between what the dashboard's
   * storage gauge needed and what the route actually served. getStorage
   * (client.ts) now calls it, and DashboardPage reads the response.
   *
   * Asserted EXACTLY, like its counterpart, so the list can only shrink.
   */
  const UNREACHED_SERVER_OPERATIONS = [
    "getOperation",
    "getSession",
    "getSystemCapabilities"
  ];

  it("pins the contract operations no client call reaches", () => {
    if (observed.length === 0) {
      throw new Error(
        "no client calls were recorded, so this assertion would report every operation as unreached for the wrong reason"
      );
    }
    const matchers = API_OPERATIONS.map((op) => ({ op, re: matcherFor(op) }));
    const unreached = matchers
      .filter(
        ({ op, re }) =>
          !observed.some((call) => {
            const [method, url] = call.split(" ");
            return op.method === method && re.test(url.split("?")[0]);
          })
      )
      .map(({ op }) => op.id)
      .sort();

    expect(unreached).toEqual(UNREACHED_SERVER_OPERATIONS);
  });

  it("would notice a path the contract does not declare", () => {
    // The positive control. If matcherFor were permissive enough to match
    // anything, the assertion above would pass no matter what the client
    // did.
    const matchers = API_OPERATIONS.map((op) => ({ op, re: matcherFor(op) }));
    const matches = (method: string, url: string) =>
      matchers.some(({ op, re }) => op.method === method && re.test(url));

    expect(matches("GET", `${API_BASE_PATH}/system/version`)).toBe(true);
    expect(matches("GET", `${API_BASE_PATH}/system/versionn`)).toBe(false);
    expect(matches("DELETE", `${API_BASE_PATH}/system/version`)).toBe(false);
    expect(matches("POST", `${API_BASE_PATH}/backup-sets/set-1/run`)).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Capability data, never a platform-name conditional.
// ---------------------------------------------------------------------------

const PLATFORM_IDS = [
  "generic",
  "ugos",
  "synology",
  "truenas",
  "unraid",
  "openmediavault",
  "proxmox"
];

/**
 * Finds a comparison against a platform NAME, which #81's standing
 * constraint forbids in the shared UI: platform differences are capability
 * data, and branching on who the host is instead of on what it can do is
 * how a shared UI turns into seven of them.
 *
 * It matches a comparison operator, or a membership test, against one of
 * the platform identifiers. It deliberately does NOT match the identifiers
 * appearing in a type union, in prose, or as a value being passed along,
 * because all three are legitimate and all three occur.
 */
export function findPlatformNameConditionals(source: string): string[] {
  const ids = PLATFORM_IDS.join("|");
  const patterns = [
    new RegExp(`[!=]==?\\s*["'\`](${ids})["'\`]`, "g"),
    new RegExp(`["'\`](${ids})["'\`]\\s*[!=]==?`, "g"),
    new RegExp(`\\.includes\\(\\s*["'\`](${ids})["'\`]\\s*\\)`, "g"),
    new RegExp(`case\\s+["'\`](${ids})["'\`]\\s*:`, "g")
  ];
  const hits: string[] = [];
  for (const line of source.split("\n")) {
    const code = line.trim();
    if (code.startsWith("//") || code.startsWith("*") || code.startsWith("/*")) continue;
    for (const pattern of patterns) {
      pattern.lastIndex = 0;
      if (pattern.test(line)) hits.push(code);
    }
  }
  return hits;
}

/**
 * Every TypeScript source in ui/shared/src, read through Vite's own glob
 * rather than through node:fs.
 *
 * Vite resolves this at transform time, so the scan needs no Node type
 * declarations and no filesystem access at all: the same reason the
 * production build has no Node runtime in it applies to the check that
 * guards it. The `eager` read is what makes the source text, not just the
 * module, available to look at.
 */
const sharedSources = import.meta.glob("../**/*.{ts,tsx}", {
  query: "?raw",
  import: "default",
  eager: true
}) as Record<string, string>;

describe("platform differences are capability data, not platform-name conditionals", () => {
  it("fires on a real conditional (positive control)", () => {
    expect(
      findPlatformNameConditionals(`if (bridge.id === "ugos") { showNativePicker(); }`)
    ).toHaveLength(1);
    expect(
      findPlatformNameConditionals(`switch (id) {\n  case "synology":\n    return 1;\n}`)
    ).toHaveLength(1);
  });

  it("does not fire on a type union, on prose, or on a value being passed along", () => {
    expect(findPlatformNameConditionals(`export type PlatformId = "generic" | "ugos";`)).toEqual([]);
    expect(findPlatformNameConditionals(`// The ugos bridge reports "ugos" here.`)).toEqual([]);
    expect(findPlatformNameConditionals(`const id: PlatformId = "truenas";`)).toEqual([]);
  });

  it("finds none in the shared UI", () => {
    const files = Object.entries(sharedSources).filter(
      ([path]) => !path.endsWith("contract.conformance.test.ts")
    );
    // An empty scan is not a clean scan. If this file moved, or the glob
    // stopped resolving, "no offender was found" would be true and
    // worthless.
    expect(files.length).toBeGreaterThan(20);

    const offenders: string[] = [];
    for (const [path, source] of files) {
      for (const hit of findPlatformNameConditionals(source)) {
        offenders.push(`${path}: ${hit}`);
      }
    }
    expect(offenders).toEqual([]);
  });
});
