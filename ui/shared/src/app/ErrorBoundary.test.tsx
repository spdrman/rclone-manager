/**
 * Proof that the boundary does its two jobs, which are easy to get half
 * right: it must be invisible when nothing throws, and it must produce
 * something an operator can act on when something does.
 *
 * The second case asserts on the reload control as well as the message,
 * because a message with no way forward is the blank screen with extra
 * steps.
 */
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { ErrorBoundary } from "./ErrorBoundary";

function Bomb(): never {
  throw new Error("boom");
}

describe("ErrorBoundary", () => {
  it("renders its children when nothing throws", () => {
    render(
      <ErrorBoundary>
        <span>all good</span>
      </ErrorBoundary>
    );

    expect(screen.getByText("all good")).toBeTruthy();
  });

  it("catches a render-time throw and shows an operator-facing message instead of a blank screen", () => {
    // React logs the error to the console on its own in addition to
    // componentDidCatch; silence both so the test output stays clean.
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});

    render(
      <ErrorBoundary>
        <Bomb />
      </ErrorBoundary>
    );

    expect(screen.getByText(/hit an unexpected error/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reload" })).toBeTruthy();

    consoleError.mockRestore();
  });
});
