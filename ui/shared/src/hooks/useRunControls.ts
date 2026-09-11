/**
 * The one place a run is submitted from the browser, and the one place a
 * refusal is turned into something an operator can read.
 *
 * # What this exists to stop happening again
 *
 * Before it, the two live run buttons each did
 * `api.runCycle(version.data?.configRevision ?? "").then(reload)` and the
 * third had no handler at all. Every part of that line was wrong in a way
 * nothing could see (issue #597):
 *
 *   - no `.catch`, so every refusal was an unhandled promise rejection.
 *     A 403 from the destructive gate, a 400 for the missing idempotency
 *     key, a 409 for a stale revision and a dead engine were all pixel-
 *     identical to the button that did nothing.
 *   - `?? ""`, so a press before the version fetch resolved sent an empty
 *     configuration revision, which the service refuses outright.
 *   - no idempotency key, because the `post` helper had no way to send a
 *     header, so the request was malformed on every deployment.
 *
 * # One key per submission, reused on retry
 *
 * `Idempotency-Key` describes the retry, not the operation. So a key is
 * minted when an operator asks for something, and the SAME key is sent
 * again if they press the button again after a refusal: that is what lets
 * the service recognise a dropped response as the same intent instead of
 * starting a second backup run. A fresh key per attempt would be worse
 * than no key at all. The key is cleared on success, so the next press is
 * a new logical submission and gets a new one.
 *
 * # Refusals are rendered by CODE, never by status
 *
 * 403 is both DESTRUCTIVE_OPERATIONS_DISABLED and a stale CSRF token, and
 * 409 covers three refusals an operator resolves in three different
 * places. Every branch below keys on the typed code the service sent.
 *
 * # Where the refusal goes
 *
 * Into browserNotices (state/), which is the seam the global terminal
 * (G1.2) and the per-set terminal (G1.3) read. A deployment-wide refusal
 * names every enabled set, because every one of them is a set the
 * operator just asked to have backed up and did not.
 *
 * That sentence was half true for two releases: the per-set terminal read
 * the seam and the global one did not, so a press answered here reached a
 * banner and nothing else. Both read it now, and ActivityDock's own tests
 * assert a browser line through the dock's filters, which is what makes
 * this paragraph a claim something can fail on rather than a description.
 */
import { useCallback, useMemo, useRef, useState } from "react";

import { useApi } from "@shared/api/ApiContext";
import { newIdempotencyKey } from "@shared/api/client";
import { apiErrorOf } from "@shared/api/failure";
import type { ApiErrorCode } from "@shared/api/contracts";
import { useCausl } from "@shared/state/graph";
import { setsNode, versionNode } from "@shared/state/appNodes";
import { browserNoticesNode, emitBrowserNotice } from "@shared/state/browserNotices";
import type { BrowserNotice } from "@shared/state/browserNotices";

/** What an operator asked for: every enabled set, or exactly one. */
export type RunScope = { kind: "all" } | { kind: "set"; id: string };

/** The command line each scope is equivalent to, copy-pasteable exactly
 *  as written and naming the set by its full source/backup-set id.
 *
 *  Neither form can carry a credential: the connection details live in
 *  the configuration, and the browser holds a session cookie rather than
 *  the administrator's password.
 *
 *  Neither carries `--config` either, and that is a gap rather than a
 *  decision: nothing on the wire tells this build where the deployment's
 *  configuration actually is, so the line shown is the packaged-default
 *  form. On an install that moved it, the command still names the right
 *  work and would need the flag added by hand. */
export function commandFor(scope: RunScope): string {
  return scope.kind === "all" ? "backupd run" : "backupd fetch --backup-set " + scope.id;
}

/** How a refusal reads to an operator, chosen by the service's own typed
 *  code rather than by a status. */
interface Refusal {
  message: string;
  remediation?: string;
}

/**
 * The refusals a run control can produce, each in the words that send an
 * operator to the right place.
 *
 * The gate gets its own sentence rather than a raw code because it is not
 * a transient failure and retrying will not help: until #92 lands, a run
 * has to be started from the command line, and the command this control
 * printed is the only thing left that works.
 */
export function describeRunRefusal(code: ApiErrorCode | "unknown", serviceMessage: string, scope: RunScope): Refusal {
  switch (code) {
    case "DESTRUCTIVE_OPERATIONS_DISABLED":
      return {
        message: "This deployment will not start a backup run.",
        remediation:
          "Nothing was submitted and nothing is queued. Destructive operations are disabled until the trusted-proxy authentication gate has been verified for this deployment, so retrying will not help. Run " +
          commandFor(scope) +
          " from a shell on this host instead."
      };
    case "OPERATION_ALREADY_RUNNING":
      return {
        message: "A backup run is already in progress.",
        remediation:
          "A run of one backup set and a run of every set cannot overlap, so this one was refused rather than queued. Wait for the run in flight to finish, then ask again."
      };
    case "BACKUP_SET_HELD_FOR_EDITING":
      return {
        message: "This backup set is being edited.",
        remediation:
          "A run against a definition somebody is changing is two writers on one backup set. Leave edit mode, here or in the other tab it is open in, then ask again."
      };
    case "CONFIG_REVISION_STALE":
      return {
        message: "The configuration changed while this page was open.",
        remediation:
          "Nothing was run. Reload the page so you are looking at the configuration this run would use, then ask again."
      };
    case "IDEMPOTENCY_KEY_CONFLICT":
      return {
        message: "This request reused a key from a different submission.",
        remediation: "Reload the page and ask again. Nothing was run."
      };
    case "BACKUP_SET_NOT_FOUND":
      return {
        message: "This backup set is no longer configured.",
        remediation: "It was removed while this page was open. Reload the page."
      };
    case "CSRF_TOKEN_MISSING":
    case "CSRF_TOKEN_MISMATCH":
      return {
        message: "This page's security token is missing or out of date.",
        remediation: "Reload the page, then ask again. Nothing was run."
      };
    case "UNAUTHENTICATED":
      return { message: "This session has expired.", remediation: "Sign in again, then ask again." };
    case "NOT_CONFIGURED":
      return {
        message: "There is nothing to back up yet.",
        remediation: "This instance has no configuration, so there are no backup sets for a run to visit."
      };
    default:
      // What the service said, because a reason nobody anticipated still
      // beats a reason invented here (api/failure.ts's rule).
      return { message: serviceMessage || "The backup service refused this run." };
  }
}

