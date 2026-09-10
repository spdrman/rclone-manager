/**
 * What a run control's last answer looked like, above the page it was
 * pressed on.
 *
 * This was written as an interim surface with a deliberate expiry, and
 * the expiry has arrived: the per-set terminal (G1.3) and the global one
 * (G1.2) both read the same browserNotices node this renders from, so
 * this banner could now go without either of them changing.
 *
 * It is kept for one reason, and it is not inertia. This sits at the top
 * of the page the button is on; the terminal is a strip at the bottom of
 * the window that an operator is free to collapse, and a refusal that
 * only appears in a collapsed panel is a press that did nothing again.
 * Retiring it is a decision about screens and belongs with whoever makes
 * that one.
 *
 * What it is not, and must never become, is a second vocabulary: every
 * word it shows comes from the notice, and the notice was written once by
 * useRunControls from the service's own typed code.
 *
 * It shows only the newest notice, on purpose. A page is not a log, and a
 * stack of banners is how a surface stops being read; the ring behind it
 * keeps the history for the terminals.
 */
import { WarningBanner } from "@shared/components/WarningBanner";
import type { BrowserNotice } from "@shared/state/browserNotices";

const TONE = { ok: "ok", refused: "danger", unreachable: "warn" } as const;

const EYEBROW = {
  ok: "Run started",
  refused: "Run refused",
  unreachable: "No answer"
} as const;

export function RunControlNotice({ notice }: { notice: BrowserNotice | null }) {
  if (notice === null) return null;
  return (
    <WarningBanner tone={TONE[notice.outcome]} eyebrow={EYEBROW[notice.outcome]} title={notice.message}>
      {notice.remediation ? <p style={{ margin: 0 }}>{notice.remediation}</p> : null}
      {/* The command this button is equivalent to, copy-pasteable exactly
          as shown. It is printed on a refusal too, and that is the point
          rather than a nicety: while the destructive gate is shut this is
          the only remaining way to start the backup that was just
          refused. */}
      {notice.command ? (
        <pre
          style={{
            margin: "6px 0 0",
            padding: "6px 8px",
            background: "var(--surface-2, rgba(0,0,0,0.04))",
            borderRadius: 4,
            fontSize: 12,
            overflowX: "auto"
          }}
        >
          $ {notice.command}
        </pre>
      ) : null}
      {/* Behind the sentence rather than in it: an id is what somebody
          copies into a support message, and it is worth nothing in a
          banner an operator is trying to read. Absent when the request
          never reached the service, because an id that matches no log
          line is worse than none (#274). */}
      {notice.correlationId ? (
        <div style={{ marginTop: 6, fontSize: 12, color: "var(--text-3)" }}>
          Correlation id: <code>{notice.correlationId}</code>
        </div>
      ) : null}
    </WarningBanner>
  );
}
