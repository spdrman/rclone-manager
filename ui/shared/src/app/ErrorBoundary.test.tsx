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
