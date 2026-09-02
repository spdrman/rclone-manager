import { useEffect, useRef, useState } from "react";

/** A password field with a reveal toggle (issue #344).
 *
 *  # What the e2e suite has to know
 *
 *  This component puts a second control inside the field's `<label>`, which
 *  changes how the field can be selected, so the contract is written here
 *  rather than left for a locator author to rediscover:
 *
 *    - Select the INPUT with a leading-anchored regex on the field label,
 *      `getByLabel(/^Confirm password/)`. What is banned is the bare
 *      substring: `getByLabel` is a case-insensitive substring match by
 *      default, so `getByLabel("Confirm password")` also matches this
 *      button's own "Show confirm password" and fails Playwright's strict
 *      mode. `{ exact: true }` works too, but only because of the
 *      `labelledBy` below, which is what stops the name growing to
 *      "Confirm passwordPasswords do not match." when a validation message
 *      renders inside the same label. The leading anchor is the shape to
 *      reach for because it survives that either way, and it is what the
 *      suite uses.
 *    - Select the TOGGLE by role and its full name, "Show password". That
 *      name does NOT change when the field is revealed, so a locator does
 *      not have to know which state the field is in; `aria-pressed` is what
 *      moves. The verb comes first on purpose: a name starting with the
 *      field's own label is what makes an input locator resolve two
 *      elements, which is #329's defect exactly.
 *
 *  # Why this diverges from FieldHelp's rule about aria-label
 *
 *  FieldHelp names its close button with visually hidden TEXT and says why
 *  in its module doc: an aria-label makes a button answer to "the control
 *  labelled Keep" in every label-based lookup. That reasoning is right, and
 *  it points the other way here. This button sits INSIDE the field's own
 *  `<label>`, where hidden text is not hidden from anything that matters:
 *  it lands in the label's textContent, which is exactly what `getByLabel`
 *  and Testing Library's `getByLabelText` read, so it would corrupt the
 *  input's own lookup rather than stay out of the way. An aria-label keeps
 *  the button's name off the label's text entirely. Divergence on purpose,
 *  for a trade that genuinely is different inside a wrapping label.
 *
 *  # The properties that are load-bearing
 *
 *  `labelledBy` is not optional decoration. A `<label>` that wraps its
 *  control names it by walking its own subtree, and the accessible-name
 *  algorithm folds an embedded control's own text alternative into that
 *  walk. Measured in Chromium over this exact DOM: without an explicit
 *  aria-labelledby the input answers to "Password Show password Minimum 12
 *  characters." rather than to "Password". Neither jsdom nor Playwright's
 *  `getByLabel` can see that, because both read the label's textContent and
 *  an aria-hidden SVG contributes nothing to it, which is why the browser
 *  had to be the one to settle it.
 *
 *  The revealed flag is local state and deliberately nothing else. Not a
 *  prop, not context, not storage. That makes two of these independent of
 *  each other without any wiring: the password and its confirmation are
 *  separate secrets to the person typing them, and a shared flag would
 *  reveal one while they check the other. Nothing here copies the value
 *  anywhere; only the input's `type` changes.
 *
 *  Revealing is a state you finish with, so it ends on its own two ways: a
 *  submit, and the value going empty. A remount clears it too, but nothing
 *  in this app remounts these fields, so a remount is not the reset that
 *  matters. SettingsPage's rotation sets all three fields back to "" while
 *  they stay mounted, and LoginPage re-renders through a failed sign-in
 *  without unmounting, so both of the resets that actually happen are
 *  logical ones. Together they mean a revealed field cannot outlive the
 *  moment it was revealed for.
 *
 *  The state is carried by `aria-pressed` alone, and the name is fixed.
 *  Doing both, which this started out doing, states the same fact twice in
 *  two tenses: a name saying what the next activation will do, beside a
 *  pressed state saying what the current one is, announces as "Hide
 *  password, toggle button, pressed". WAI-ARIA's button pattern says to
 *  pick one, and its advice for a toggle is explicit about which: do not
 *  change a toggle button's label as its state changes, name it for the
 *  thing being toggled and let `aria-pressed` move. That also happens to be
 *  the half a screen reader announces on its own when the button is
 *  activated while focused, which a silently renamed plain button is not.
 *  There is no visible text to keep in sync, only the eye glyph, which
 *  gains its slash when the field is revealed.
 *
 *  `type="button"` is explicit because every one of these fields sits in a
 *  form with a real submit button, and a button with no type inside a form
 *  submits it.
 *
 *  The toggle is disabled with the field, which reads like a loss and is
 *  not one here: the only disabled case in this app is SettingsPage under
 *  `readOnly`, where all three fields are empty and cannot be typed into,
 *  so there is never a value behind the mask to reveal. It is a decision,
 *  not an oversight, and it is the one to revisit first if a disabled
 *  field ever arrives holding a value. */
