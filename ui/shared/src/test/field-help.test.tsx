import { afterEach, describe, expect, it } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HelpField } from "@shared/components/FieldHelp";
import { FIELD_HELP } from "@shared/components/fieldHelpCopy";
import type { FieldHelpCopy } from "@shared/components/fieldHelpCopy";
// The real stylesheet, not a stub (vite.config.ts's `css: true`). Every
// visibility assertion below is therefore a statement about the CSS this
// project ships, which is the only place the hidden state actually lives:
// `hidden` gets its display: none from the user-agent sheet, and any
// author `display` on the same element overrides it. Without the
// .fieldhelp__pop[hidden] rule, every pop-up on a real page is permanently
// open, covering the control beneath it and swallowing clicks meant for
// it, and with a stubbed stylesheet nothing here notices.
import "@shared/design-system/components.css";

/**
 * Issue #278: the help pop-up's interaction, state by state and
 * transition by transition.
 *
 * There are four visible states (hidden, hover-shown, pinned, dismissed)
 * and three ways out of the pinned one (a click away, the close control,
 * Escape). A test that only proves "a pop-up appears" passes with pinning
 * entirely broken, which is the half of this feature that is worth
 * anything: the copy is three sentences and a pop-up that vanishes when
 * the pointer moves cannot be read. So every transition gets its own case,
 * and the pinning ones assert the negative that a naive implementation
 * gets wrong: the pop-up is STILL there after the pointer has left.
 *
 * The keyboard and screen-reader cases are here for the same reason. Hover
 * is not available to either, and a help affordance only a mouse can reach
 * is help a keyboard operator does not have.
 */

const HELP: FieldHelpCopy = FIELD_HELP.tierKeep;

function Harness() {
  return (
    <div>
      <HelpField label="Keep" help={HELP}>
        {(helpId) => <input className="input" aria-describedby={helpId} defaultValue="7" />}
      </HelpField>
      <button type="button">Somewhere else</button>
    </div>
  );
}

/** The control the pop-up explains. */
const field = () => screen.getByLabelText("Keep");
/** The pop-up itself, found through a sentence only it carries. getByText
 *  matches regardless of visibility, which is what lets the hidden state be
 *  asserted as "present but not visible" rather than as absence. */
const popup = () => screen.getByText(HELP.effect);
/** The pop-up's own box, for the assertions that are about the box rather
 *  than about the copy in it. */
const popupBox = () => popup().closest(".fieldhelp__pop") as HTMLElement;
const closeControl = () => screen.getByRole("button", { name: "Close help for Keep" });
const elsewhere = () => screen.getByRole("button", { name: "Somewhere else" });

