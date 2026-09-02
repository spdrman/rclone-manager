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
      {(helpId) => (
        <PasswordInput
          label={label}
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
    expect(screen.getByRole("button", { name: "Hide password" })).toHaveAttribute("aria-pressed", "true");
  });

  it("masks again on a second activation", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.click(screen.getByRole("button", { name: "Show password" }));
    await user.click(screen.getByRole("button", { name: "Hide password" }));
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

  it("is disabled with the field it belongs to", () => {
    render(<Harness disabled />);
    expect(screen.getByRole("button", { name: "Show password" })).toBeDisabled();
  });
});
