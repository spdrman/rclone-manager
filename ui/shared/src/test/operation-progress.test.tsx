import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { OperationProgress } from "@shared/components/OperationProgress";
import type { Operation, TransferProgress } from "@shared/types/operation";

const READING: TransferProgress = {
  observedAt: "2026-08-30T09:15:00Z",
  sequence: 12,
  stage: "transferring",
  backupSetId: "alpha/nightly",
  backupSetsDone: 1,
  backupSetsTotal: 3,
  artifact: "nightly.dump",
  artifactsDone: 4,
  bytesDone: 512,
  bytesTotal: 2048,
  bytesPerSecond: 128
};

function operation(over: Partial<Operation> = {}): Operation {
  return {
    id: "op_1",
    setId: "alpha/nightly",
    setName: "Production PostgreSQL",
    kind: "transfer",
    label: "run cycle",
    status: "running",
    progress: READING,
    nonDestructive: false,
    startedAt: "2026-08-30T09:00:00Z",
    ...over
  };
}

/** Issue #221. The panel's job is to be honest about three different
 *  situations that used to render identically as a bar sitting at 0%. */
describe("OperationProgress", () => {
  it("draws the bar at the artifact's own fraction and says that is what it measures", () => {
    render(<OperationProgress operation={operation()} />);

    const bar = screen.getByRole("progressbar");
    expect(bar.getAttribute("aria-valuenow")).toBe("25");
    expect(bar.getAttribute("aria-label")).toContain("nightly.dump");
    // The bar is one artifact's, not the run's, and the caption has to say
    // so: a run cycle is a pass over every enabled set and no honest
    // percentage of the whole exists.
    expect(screen.getByText(/measures nightly.dump, not the whole run/)).toBeTruthy();
    // The position information that IS honest for the whole cycle.
    expect(screen.getByText("set 2 of 3")).toBeTruthy();
    expect(screen.getByText("4 done")).toBeTruthy();
  });

  it("marks the reported stage as current, exactly once", () => {
    render(<OperationProgress operation={operation()} />);

    const current = screen.getAllByRole("listitem").filter((li) => li.getAttribute("aria-current") === "step");
    expect(current).toHaveLength(1);
    expect(current[0].textContent).toContain("Transferring");
  });

  it("renders no bar at all for a finished operation, rather than a full or empty one", () => {
    render(<OperationProgress operation={operation({ status: "completed", progress: null })} />);

    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(screen.getByText(/reported only while an operation is running/)).toBeTruthy();
  });

  it("says why there is no reading for an operation left running by a restart, instead of drawing 0%", () => {
    render(<OperationProgress operation={operation({ status: "running", progress: null })} />);

    // The failure this test exists to prevent: a bar at zero, which reads
    // as "a transfer is running and has moved nothing".
    expect(screen.queryByRole("progressbar")).toBeNull();
    expect(screen.queryByText("0%")).toBeNull();
    expect(screen.getByText(/left behind by a restart reports none/)).toBeTruthy();
  });

  it("is indeterminate, not zero, when the artifact's size is unknown", () => {
    const sizeless: TransferProgress = { ...READING, bytesDone: 900, bytesTotal: undefined, bytesPerSecond: undefined };
    render(<OperationProgress operation={operation({ progress: sizeless })} />);

    const bar = screen.getByRole("progressbar");
    // ARIA's own encoding of "indeterminate" is an absent aria-valuenow.
    // Zero would be a measurement, and there is none.
    expect(bar.hasAttribute("aria-valuenow")).toBe(false);
    expect(screen.queryByText("0%")).toBeNull();
    expect(screen.getByText(/size is not known/)).toBeTruthy();
  });
});