export function PasswordInput({
  label,
  labelledBy,
  value,
  onChange,
  autoComplete,
  describedBy,
  disabled = false,
  required = false,
}: {
  label: string;
  /** The id of the field's own label, from HelpField's render argument. */
  labelledBy: string;
  value: string;
  onChange: (value: string) => void;
  autoComplete: string;
  describedBy?: string;
  disabled?: boolean;
  required?: boolean;
}) {
  const [revealed, setRevealed] = useState(false);
  const input = useRef<HTMLInputElement | null>(null);

  // Adjusting state during render rather than in an effect, which is the
  // pattern React documents for "reset when a prop changes": an effect
  // would paint one frame of an empty field still in type="text" before it
  // corrected itself.
  const [lastValue, setLastValue] = useState(value);
  if (value !== lastValue) {
    setLastValue(value);
    if (value === "") setRevealed(false);
  }

  useEffect(() => {
    // The submit half of the same rule. Listening on the form rather than
    // taking an onSubmit prop keeps it true at every call site by
    // construction instead of by six call sites remembering, and the event
    // fires whether or not the handler calls preventDefault, which every
    // one of these forms does.
    const form = input.current?.form;
    if (!form) return;
    const mask = () => setRevealed(false);
    form.addEventListener("submit", mask);
    return () => form.removeEventListener("submit", mask);
  }, []);

  return (
    <span className="password-field">
      {/* First child on purpose: a <label> binds to its first labelable
          descendant, and HelpField wraps this whole thing in one. The
          explicit aria-labelledby below means the input would keep its NAME
          either way now, but the binding is more than the name: it is also
          what makes clicking the label text focus the field. Put the button
          first and that click activates the toggle instead. */}
      <input
        ref={input}
        className="input password-field__input"
        type={revealed ? "text" : "password"}
        aria-labelledby={labelledBy}
        aria-describedby={describedBy}
        autoComplete={autoComplete}
        // Revealing is what makes a secret eligible to leave the machine:
        // Chrome's and Edge's enhanced spellcheck transmit the contents of
        // a type=text field to a remote service and exempt type=password
        // explicitly, so the moment this field becomes readable it also
        // becomes sendable. Autocapitalize is the mobile half, quietly
        // upper-casing the first character of a password typed while it is
        // revealed and turning it into a wrong-password failure with no
        // visible cause. BackupSetWizardPage's private-key field already
        // carries all three for the same reason (#98).
        spellCheck={false}
        autoCorrect="off"
        autoCapitalize="off"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        required={required}
      />
      <button
        type="button"
        className="password-field__toggle"
        aria-label={"Show " + label.toLowerCase()}
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
 *  second announcement here would only repeat them. The slash is the
 *  sighted half of what aria-pressed says. Colour comes from currentColor
 *  so the theme drives it, matching Logo.tsx. */
function EyeIcon({ open }: { open: boolean }) {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden={true} focusable="false">
      <path d="M1.8 12S5.4 5.4 12 5.4 22.2 12 22.2 12 18.6 18.6 12 18.6 1.8 12 1.8 12Z" />
      <circle cx="12" cy="12" r="3.2" />
      {open ? <path d="M3.5 3.5 20.5 20.5" /> : null}
    </svg>
  );
}
