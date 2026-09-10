import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { BackendManifest } from "@shared/api/contracts";
import { ManifestReview } from "@shared/components/manifest/ManifestReview";

/**
 * The review step (issue #669, artboard `ConfigReview`).
 *
 * It answers three questions and each one has a test below: what is
 * about to be written, whether it was proven, and whether this becomes
 * the destination new tiers start on.
 *
 * # Why the credential assertion is here and not only in the backend
 *
 * #665 shipped five negative assertions (C1-C5) keeping credential
 * material out of config.yaml, out of a terminal line and out of every
 * API response. None of them can see a React component. The remaining
 * way to leak an access key is entirely local to the browser: hold the
 * typed value in component state and let a later step render it back.
 * This screen is that later step, so the assertion belongs here.
 *
 * It is written against the WORST case deliberately. The value bag
 * handed to the component below has credential material in it, which
 * `emptyValues` structurally prevents and which nothing in the wizard
 * puts there. The point is that this component still does not print it:
 * it renders a credential field by its declared KIND and has no branch
 * that reaches for its value, so a future caller who wires the bag up
 * differently cannot turn this into a leak. Refusals by shape, never by
 * content - the same doctrine mediumcreds.go argues for on the server.
 */

const CANARY = "AKIAcanary7f3aSECRETzz";

function syntheticManifest(): BackendManifest {
  return {
    id: "widget_locker",
    label: "Widget locker",
    summary: "A backend that does not exist.",
    role: "object_store",
    rcloneBackend: "widgetlocker",
    fields: [
      { id: "locker_name", label: "Locker name", kind: "string", required: true },
      { id: "spool_directory", label: "Spool directory", kind: "path", required: true },
      { id: "control_url", label: "Control URL", kind: "url", required: false },
      {
        id: "shelf_class",
        label: "Shelf class",
        kind: "enum",
        required: false,
        unsetMeans: "OPEN_SHELF",
        values: [
          { value: "OPEN_SHELF", label: "Open shelf" },
          { value: "SEALED_CRATE", label: "Sealed crate" }
        ]
      },
      { id: "warm_the_locker", label: "Warm the locker first", kind: "bool", required: false },
      { id: "locker_key", label: "Locker key", kind: "credential", required: true },
      { id: "shelf_prefix", label: "Shelf namespace", kind: "key_prefix", required: false }
    ],
    probe: {
      steps: [
        { step: "credentials", run: true },
        { step: "reach", run: true },
        { step: "deliverable", run: true },
        { step: "write", run: true },
        { step: "read_back", run: true },
        { step: "storage_class", run: false, reason: "one shelf." },
        { step: "verification", run: true },
        { step: "delete", run: true }
      ]
    }
  };
}

function renderReview(overrides?: {
  values?: Record<string, string | boolean>;
  verified?: boolean;
  makeDefault?: boolean;
  currentDefault?: string | null;
}) {
  const onMakeDefaultChange = vi.fn();
  const { container } = render(
    <ManifestReview
      manifest={syntheticManifest()}
      destinationId="locker_one"
      values={
        overrides?.values ?? {
          locker_name: "row-14",
          spool_directory: "/mnt/lockers",
          control_url: "",
          shelf_class: "",
          warm_the_locker: true,
          shelf_prefix: "widgets/2026"
        }
      }
      credentialStored
      verified={overrides?.verified ?? true}
      makeDefault={overrides?.makeDefault ?? false}
      currentDefault={overrides?.currentDefault ?? "local"}
      onMakeDefaultChange={onMakeDefaultChange}
    />
  );
  return { container, onMakeDefaultChange };
}

function valueOf(fieldId: string): string {
  return within(screen.getByTestId(`review-row-${fieldId}`)).getByTestId("review-value").textContent ?? "";
}

afterEach(cleanup);

