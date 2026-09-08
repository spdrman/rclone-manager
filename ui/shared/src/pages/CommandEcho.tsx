/**
 * The `backup-manager` command a UI action is equivalent to, shown beside
 * the control that produces it (EPIC G's standing rule; G2.2, #594).
 *
 * G1.2's global terminal is where these lines are meant to end up, and it
 * does not exist yet. Rendering them here in the meantime is not a
 * placeholder for that: it is the same string from the same function
 * (storageDestinationCommands.ts), so when the terminal arrives it prints
 * what this shows rather than a second idea of what the equivalent command
 * is.
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