export interface RunControls {
  /** A submission is in flight. */
  busy: boolean;
  /**
   * Whether a run may be submitted at all right now.
   *
   * False until GET /system/version has resolved, because the submission
   * carries the configuration revision that read supplies and sending an
   * empty one is refused by the service. This is the guard that replaces
   * `?? ""`: refusing here says why, and sending "" said nothing.
   */
  ready: boolean;
  /** Runs every enabled backup set. */
  runAll: () => void;
  /** Runs exactly one backup set, by its full source/backup-set id. */
  runBackupSet: (id: string) => void;
  /** The newest notice about this scope, or null. */
  notice: BrowserNotice | null;
}

/**
 * `scope` is what this page's notices are about: the whole deployment
 * for the dashboard and the list, one set for a detail page. It decides
 * which notices come back on `notice`, and nothing else: a page scoped to
 * one set can still submit a deployment-wide run, which is what both
 * existing copies of that button do.
 */
export function useRunControls(scope: RunScope = { kind: "all" }): RunControls {
  const api = useApi();
  const version = useCausl(versionNode);
  const sets = useCausl(setsNode);
  const notices = useCausl(browserNoticesNode);
  const [busy, setBusy] = useState(false);

  // The key for the submission currently being retried, per scope. It
  // outlives a render on purpose: pressing the button again after a
  // refusal is the SAME logical submission, and the whole value of the
  // header is that the service can see that.
  const pendingKeys = useRef(new Map<string, string>());

  const configRevision = version.data?.configRevision;
  const ready = typeof configRevision === "string" && configRevision !== "";

  // Every set a deployment-wide run would actually visit. A refusal lands
  // on each of them, and on no others: telling a disabled set its backup
  // was refused would be a false alarm about a set that was never going
  // to run.
  const enabledSetIds = useMemo(() => (sets.data ?? []).filter((s) => s.enabled).map((s) => s.id), [sets.data]);

  const submit = useCallback(
    (target: RunScope) => {
      const command = commandFor(target);
      const backupSetIds = target.kind === "set" ? [target.id] : enabledSetIds;
      const keyFor = target.kind === "set" ? "set:" + target.id : "all";

      if (!ready) {
        // Not a request that failed: a request this page refused to make.
        // It is a notice rather than silence because the button being
        // pressed and nothing happening is the exact experience this
        // whole issue is about.
        emitBrowserNotice({
          outcome: "refused",
          code: "unknown",
          message: "This page has not finished loading.",
          remediation:
            "The configuration revision a run is checked against has not arrived yet, and a run submitted without one is refused. Wait a moment, then ask again.",
          backupSetIds,
          command
        });
        return;
      }

      const key = pendingKeys.current.get(keyFor) ?? newIdempotencyKey();
      pendingKeys.current.set(keyFor, key);

      setBusy(true);
      const request =
        target.kind === "all"
          ? api.runCycle(configRevision, key)
          : api.runBackupSet(target.id, configRevision, key);

      request.then(
        () => {
          // Done with this submission, so the next press is a new one.
          pendingKeys.current.delete(keyFor);
          setBusy(false);
          emitBrowserNotice({
            outcome: "ok",
            code: "unknown",
            message: target.kind === "all" ? "Started a run of every enabled backup set." : "Started a run of this backup set.",
            backupSetIds,
            command
          });
        },
        (e: unknown) => {
          setBusy(false);
          // The key is deliberately KEPT: pressing the button again is
          // the same submission retried, which is what the header is for.
          const failure = apiErrorOf(e);
          const code = failure?.code ?? "unknown";
          const refusal = describeRunRefusal(code, failure?.message ?? "", target);
          emitBrowserNotice({
            outcome: failure === null ? "unreachable" : "refused",
            code,
            message: failure === null ? "Backupd did not answer." : refusal.message,
            // Deliberately does NOT claim nothing was run when there was
            // no reply. A request that got no answer may still have been
            // carried out with only the response lost, and claiming
            // otherwise would be this module's own version of the defect
            // it exists to fix (api/failure.ts makes the same distinction).
            remediation:
              failure === null
                ? "The request got no reply at all, so whether the run started is unknown. Check that the Backupd service is still running, then ask again."
                : refusal.remediation,
            correlationId: failure?.correlationId,
            backupSetIds,
            command
          });
        }
      );
    },
    [api, configRevision, enabledSetIds, ready]
  );

  // Narrowed to a plain string so the memo below depends on a value
  // rather than on an object literal a caller rebuilds every render.
  const scopeId = scope.kind === "set" ? scope.id : "";
  const notice = useMemo(() => {
    const mine = scopeId === "" ? notices : notices.filter((n) => n.backupSetIds.includes(scopeId));
    return mine.length === 0 ? null : mine[mine.length - 1];
  }, [notices, scopeId]);

  return {
    busy,
    ready,
    runAll: useCallback(() => submit({ kind: "all" }), [submit]),
    runBackupSet: useCallback((id: string) => submit({ kind: "set", id }), [submit]),
    notice
  };
}
