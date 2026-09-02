import { useState } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PasswordInput } from "@shared/components/PasswordInput";
import { HelpField } from "@shared/components/FieldHelp";

/**
 * Issue #344 — the eye toggle on password fields.
 *
 * The interesting assertions here are not "does it flip the type", which is
 * a one-liner, but the three ways a reveal control goes wrong: leaking the
 * revealed state somewhere it outlives the field, coupling two fields that
 * are separate secrets to the person typing them, and naming the button in
 * a way that collides with how the e2e suite selects the input. The last
 * one is #329's defect, and it is why the accessible names here are
 * asserted rather than left to read well by eye.
 */

/** Wrapped in the real HelpField, not rendered bare, because that is how
 *  every caller composes it and because the association it provides is
 *  itself at risk: a <label> binds to its first labelable descendant, and
 *  this component puts a <button> inside one. */
function Harness({ label = "Password", disabled = false }: { label?: string; disabled?: boolean }) {
  return (
    <HelpField
      label={label}
      help={{ what: "A password.", example: "correct-horse-battery", effect: "Signs you in." }}
    >
      {(helpId, field) => (
        <PasswordInput
          label={field.label}
          labelledBy={field.id}
          value="hunter2-and-then-some"
          onChange={() => {}}
          autoComplete="current-password"
          describedBy={helpId}
          disabled={disabled}
        />
      )}
    </HelpField>
  );
}

/** The reset rules are about a value that changes, so they need a harness
 *  that owns one. Shaped like the pages that actually do this: a form whose
 *  submit handler either clears the field (SettingsPage's rotation) or
 *  leaves it alone (LoginPage's failed sign-in). */
function StatefulHarness({ clearOnSubmit }: { clearOnSubmit: boolean }) {
  const [password, setPassword] = useState("");
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (clearOnSubmit) setPassword("");
      }}
    >
      <HelpField
        label="Password"
        help={{ what: "A password.", example: "correct-horse-battery", effect: "Signs you in." }}
      >
        {(helpId, field) => (
          <PasswordInput
            label={field.label}
            labelledBy={field.id}
            value={password}
            onChange={setPassword}
            autoComplete="current-password"
            describedBy={helpId}
          />
        )}
      </HelpField>
      <button type="submit">Sign in</button>
    </form>
  );
}

afterEach(cleanup);

