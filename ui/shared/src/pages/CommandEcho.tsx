/**
 * The `rbm` command a UI action is equivalent to, shown beside the
 * control that produces it (EPIC G's standing rule; G2.2, #594).
 *
 * The global terminal landed with #599 and prints these lines too, from
 * the engine's own echo of the request each action made. This is not a
 * second idea of what the equivalent command is: it is the same string
 * from the same function (storageDestinationCommands.ts), which is what
 * made the two agree the moment the terminal arrived rather than needing
 * a reconciliation.
 *
 * Both surfaces stay, because they answer at different moments. This one
 * is beside the control BEFORE it is pressed, so an operator can read
 * what a button is about to do and decide not to. The terminal is the
 * record of what was actually done, after the fact, at the bottom of
 * every page.
 *
 * The lines are selectable text rather than an image or a styled
 * reconstruction, because the point is that they can be copied and typed.
 * They are also the command that actually works, byte for byte, with
 * nothing starred out: there is no flag on this CLI surface that takes a
 * secret, so there is nothing here to redact. That argument lives in full
 * in storageDestinationCommands.ts.
 */
export function CommandEcho({ label, commands }: { label: string; commands: string[] }) {
  if (commands.length === 0) return null;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      <div className="eyebrow" style={{ fontSize: 10 }}>
        {label}
      </div>
      {commands.map((c) => (
        <code
          key={c}
          className="mono"
          style={{
            display: "block",
            fontSize: "var(--text-xs)",
            color: "var(--text-2)",
            background: "var(--surface-2)",
            border: "1px solid var(--border)",
            borderRadius: "var(--radius-md)",
            padding: "5px 8px",
            overflowX: "auto",
            whiteSpace: "pre"
          }}
        >
          {c}
        </code>
      ))}
    </div>
  );
}
