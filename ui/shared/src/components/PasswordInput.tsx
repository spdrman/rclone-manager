import { useState } from "react";

/** A password field with a reveal toggle (issue #344).
 *
 *  Three properties are load-bearing and easy to lose in a refactor.
 *
 *  The revealed flag is local state and deliberately nothing else. Not a
 *  prop, not context, not storage. That is what makes it reset on remount
 *  and makes two of these independent of each other without any wiring:
 *  the password and its confirmation are separate secrets to the person
 *  typing them, and a shared flag would reveal one while they check the
 *  other. Nothing here copies the value anywhere; only the input's `type`
 *  changes.
 *
 *  The toggle's accessible name starts with the verb, never with the field
 *  label. `getByLabel` matches on a substring by default, and the e2e suite
 *  selects these inputs by their label with a leading-anchored prefix, so a
 *  button named "Confirm password visibility" would resolve alongside the
 *  input it sits next to and fail Playwright's strict mode. That is exactly
 *  the defect behind #329, and rclone-manager-tests#19 scoped the locators
 *  for this change on the same reasoning. `aria-pressed` carries the state
 *  rather than the name alone flipping, so a screen reader user can query
 *  whether the field is currently revealed instead of inferring it.
 *
 *  `type="button"` is explicit because every one of these fields sits in a
 *  form with a real submit button, and a button with no type inside a form
 *  submits it. */
export function PasswordInput({
  label,
  value,
  onChange,
  autoComplete,
  describedBy,
  disabled = false,
  required = false,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  autoComplete: string;
  describedBy?: string;
  disabled?: boolean;
  required?: boolean;
}) {
  const [revealed, setRevealed] = useState(false);
  const action = revealed ? "Hide" : "Show";

  return (
    <span className="password-field">
      {/* First child on purpose: a <label> binds to its first labelable
          descendant, and HelpField wraps this whole thing in one. If the
          button came first it would steal the association and the field
          would lose its name. */}
      <input
        className="input password-field__input"
        type={revealed ? "text" : "password"}
        aria-describedby={describedBy}
        autoComplete={autoComplete}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        required={required}
      />
      <button
        type="button"
        className="password-field__toggle"
        aria-label={action + " " + label.toLowerCase()}
        aria-pressed={revealed}
        disabled={disabled}
        onClick={() => setRevealed((shown) => !shown)}
      >
        <EyeIcon open={revealed} />
      </button>
    </span>
  );
}

/** Decorative: the button already carries the name and the state, so a
 *  second announcement here would only repeat them. Colour comes from
 *  currentColor so the theme drives it, matching Logo.tsx. */
function EyeIcon({ open }: { open: boolean }) {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden={true} focusable="false">
      <path d="M1.8 12S5.4 5.4 12 5.4 22.2 12 22.2 12 18.6 18.6 12 18.6 1.8 12 1.8 12Z" />
      <circle cx="12" cy="12" r="3.2" />
      {open ? <path d="M3.5 3.5 20.5 20.5" /> : null}
    </svg>
  );
}