describe("FieldHelp pop-up", () => {
  afterEach(cleanup);

  it("starts hidden, with no close control in the accessibility tree", () => {
    render(<Harness />);

    expect(popup()).not.toBeVisible();
    // toBeVisible() is satisfied by the `hidden` ATTRIBUTE alone and never
    // consults the stylesheet, so it stays green even when the shipped CSS
    // overrides the user-agent rule and leaves every pop-up on the page
    // permanently open. This is the assertion that reads the CSS, and it
    // is the one that catches that.
    expect(getComputedStyle(popupBox()).display).toBe("none");
    expect(screen.queryByRole("button", { name: "Close help for Keep" })).toBeNull();
  });

  it("shows on hover and hides again when the pointer leaves", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(field());
    expect(popup()).toBeVisible();

    await user.unhover(field());
    expect(popup()).not.toBeVisible();
  });

  it("stays up after the pointer leaves once the pop-up has been clicked", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(field());
    await user.click(popup());
    await user.unhover(popup());

    // The whole point of pinning, and the assertion an implementation that
    // only tracks hover passes every other case without.
    expect(popup()).toBeVisible();
  });

  it("closes a pinned pop-up on a click away from it", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(field());
    await user.click(popup());
    expect(popup()).toBeVisible();

    await user.click(elsewhere());
    expect(popup()).not.toBeVisible();
  });

  it("closes a pinned pop-up on its close control", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(field());
    await user.click(popup());

    await user.click(closeControl());

    // Still hovering the field: the close control has to beat the hover
    // that would otherwise re-open it immediately.
    expect(popup()).not.toBeVisible();
  });

  it("re-opens on hover after a dismissal, once the pointer has actually left", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(field());
    await user.click(popup());
    await user.click(closeControl());
    expect(popup()).not.toBeVisible();

    await user.unhover(field());
    await user.hover(field());

    expect(popup()).toBeVisible();
  });

  it("opens on keyboard focus and closes on Escape", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.tab();
    expect(field()).toHaveFocus();
    expect(popup()).toBeVisible();

    await user.keyboard("{Escape}");
    expect(popup()).not.toBeVisible();

    // Escape dismissed it; the field itself is not disturbed.
    expect(field()).toHaveFocus();
  });

  it("re-opens on focus after Escape, once the focus has actually left", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.tab();
    await user.keyboard("{Escape}");
    expect(popup()).not.toBeVisible();

    // The keyboard's version of the hover case above. Escape has to stay
    // dismissed while the field keeps focus, or Escape does nothing
    // visible; and it has to stop being dismissed once focus comes back,
    // or Escape silently turns this field's help off for the session.
    await user.tab();
    expect(elsewhere()).toHaveFocus();

    await user.tab({ shift: true });
    expect(field()).toHaveFocus();
    expect(popup()).toBeVisible();
  });

  it("lets a keyboard reach the close control and returns focus to the field", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.tab();
    expect(popup()).toBeVisible();

    // Tabbing from the field to the close control is focus moving WITHIN
    // this field's help. If that counted as leaving, the pop-up would hide
    // out from under the element being tabbed into.
    await user.tab();
    expect(closeControl()).toHaveFocus();
    expect(popup()).toBeVisible();

    await user.keyboard("{Enter}");
    expect(popup()).not.toBeVisible();
    expect(field()).toHaveFocus();
  });

  it("pins on a touch tap, because a phone has no hover to show it with", async () => {
    render(<Harness />);

    // A touch tap, spelled as the pointerdown a touch screen actually
    // sends. A mouse pointerdown carries pointerType "mouse" and is
    // deliberately inert here, so clicking into a field to type does not
    // leave a pop-up pinned over the next one.
    tap(field());
    expect(popup()).toBeVisible();

    // Pinned, so it survives the pointer going away entirely.
    const user = userEvent.setup();
    await user.unhover(field());
    expect(popup()).toBeVisible();
  });

  it("gets out of the way when the control itself is clicked", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(field());
    expect(popup()).toBeVisible();

    // The pop-up is a real overlay: while it is up it covers, and takes
    // the clicks meant for, whatever is below the field. Clicking the
    // control is the operator saying they are done reading, so the help
    // stands aside rather than sitting over the button they are heading
    // for. A mouse click must also not PIN, which is the touch gesture.
    await user.click(field());
    expect(popup()).not.toBeVisible();

    await user.unhover(field());
    act(() => field().blur());
    expect(popup()).not.toBeVisible();

    // And it is not gone for good.
    await user.hover(field());
    expect(popup()).toBeVisible();
  });

  it("keeps a pinned pop-up up when the control is clicked, so a touch tap survives its own click", async () => {
    render(<Harness />);

    // A real touch tap is a pointerdown AND a click. The pointerdown pins
    // and the click dismisses, so what keeps the pop-up up is that `pinned`
    // beats `dismissed`. Get that ordering wrong and a tap opens and closes
    // the pop-up in one gesture, and a phone never sees it at all.
    tap(field());
    fireEvent.click(field());

    expect(popup()).toBeVisible();
  });

  it("gets out of the way when the control's value changes without any click at all", () => {
    render(<Harness />);

    // Found on the wizard's private-key textarea: focusing a field opens
    // its help, and a paste, a browser autofill, or a script's own
    // .fill() changes the field's value by dispatching input/change
    // directly, never a click. Without this, a pop-up opened purely by
    // focus can sit open indefinitely over whatever the operator reaches
    // for next, since nothing else was ever going to dismiss it.
    fireEvent.focus(field());
    expect(popup()).toBeVisible();

    fireEvent.change(field(), { target: { value: "14" } });
    expect(popup()).not.toBeVisible();
  });

  it("keeps a pinned pop-up up when the control's value changes", () => {
    render(<Harness />);

    tap(field());
    fireEvent.change(field(), { target: { value: "14" } });

    expect(popup()).toBeVisible();
  });

  it("associates all three parts of the copy with the field, and nothing else", () => {
    render(<Harness />);

    const describedBy = field().getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();

    const described = document.getElementById(describedBy ?? "");
    expect(described).not.toBeNull();
    expect(described?.textContent).toContain(HELP.what);
    expect(described?.textContent).toContain(HELP.example);
    expect(described?.textContent).toContain(HELP.effect);
    // The close control sits outside the described node on purpose: its
    // label is an instruction about the pop-up, not part of the field's
    // description, and concatenating it would have a screen reader read
    // "Close help for Keep" as the last words of what Keep means.
    expect(described?.textContent).not.toContain("Close help");
  });

  it("announces the description while the pop-up is not on screen", () => {
    render(<Harness />);

    expect(popup()).not.toBeVisible();
    // Hidden, but directly referenced by aria-describedby, which is the
    // one case the accessible-description computation still reads. Without
    // this, help that is only visual is help a screen reader never gets.
    expect(field()).toHaveAccessibleDescription(/covers today and the six days before it/);
  });

  it("gives the close control a name rather than a bare glyph", async () => {
    const user = userEvent.setup();
    render(<Harness />);

    await user.hover(field());

    // "×" alone is announced as "times" or skipped entirely, depending on
    // the screen reader's punctuation setting.
    expect(closeControl()).toHaveAccessibleName("Close help for Keep");
    // #257: a Unicode escape written as JSX text renders as its six
    // literal characters. This is the fifth site that could have made
    // that mistake, so it is asserted rather than reviewed.
    expect(closeControl().querySelector("[aria-hidden='true']")?.textContent).toBe("×");
    // The name is hidden TEXT, not an aria-label. An aria-label here would
    // make this button answer to a lookup for "the control labelled Keep",
    // which is the field beside it, not this.
    expect(closeControl().getAttribute("aria-label")).toBeNull();
  });
});

