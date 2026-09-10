/**
 * The connection test as a step list (issue #669; artboards
 * `TestVerifying`, `TestVerified`, `TestRefused`).
 *
 * # One list, three renderings
 *
 * Verifying, verified and refused are not three screens and not three
 * booleans: they are this list with different things filled in. The
 * probe is `mediumcheck`'s closed, ordered vocabulary with a run-or-skip
 * decision per step, so the rows exist before the first request and the
 * outcomes arrive later. "Refused" with no step named throws away the
 * only part an operator can act on - reads succeeded and the write came
 * back refused means the credential is valid, the container is right,
 * and the policy is missing the write permission for that prefix, which
 * is most of the diagnosis - and that regression is exactly what the
 * Activity page's error box was added to stop.
 *
 * # Why the skeleton comes off the manifest and not off the report
 *
 * `MediumPreflightChecks` (pages/) draws a finished report and can draw
 * nothing before one exists, which is right for the surface it serves.
 * This one is entered before anything has been sent: a manifest declares
 * which steps this backend runs and, for each one it does not, the
 * sentence saying why, so the skipped rows and their reasons are on
 * screen from the start. That is a genuine improvement on the artboard
 * rather than a shortcut - an operator learns that a local volume has no
 * credential step before wondering why nothing checked their key - and
 * it is possible only because #665 made the skips data.
 *
 * # Nothing here animates a current step
 *
 * `TestVerifying` was drawn "showing which step it is on", and this
 * renders every runnable step as `waiting` instead. Measured, not
 * assumed: there is no `text/event-stream` anywhere in
 * apps/common/webhost, so the manager runs the steps and answers with
 * one report. A row claiming to be the step in flight would be this
 * screen asserting something it was never told, which is the one thing
 * this product does not do. What the artboard actually wanted was for a
 * long round trip not to look hung, and a live elapsed counter says that
 * without inventing knowledge.
 *
 * # The service's sentence is repeated, never rewritten
 *
 * `detail` is one of the engine's own sentences and deliberately never
 * carries an underlying error's text (FR-33). It is rendered verbatim
 * and the machine-readable `category` is shown beside it rather than
 * folded into it. Reformatting is how a surface ends up interpolating
 * what the operator typed into a refusal, which is what
 * `mediumcreds.go`'s "refusals by SHAPE, never by content" forbids -
 * and a refusal that quotes a value back is one screenshot away from
 * quoting back something pasted into the wrong box.
 */
import type { ReactNode } from "react";
import type { BackendManifest, MediumPreflight, MediumPreflightCheck } from "@shared/api/contracts";
import { Icon } from "@shared/design-system/icons";

/**
 * What one row says happened. `passed`, `failed` and `skipped` are the
 * report's own three; `not run` and `waiting` are this screen's, for the
 * moments before an answer exists, and both are worded so they cannot be
 * mistaken for a verdict.
 */
type RowOutcome = "passed" | "failed" | "skipped" | "not run" | "waiting";

interface StepRow {
  step: string;
  outcome: RowOutcome;
  detail?: string;
  category?: string;
}

export function ManifestProbeSteps({
  manifest,
  report,
  running,
  elapsedMs
}: {
  manifest: BackendManifest;
  report: MediumPreflight | null;
  running: boolean;
  /** Measured in the browser, around the request. The report carries no
   *  timing of its own, so this is honestly the round trip an operator
   *  waited through and is labelled as that rather than as the probe's
   *  own duration, which nothing reports. */
  elapsedMs: number | null;
}) {
  const rows = stepRows(manifest, report, running);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      {elapsedMs === null ? null : (
        <div
          data-testid="probe-elapsed"
          className="mono"
          style={{ fontSize: 12, color: "var(--text-3)" }}
        >
          {running ? "waiting " : "round trip "}
          {(elapsedMs / 1000).toFixed(1)}s
        </div>
      )}
      {rows.map((row) => (
        <div
          key={row.step}
          data-testid={`probe-step-row-${row.step}`}
          data-step={row.step}
          style={{ display: "flex", gap: 10, alignItems: "baseline", fontSize: 12.5 }}
        >
          <span aria-hidden="true" style={{ width: 12, textAlign: "center", color: colour(row.outcome) }}>
            {mark(row.outcome)}
          </span>
          <span className="mono" style={{ width: 110, color: "var(--text-2)" }}>
            {row.step}
          </span>
          <span
            data-testid="probe-step-outcome"
            className="mono"
            style={{ width: 90, color: colour(row.outcome) }}
          >
            {row.outcome}
          </span>
          {row.category ? (
            <span
              data-testid="probe-step-category"
              className="mono"
              style={{ color: colour(row.outcome) }}
            >
              {row.category}
            </span>
          ) : null}
          {row.detail ? (
            <span data-testid="probe-step-detail" style={{ flex: 1, color: "var(--text-2)" }}>
              {row.detail}
            </span>
          ) : null}
        </div>
      ))}
    </div>
  );
}

