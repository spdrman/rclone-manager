/** Option 1a "Cycle" — the selected mark. A broken ring reads as a transfer
 *  cycle in progress and survives 16px. Colour comes from currentColor so the
 *  provider accent token drives it with no per-provider asset. */
export function Logo({ size = 24, title }: { size?: number; title?: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 48 48"
      role={title ? "img" : undefined}
      aria-hidden={title ? undefined : true}
      aria-label={title}
      style={{ color: "var(--accent)", flex: "none" }}
    >
      {title ? <title>{title}</title> : null}
      <circle
        cx="24" cy="24" r="17" fill="none" stroke="currentColor"
        strokeWidth={size <= 20 ? 5.5 : 5} strokeLinecap="round"
        strokeDasharray="47 20" transform="rotate(-52 24 24)"
      />
      <circle cx="24" cy="24" r={size <= 20 ? 6 : 5.5} fill="currentColor" />
    </svg>
  );
}

export function Wordmark({ size = 14 }: { size?: number }) {
  return (
    <span
      style={{
        fontFamily: "var(--font-mono)", fontWeight: 600,
        fontSize: size, letterSpacing: "-0.01em", whiteSpace: "nowrap"
      }}
    >
      rclone<span style={{ color: "var(--text-3)" }}>-</span>manager
    </span>
  );
}

export function Lockup({ size = 24 }: { size?: number }) {
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 10 }}>
      <Logo size={size} />
      <Wordmark size={size * 0.58 + 0} />
    </span>
  );
}
