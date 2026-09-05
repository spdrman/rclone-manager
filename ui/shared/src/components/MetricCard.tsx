/**
 * One number with its label, sized to be read across a room.
 *
 * A right border rather than a gap between cells is what makes a row of
 * these read as one instrument panel: the enclosing `.card` hides its
 * overflow, so the last divider lands on the card's edge and vanishes,
 * and adding or removing a metric needs no change here. `children` is for
 * the metrics that carry a gauge or a badge under the figure, which is
 * why this is a component rather than a helper returning a string.
 */
export function MetricCard({
  label,
  value,
  detail,
  children
}: {
  label: string;
  value: string;
  detail?: string;
  children?: React.ReactNode;
}) {
  return (
    <div style={{ padding: "15px 20px", borderRight: "1px solid var(--border)" }}>
      <div className="eyebrow" style={{ fontSize: "var(--text-xs)" }}>{label}</div>
      <div style={{ marginTop: 6, fontSize: 24, fontWeight: 600, letterSpacing: "-0.02em" }}>
        {value}
      </div>
      {detail ? (
        <div style={{ marginTop: 4, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
          {detail}
        </div>
      ) : null}
      {children}
    </div>
  );
}