describe("what is about to be written", () => {
  it("renders one row per declared field, in the manifest's order", () => {
    renderReview();
    const labels = screen
      .getAllByTestId(/^review-row-/)
      .map((r) => within(r).getByTestId("review-label").textContent);
    expect(labels).toEqual([
      "Locker name",
      "Spool directory",
      "Control URL",
      "Shelf class",
      "Warm the locker first",
      "Locker key",
      "Shelf namespace"
    ]);
  });

  it("says what an unset optional field will resolve to, without claiming it was chosen", () => {
    renderReview();
    // #294 again, one screen later. "unset" is the value being written
    // and "OPEN_SHELF" is what an accessor resolves it to at read time;
    // printing only the second turns a product default into something
    // the operator appears to have picked, and the next settings save
    // freezes it into their file.
    expect(valueOf("shelf_class")).toBe("unset — read as OPEN_SHELF");
    expect(valueOf("control_url")).toBe("unset");
  });

  it("renders a bool as a word rather than as a checkbox nobody can act on", () => {
    renderReview();
    expect(valueOf("warm_the_locker")).toBe("yes");
  });
});

describe("the credential", () => {
  it("is never rendered, even when the value bag has been polluted with it", () => {
    const { container } = renderReview({
      values: {
        locker_name: "row-14",
        spool_directory: "/mnt/lockers",
        control_url: "",
        shelf_class: "",
        warm_the_locker: false,
        shelf_prefix: "widgets/2026",
        // Nothing in the wizard can put this here. That is the point:
        // this component must not be the second line of defence, it
        // must be a screen with no code path that prints a credential.
        locker_key: CANARY
      }
    });

    expect(container.textContent).not.toContain(CANARY);
    expect(valueOf("locker_key")).toBe("stored, and never shown again");
  });

  it("the canary would have been visible had the row printed its value", () => {
    // The positive control. Without it, this file would keep passing on
    // the day the credential row stops rendering at all, or the day the
    // test's own fixture stops carrying the canary - both of which make
    // the absence assertion above vacuous.
    const { container } = renderReview({
      values: {
        locker_name: CANARY,
        spool_directory: "/mnt/lockers",
        control_url: "",
        shelf_class: "",
        warm_the_locker: false,
        shelf_prefix: "widgets/2026"
      }
    });
    expect(container.textContent).toContain(CANARY);
  });
});

describe("the verification mark", () => {
  it("says what was proven when the probe passed", () => {
    renderReview({ verified: true });
    expect(screen.getByTestId("review-verification")).toHaveTextContent(
      "Verified: an object was written to this destination, read back byte for byte, and deleted."
    );
  });

  it("says nothing was proven when it did not, and does not dress it up", () => {
    renderReview({ verified: false });
    // #636's guarantee, on the surface it is decided on. An unverified
    // destination has to be distinguishable from a proven one; a review
    // screen that simply omits the mark makes the two identical.
    expect(screen.getByTestId("review-verification")).toHaveTextContent(
      "Not verified: nothing has been written to this destination and read back."
    );
  });
});

describe("making it the default", () => {
  it("states the consequence and names what stops being it", () => {
    renderReview({ currentDefault: "local" });
    expect(screen.getByTestId("review-default")).toHaveTextContent(
      "local stops being the default and becomes removable"
    );
  });

  it("is off until it is asked for, and never a side effect of saving", async () => {
    const { onMakeDefaultChange } = renderReview({ makeDefault: false });
    const user = userEvent.setup();

    const box = screen.getByRole("checkbox", { name: /default/i });
    expect(box).not.toBeChecked();

    await user.click(box);
    expect(onMakeDefaultChange).toHaveBeenCalledWith(true);
  });

  it("is not offered when this destination is already the default", () => {
    renderReview({ currentDefault: "locker_one" });
    // Offering it would be offering to do nothing, and the consequence
    // sentence would have to name this destination as the thing that
    // stops being the default, which is false.
    expect(screen.queryByTestId("review-default")).toBeNull();
  });
});
