import { useEffect, useRef } from "react";
import type { ReactNode } from "react";

/** Destructive confirmations must name the consequence in the button (§35).
 *  "OK" is never acceptable, so confirmLabel is required. */
export function ConfirmationDialog({
  open,
  eyebrow,
  title,
  confirmLabel,
  cancelLabel = "Cancel",
  destructive = false,
  disabled = false,
  onConfirm,
  onCancel,
  children
}: {
  open: boolean;
  eyebrow?: string;
  title: string;
  confirmLabel: string;
  cancelLabel?: string;
  destructive?: boolean;
  disabled?: boolean;
  onConfirm(): void;
  onCancel(): void;
  children: ReactNode;
}) {
  const cancelRef = useRef<HTMLButtonElement>(null);

  // Focus lands on the SAFE action, never the destructive one.
  useEffect(() => {
    if (open) cancelRef.current?.focus();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onCancel]);

  if (!open) return null;

  return (
    <div className="dialog-scrim" onClick={(e) => e.target === e.currentTarget && onCancel()}>
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="confirm-title"
        className={"dialog" + (destructive ? " dialog--destructive" : "")}
        style={{ maxWidth: 476 }}
      >
        <div style={{ padding: "20px 22px 0" }}>
          {eyebrow ? (
            <div
              className="eyebrow"
              style={{
                color: destructive ? "var(--danger)" : "var(--text-3)",
                fontSize: "var(--text-xs)", letterSpacing: "0.09em"
              }}
            >
              {eyebrow}
            </div>
          ) : null}
          <h2 id="confirm-title" style={{ margin: "8px 0 0", fontSize: 18 }}>{title}</h2>
        </div>
        <div
          style={{
            padding: "14px 22px 20px", display: "flex", flexDirection: "column",
            gap: 12, fontSize: 13.5
          }}
        >
          {children}
        </div>
        <div
          className="card__footer"
          style={{ display: "flex", justifyContent: "flex-end", gap: 9, borderRadius: "0 0 10px 10px" }}
        >
          <button ref={cancelRef} className="btn" onClick={onCancel}>{cancelLabel}</button>
          <button
            className={"btn " + (destructive ? "btn--destructive-confirm" : "btn--primary")}
            onClick={onConfirm}
            disabled={disabled}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