describe("password reveal toggle", () => {
  it("starts masked", () => {
    render(<Harness />);
    expect(screen.getByLabelText("Password", { exact: true })).toHaveAttribute("type", "password");
    expect(screen.getByRole("button", { name: "Show password" })).toHaveAttribute("aria-pressed", "false");
  });

  it("reveals the value and reports itself pressed", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Show password" }));
    expect(screen.getByLabelText("Password", { exact: true })).toHaveAttribute("type", "text");
    // aria-pressed is the whole state announcement, and the name holds
    // still. Doing both would state the same fact in two tenses, which
    // WAI-ARIA's button pattern says to pick one of, and its advice for a
    // toggle is not to rename it as its state changes.
    expect(screen.getByRole("button", { name: "Show password" })).toHaveAttribute("aria-pressed", "true");
  });

  it("masks again on a second activation", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Show password" }));
    await user.click(screen.getByRole("button", { name: "Show password" }));
    expect(screen.getByLabelText("Password", { exact: true })).toHaveAttribute("type", "password");
    expect(screen.getByRole("button", { name: "Show password" })).toHaveAttribute("aria-pressed", "false");
  });

  it("does not carry the revealed state across a remount", async () => {
    const user = userEvent.setup();
    const first = render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Show password" }));
    expect(screen.getByLabelText("Password", { exact: true })).toHaveAttribute("type", "text");
    first.unmount();

    render(<Harness />);
    expect(screen.getByLabelText("Password", { exact: true })).toHaveAttribute("type", "password");
  });

  it("keeps two fields independent, because they are separate secrets", async () => {
    const user = userEvent.setup();
    render(
      <>
        <Harness label="Password" />
        <Harness label="Confirm password" />
      </>,
    );
    await user.click(screen.getByRole("button", { name: "Show password" }));
    expect(screen.getByLabelText("Password", { exact: true })).toHaveAttribute("type", "text");
    expect(screen.getByLabelText("Confirm password", { exact: true })).toHaveAttribute("type", "password");
  });

  it("names the toggle so it cannot collide with how the e2e suite selects the input", () => {
    render(<Harness label="Confirm password" />);
    // #329's defect restated: an unanchored getByLabel("Confirm password")
    // matches anything whose accessible name merely contains the phrase. The
    // e2e suite selects these inputs with a leading-anchored prefix, so the
    // button's name must not start with the field's label.
    const name = screen.getByRole("button", { name: /confirm password/i }).getAttribute("aria-label") ?? "";
    expect(name.startsWith("Confirm password")).toBe(false);
    expect(name.startsWith("Show")).toBe(true);
  });

  it("cannot submit the form it sits inside", () => {
    render(<Harness />);
    // Inside <form>, a button with no explicit type defaults to submit, and
    // every one of these fields lives in a form with a real submit button.
    expect(screen.getByRole("button", { name: "Show password" })).toHaveAttribute("type", "button");
  });

  it("keeps the input's own accessible name to the field label alone", () => {
    render(<Harness label="Confirm password" />);
    // Measured in Chromium over the exact DOM these components emit: a
    // <label> that wraps an embedded control folds that control's own text
    // alternative into the name it gives the field, so without this the
    // input answers to "Confirm password Show confirm password Passwords do
    // not match." rather than to its label. Neither jsdom nor Playwright's
    // getByLabel can see that, because both read the wrapping label's
    // textContent, which an aria-hidden SVG button contributes nothing to.
    // So the assertion is on the mechanism that fixes it: an explicit
    // aria-labelledby, which the accname algorithm resolves before it ever
    // walks the label's subtree.
    const input = screen.getByLabelText(/^Confirm password/);
    const ids = (input.getAttribute("aria-labelledby") ?? "").split(" ").filter(Boolean);
    expect(ids.length).toBeGreaterThan(0);
    const named = ids.map((id) => document.getElementById(id));
    expect(named.every((n) => n !== null)).toBe(true);
    expect(named.map((n) => n?.textContent).join(" ")).toBe("Confirm password");
  });

  it("stays out of the browser features that skip a password field", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    const input = screen.getByLabelText(/^Password/);
    // Chrome's and Edge's enhanced spellcheck send the contents of a text
    // field to a remote service and exempt type=password explicitly, so a
    // revealed field is the state where these three matter: revealing is
    // what makes the secret eligible to leave the machine. Autocapitalize
    // is the mobile half of the same problem, silently upper-casing the
    // first character of a password typed while it is readable.
    // BackupSetWizardPage does exactly this for the private-key field.
    for (const [attr, value] of [["spellcheck", "false"], ["autocorrect", "off"], ["autocapitalize", "off"]]) {
      expect(input).toHaveAttribute(attr, value);
    }
    await user.click(screen.getByRole("button", { name: "Show password" }));
    expect(input).toHaveAttribute("type", "text");
    for (const [attr, value] of [["spellcheck", "false"], ["autocorrect", "off"], ["autocapitalize", "off"]]) {
      expect(input).toHaveAttribute(attr, value);
    }
  });

  it("re-masks when the value it was revealing is cleared", async () => {
    const user = userEvent.setup();
    render(<StatefulHarness clearOnSubmit />);
    const input = screen.getByLabelText(/^Password/);

    await user.type(input, "a-long-enough-passphrase");
    await user.click(screen.getByRole("button", { name: "Show password" }));
    expect(input).toHaveAttribute("type", "text");

    // SettingsPage's rotation: on success it sets all three fields back to
    // "" without unmounting anything, which used to leave the operator with
    // empty fields still in type="text", ready for the NEXT password to be
    // typed in the clear.
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(input).toHaveValue("");
    expect(input).toHaveAttribute("type", "password");
  });

  it("re-masks on submit even when the value survives it", async () => {
    const user = userEvent.setup();
    render(<StatefulHarness clearOnSubmit={false} />);
    const input = screen.getByLabelText(/^Password/);

    await user.type(input, "a-long-enough-passphrase");
    await user.click(screen.getByRole("button", { name: "Show password" }));
    expect(input).toHaveAttribute("type", "text");

    // LoginPage's failed sign-in: no remount, no clear, so the password
    // stayed readable on screen through the failure and for as long as the
    // operator left the page open afterwards.
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(input).toHaveValue("a-long-enough-passphrase");
    expect(input).toHaveAttribute("type", "password");
  });

  it("is disabled with the field it belongs to", () => {
    render(<Harness disabled />);
    expect(screen.getByRole("button", { name: "Show password" })).toBeDisabled();
  });
});
