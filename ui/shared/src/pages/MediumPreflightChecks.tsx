/**
 * A test connection's steps, rendered (G2.2, issue #594).
 *
 * # Every step, always, including the skipped ones
 *
 * Never a single OK or FAILED. A report is a fixed order of steps, and
 * the whole value of the check is in WHICH one failed: knowing that a
 * write failed after credentials and reach both passed is most of the
 * diagnosis, and collapsing that into one word throws it away.
 *
 * It used to say eight steps, and it now says however many the engine
 * sent, because the drive on this machine answers this call too (#622)
 * and its report has a `space` step a bucket has no answer for. Nothing
 * here enumerates the steps: the list comes off the report, in the
 * engine's own order, which is what lets one renderer draw both kinds of
 * destination and is why there is no second one.
 *
 * Skipped is a first-class outcome and not a quiet pass. The engine's own
 * comment on it is the reason this component exists as its own file
 * rather than as a loop inside a form: "a surface that renders a skipped
 * write as anything but 'this was never tried' has told an operator their
 * bucket is writable on the strength of a credential that was never
 * obtained". So skipped rows are drawn, with their own mark and their own
 * colour, and they are visibly not passes.
 *
 * # The category is what a surface branches on, never the detail
 *
 * `detail` is one of the engine's own sentences and deliberately never
 * carries an underlying error's text (FR-33), because that text names a
 * path on the manager's host or the name of an environment variable. The
 * machine-readable half is `category`, and it is shown beside the outcome
 * rather than buried in the sentence, because an operator scanning that
 * column is deciding whose problem it is: theirs, the provider's, or the
 * network's.
 */
import type { ReactNode } from "react";
import type { MediumPreflight, MediumPreflightCheck } from "@shared/api/contracts";
import { Icon } from "@shared/design-system/icons";

export function MediumPreflightChecks({ report }: { report: MediumPreflight }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      {report.checks.map((c) => (
        <CheckRow key={c.step} check={c} />
      ))}
    </div>
  );
}

function CheckRow({ check }: { check: MediumPreflightCheck }) {
  return (
    <div style={{ display: "flex", gap: 10, alignItems: "baseline", fontSize: 12.5 }}>
      <span aria-hidden="true" style={{ width: 12, textAlign: "center", color: markColour(check.outcome) }}>
        {mark(check.outcome)}
      </span>
      <span className="mono" style={{ width: 110, color: "var(--text-2)" }}>
        {check.step}
      </span>
      <span className="mono" style={{ width: 150, color: markColour(check.outcome) }}>
        {check.category ? `${check.outcome}(${check.category})` : check.outcome}
      </span>
      <span style={{ flex: 1, color: "var(--text-2)" }}>{check.detail}</span>
    </div>
  );
}

/**
 * Three marks for three outcomes. A skipped step gets a dash and never a
 * tick: the two must not be able to be mistaken for each other at a
 * glance, which is the entire reason skipped is reported at all.
 *
 * Two of the three are artwork after #621 and the third deliberately is
 * not. A pass and a failure are both verdicts, so they get the icons every
 * other verdict in this app gets; skipped is the ABSENCE of a verdict, and
 * a dash says "nothing was tried here" in a way no picture does. Giving it
 * one would have made the column three pictures, which is the arrangement
 * that lets a skip pass for a soft pass.
 *
 * # Why both switches are exhaustive rather than defaulted
 *
 * Both of these used to end in `default:`, which meant "skipped" and
 * anything the contract might add next rendered identically: a dash and
 * grey, which an operator reads as "this was never tried". A fourth
 * outcome on the wire would have arrived as that sentence about a step
 * that had in fact run, with no compile error anywhere to say so.
 *
 * That was survivable only while nothing could add one. Since #633 this
 * file's outcome type comes from the generated contract, so the day the
 * engine gains an outcome, tsc names these two functions instead. It is
 * ActivityStrip.statedTone's argument, applied where the same defect was
 * still live: a value nobody wrote a case for must not be able to read as
 * a verdict somebody did.
 */
function mark(outcome: MediumPreflightCheck["outcome"]): ReactNode {
  switch (outcome) {
    case "passed":
      return <Icon name="success" />;
    case "failed":
      return <Icon name="failure" />;
    case "skipped":
      return "–";
  }
  const unhandled: never = outcome;
  throw new Error("unhandled preflight outcome: " + String(unhandled));
}

function markColour(outcome: MediumPreflightCheck["outcome"]): string {
  switch (outcome) {
    case "passed":
      return "var(--ok)";
    case "failed":
      return "var(--danger)";
    case "skipped":
      return "var(--text-3)";
  }
  const unhandled: never = outcome;
  throw new Error("unhandled preflight outcome: " + String(unhandled));
}