describe("field help copy", () => {
  it("carries all three parts for every field, none of them an escape sequence", () => {
    const entries = Object.entries(FIELD_HELP) as [string, FieldHelpCopy][];
    expect(entries.length).toBeGreaterThan(0);

    for (const [key, copy] of entries) {
      for (const [part, text] of Object.entries(copy)) {
        expect(text, key + "." + part + " is empty").not.toBe("");
        // The #257 class of bug, caught for the whole catalogue rather
        // than one instance at a time: nothing an operator reads should
        // contain a literal backslash-u escape.
        expect(text, key + "." + part + " contains a literal escape").not.toMatch(/\\u[0-9a-fA-F]{4}/);
      }
      // An effect that merely restates what the field is for is the
      // failure mode this whole catalogue exists to avoid.
      expect(copy.effect, key + " restates `what` as its effect").not.toBe(copy.what);
    }
  });
});

/** One touch tap. `fireEvent.pointerDown` cannot carry a pointerType in
 *  jsdom, which has no PointerEvent, so the native event is built by hand
 *  and React reads pointerType straight off it. */
function tap(element: Element) {
  const event = new Event("pointerdown", { bubbles: true, cancelable: true });
  Object.defineProperty(event, "pointerType", { value: "touch" });
  act(() => {
    element.dispatchEvent(event);
  });
}
