import { useEffect, useId, useRef, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";

/**
 * Issue #278 — the explanatory pop-up an input carries, and the four states
 * it can be in.
 *
 * # The interaction
 *
 * Hovering an input shows its pop-up and moving away hides it again.
 * Clicking the pop-up PINS it, and a pinned pop-up stays up: that is the
 * whole point of the control, because the copy is three sentences long and
 * a hover-only pop-up cannot be read by anyone whose pointer has to travel
 * to a scrollbar. A pinned pop-up closes on a click away from it, on its
 * close control, or on Escape.
 *
 * Four visible states (hidden, hover-shown, pinned, dismissed) fall out of
 * four booleans rather than one enum, because two of them are independent
 * inputs from the environment (is the pointer inside, is the focus inside)
 * and two are the operator's own decisions (pinned, dismissed). An enum
 * would have to re-derive "is the pointer still inside" on every exit from
 * the pinned state, and getting that wrong is exactly how a pop-up ends up
 * stuck on screen.
 *
 * `dismissed` is what all three exits set, and it is also what clicking
 * the control itself sets. It is the one that is easy to leave out and is
 * not optional.
 * Without it, closing a pinned pop-up while the pointer is still over the
 * input immediately re-opens it as a hover pop-up, so the close control and
 * Escape both appear to do nothing. It is cleared on the next thing that
 * asks for the pop-up rather than on the next thing that would hide it: a
 * pointer arriving, focus arriving, or a touch tap. Clearing it on the way
 * out as well reads like belt and braces and is not — nothing can observe
 * `dismissed` while neither the pointer nor the focus is here, so a clear
 * there is a line no test can fail on.
 *
 * # Accessibility, which the interaction spec does not cover
 *
 * Hover is unavailable to a keyboard and does not exist on a touch screen,
 * so the same content arrives four ways rather than one:
 *
 *   - Focus. Focusing the control shows the pop-up, Escape dismisses it,
 *     and the close button is a real <button> that Tab reaches (it follows
 *     the control in DOM order) and Enter or Space operates. Focus moving
 *     between the control and the close button stays inside the wrapper and
 *     does not count as leaving, which is what keeps the pop-up on screen
 *     long enough to Tab into it.
 *   - Screen readers. The copy is a permanent node referenced by the
 *     control's own aria-describedby, so it is announced with the control
 *     whether or not the pop-up is on screen. It stays referenced while
 *     hidden on purpose: the accessible description computation reads a
 *     hidden node that is DIRECTLY referenced this way, which is what makes
 *     a visual pop-up and an announced description the same copy instead of
 *     two that drift. The close button sits OUTSIDE that referenced node,
 *     so its label is not read as part of the description.
 *   - Touch. There is no hover to detect, so a touch pointerdown pins
 *     directly. That is the only sensible reading of "hover shows, click
 *     pins" on a phone: one tap, and the pop-up stays until it is dismissed.
 *     A mouse pointerdown deliberately does nothing here, so clicking into
 *     a field to type does not leave a pop-up pinned over the next field.
 *   - The close control has an accessible name naming its field ("Close
 *     help for Keep"), not a bare glyph. That name is visually hidden TEXT
 *     inside the button rather than an aria-label on it, which is not a
 *     stylistic choice: an aria-label makes the button answer to "the
 *     control labelled Keep" in every label-based lookup, browser
 *     automation and assistive-technology alike, and the control labelled
 *     Keep is the field. A button should take its name from its content,
 *     which is also what survives translation and find-in-page. The glyph
 *     itself is a literal character in a string expression rather than an
 *     escape in JSX text, which is the bug #257 exists for: `×` written as
 *     element content renders as the six characters, not the symbol.
 *
 * Hiding uses the `hidden` attribute rather than unmounting, so the pop-up
 * keeps one stable id for aria-describedby across every state change, and
 * so the close button inside it is not focusable while the pop-up is not
 * on screen.
 */

/** How a caller renders its own control: it must put `helpId` on the
 *  control's aria-describedby, which is what associates the copy with it. */
export type FieldHelpRender = (helpId: string) => ReactNode;

export interface FieldHelpProps {
  /** The field's name, used for the close control's accessible name. */
  label: string;
  help: FieldHelpCopy;
  children: FieldHelpRender;
  /** Forwarded to the positioning wrapper, for callers whose control has to
   *  participate in a grid or flex layout of their own. */
  style?: CSSProperties;
}

export function FieldHelp({ label, help, children, style }: FieldHelpProps) {
  const helpId = useId();
  const wrapper = useRef<HTMLDivElement | null>(null);

  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const [pinned, setPinned] = useState(false);
  const [dismissed, setDismissed] = useState(false);

  const open = pinned || ((hovered || focused) && !dismissed);

  // A pinned pop-up closes on a click anywhere outside it. Registered in
  // the CAPTURE phase so this runs before React's own delegated handlers
  // at the root container: the alternative depends on whether a nested
  // handler stopped propagation, which is a needlessly fragile thing for
  // "did the operator click somewhere else" to rest on.
  useEffect(() => {
    if (!pinned) return;
    const onDocumentClick = (event: MouseEvent) => {
      const node = wrapper.current;
      if (node && event.target instanceof Node && node.contains(event.target)) return;
      setPinned(false);
      setDismissed(true);
    };
    document.addEventListener("click", onDocumentClick, true);
    return () => document.removeEventListener("click", onDocumentClick, true);
  }, [pinned]);

  // Escape dismisses whatever is showing. On the document rather than on
  // the wrapper, so it works for a pop-up opened by hover with the focus
  // somewhere else entirely, not only for the keyboard case.
  useEffect(() => {
    if (!open) return;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      setPinned(false);
      setDismissed(true);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [open]);

  /** Focus moving between the control and the close button is movement
   *  WITHIN this field's help, not away from it. relatedTarget is the
   *  element focus is arriving at (on focusout) or leaving from (on
   *  focusin), and it is null when focus came from or went to nowhere. */
  const staysInside = (event: React.FocusEvent<HTMLDivElement>) => {
    const other = event.relatedTarget;
    return other instanceof Node && event.currentTarget.contains(other);
  };

  const dismiss = () => {
    setPinned(false);
    setDismissed(true);
    // Put the operator back on the control they were describing rather
    // than dropping focus to the document, which is where a keyboard user
    // would otherwise have to Tab back from. The pop-up does not re-open:
    // this focus never leaves the wrapper, so `dismissed` is not cleared.
    wrapper.current?.querySelector<HTMLElement>("input, select, textarea")?.focus();
  };

  return (
    <div
      ref={wrapper}
      className="fieldhelp"
      style={style}
      onMouseEnter={() => {
        setHovered(true);
        setDismissed(false);
      }}
      onMouseLeave={() => setHovered(false)}
      onFocus={(event) => {
        if (staysInside(event)) return;
        setFocused(true);
        setDismissed(false);
      }}
      onBlur={(event) => {
        if (staysInside(event)) return;
        setFocused(false);
      }}
      onClick={() => {
        // Acting on the control puts its help away. This is not cosmetic:
        // the pop-up is a real overlay, so while it is up it covers, and
        // takes the clicks meant for, whatever sits below the field —
        // which in these forms is the Save button, the Sign in button, the
        // next row. Once the operator has clicked the control they have
        // read what they were going to read and are on their way somewhere
        // else, so this is the moment to get out of that way. Hovering or
        // focusing the field again brings it straight back.
        //
        // Every click inside this field's help lands here, the pop-up's
        // own included, and none of them needs excluding: `pinned` wins
        // over `dismissed` in `open` above, so a click that pins survives
        // the dismissal it also records. That ordering is what makes a
        // touch tap work too, since a tap is a pointerdown that pins
        // followed by a click that lands here.
        setDismissed(true);
      }}
      onPointerDown={(event) => {
        // Touch only. See the module doc: a tap is the pinned case,
        // because there is no hover to show a pop-up with first.
        if (event.pointerType !== "touch") return;
        setPinned(true);
        setDismissed(false);
      }}
    >
      {children(helpId)}

      <div
        className="fieldhelp__pop"
        hidden={!open}
        // Clicking the pop-up pins it. A click on the close button inside
        // stops before it reaches here, so closing cannot re-pin.
        onClick={() => setPinned(true)}
      >
        {/* The described node holds the copy and nothing else, so the close
            button's own label is not concatenated into the description a
            screen reader reads for the control. */}
        <div id={helpId} className="fieldhelp__body">
          <p className="fieldhelp__what">{help.what}</p>
          <p className="fieldhelp__example">
            <span className="fieldhelp__example-lead">For example</span>{" "}
            <code>{help.example}</code>
          </p>
          <p className="fieldhelp__effect">{help.effect}</p>
        </div>
        <button
          type="button"
          className="fieldhelp__close"
          onClick={(event) => {
            event.stopPropagation();
            dismiss();
          }}
        >
          <span aria-hidden="true">{"×"}</span>
          <span className="visually-hidden">{"Close help for " + label}</span>
        </button>
      </div>
    </div>
  );
}

export interface HelpFieldProps extends FieldHelpProps {
  /** Forwarded to the <label>, matching the plain `.field` usage this
   *  replaces (several call sites need a grid span or a max width). */
  labelStyle?: CSSProperties;
}

/**
 * The common case: a `.field` label with its control, plus the pop-up. This
 * is a drop-in for the
 *
 *   <label className="field"><span className="field__label">…</span>…</label>
 *
 * shape every form in this UI already uses, so adopting it on a page is a
 * wrapper change rather than a rewrite of the control inside it.
 */
export function HelpField({ label, help, children, style, labelStyle }: HelpFieldProps) {
  return (
    <FieldHelp label={label} help={help} style={style}>
      {(helpId) => (
        <label className="field" style={labelStyle}>
          <span className="field__label">{label}</span>
          {children(helpId)}
        </label>
      )}
    </FieldHelp>
  );
}