/**
 * The rows, which is where every decision in this file actually lives.
 *
 * Three rules, in order.
 *
 * A step the manifest declares as not run is `skipped` with the
 * manifest's own reason, whatever the report says - including in a
 * PASSING report. It is the same fact from both ends (the engine skips
 * it because the manifest said to), and the manifest's sentence is the
 * one written for an operator to read. Letting a passing report promote
 * it would put a tick beside a write nobody attempted, which is the
 * single most expensive thing this screen could say.
 *
 * A step the manifest declares as run takes its outcome from the report,
 * and before there is one says `not run`, or `waiting` while a request
 * is in flight.
 *
 * A step the REPORT carries that the manifest does not declare is
 * appended rather than dropped. #622 gave the drive on this machine a
 * ninth step, `space`, which mediumcheck emits and the wire union
 * carries while the manifest vocabulary is the eight; a renderer drawing
 * only declared rows would silently discard a real answer about a real
 * destination, and the one it discarded would be the one about running
 * out of disk.
 */
function stepRows(manifest: BackendManifest, report: MediumPreflight | null, running: boolean): StepRow[] {
  const reported: Record<string, MediumPreflightCheck> = {};
  for (const check of report?.checks ?? []) reported[check.step] = check;

  const rows: StepRow[] = manifest.probe.steps.map((declared) => {
    if (!declared.run) {
      return { step: declared.step, outcome: "skipped", detail: declared.reason };
    }
    const check = reported[declared.step];
    if (!check) {
      return { step: declared.step, outcome: running ? "waiting" : "not run" };
    }
    return {
      step: declared.step,
      outcome: check.outcome,
      detail: check.detail,
      category: check.category
    };
  });

  const declaredIds: Record<string, true> = {};
  for (const declared of manifest.probe.steps) declaredIds[declared.step] = true;
  for (const check of report?.checks ?? []) {
    if (declaredIds[check.step]) continue;
    rows.push({
      step: check.step,
      outcome: check.outcome,
      detail: check.detail,
      category: check.category
    });
  }

  return rows;
}

/**
 * A mark per outcome, and neither of the two "no answer yet" marks is a
 * picture.
 *
 * MediumPreflightChecks made this argument for `skipped` and it applies
 * to `waiting` and `not run` for the same reason: a pass and a failure
 * are verdicts and get the icons every other verdict in this app gets,
 * and everything else is the ABSENCE of a verdict. Three pictures and
 * two glyphs is what keeps a step nobody ran from reading as a soft
 * pass at a glance.
 *
 * Both switches are exhaustive with no `default`, so a sixth row state
 * is a compile error naming these two functions rather than a value that
 * renders as grey and reads as "never tried".
 */
function mark(outcome: RowOutcome): ReactNode {
  switch (outcome) {
    case "passed":
      return <Icon name="success" />;
    case "failed":
      return <Icon name="failure" />;
    case "skipped":
      return "–";
    case "waiting":
      return "…";
    case "not run":
      return "·";
  }
  const unhandled: never = outcome;
  throw new Error("unhandled probe row outcome: " + String(unhandled));
}

function colour(outcome: RowOutcome): string {
  switch (outcome) {
    case "passed":
      return "var(--ok)";
    case "failed":
      return "var(--danger)";
    case "skipped":
    case "waiting":
    case "not run":
      return "var(--text-3)";
  }
  const unhandled: never = outcome;
  throw new Error("unhandled probe row outcome: " + String(unhandled));
}
