import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import type { BackendManifest, MediumPreflight } from "@shared/api/contracts";
import { ManifestProbeSteps } from "@shared/components/manifest/ManifestProbeSteps";

/**
 * The connection test drawn as a step list, not as three booleans
 * (issue #669; the probe vocabulary is #665's `Probe.Steps`).
 *
 * `TestVerifying`, `TestVerified` and `TestRefused` are three renderings
 * of ONE list, which is why there is one component and three tests
 * rather than three components. The whole value of the check is in which
 * step answered what: knowing that reads succeeded and the write came
 * back AccessDenied is most of the diagnosis, and "refused" on its own
 * throws away the only part an operator can act on. That is the exact
 * regression the Activity page's error box was added to stop (#669's own
 * text), so it is asserted here.
 *
 * The list's skeleton comes off the MANIFEST and not off the report, and
 * that is the difference from MediumPreflightChecks, which draws a
 * finished report and can draw nothing before one exists. A manifest
 * declares which steps this backend runs and, for each one it does not,
 * the sentence saying why, so the skipped rows and their reasons are on
 * screen before the first request is sent and stay there afterwards.
 */

/** A synthetic backend, for the reason manifest-field-renderer.test.tsx
 *  gives: a step list that renders for a backend nobody wrote code for
 *  is a step list driven by data. Two declared skips, so the reason
 *  rendering is tested against a manifest that carries two different
 *  sentences and cannot pass by printing one of them everywhere. */
function syntheticManifest(): BackendManifest {
  return {
    id: "widget_locker",
    label: "Widget locker",
    summary: "A backend that does not exist.",
    role: "object_store",
    fields: [{ id: "locker_name", label: "Locker name", kind: "string", required: true }],
    probe: {
      steps: [
        {
          step: "credentials",
          run: false,
          reason: "a widget locker is opened with a key cut on this machine, so there is no credential to obtain."
        },
        { step: "reach", run: true },
        { step: "deliverable", run: true },
        { step: "write", run: true },
        { step: "read_back", run: true },
        { step: "storage_class", run: false, reason: "a widget locker has one shelf, so nothing could have landed on a different one." },
        { step: "verification", run: true },
        { step: "delete", run: true }
      ]
    }
  };
}

const DECLARED_ORDER = [
  "credentials",
  "reach",
  "deliverable",
  "write",
  "read_back",
  "storage_class",
  "verification",
  "delete"
];

function rowsInOrder(): string[] {
  return screen.getAllByTestId(/^probe-step-row-/).map((row) => row.getAttribute("data-step") ?? "");
}

function row(step: string) {
  return screen.getByTestId(`probe-step-row-${step}`);
}

function outcomeOf(step: string): string {
  return within(row(step)).getByTestId("probe-step-outcome").textContent ?? "";
}

afterEach(cleanup);

describe("before the check has been asked for", () => {
  it("renders every declared step, in the manifest's order", () => {
    render(<ManifestProbeSteps manifest={syntheticManifest()} report={null} running={false} elapsedMs={null} />);
    expect(rowsInOrder()).toEqual(DECLARED_ORDER);
  });

  it("says a runnable step has not run, and never that it passed", () => {
    render(<ManifestProbeSteps manifest={syntheticManifest()} report={null} running={false} elapsedMs={null} />);
    expect(outcomeOf("write")).toBe("not run");
  });

  it("shows a declared skip and its reason before anything has been sent", () => {
    render(<ManifestProbeSteps manifest={syntheticManifest()} report={null} running={false} elapsedMs={null} />);

    // The reason is the manifest's, in the manifest's words. A skipped
    // step with no reason is the shape mediumcheck's own comment refuses:
    // "a surface that renders a skipped write as anything but 'this was
    // never tried' has told an operator their bucket is writable on the
    // strength of a credential that was never obtained".
    expect(outcomeOf("credentials")).toBe("skipped");
    expect(
      within(row("credentials")).getByText(
        "a widget locker is opened with a key cut on this machine, so there is no credential to obtain."
      )
    ).toBeInTheDocument();
    expect(
      within(row("storage_class")).getByText(
        "a widget locker has one shelf, so nothing could have landed on a different one."
      )
    ).toBeInTheDocument();
  });
});

describe("while the check is running", () => {
  it("says a runnable step is waiting, and keeps the declared skips settled", () => {
    render(<ManifestProbeSteps manifest={syntheticManifest()} report={null} running elapsedMs={2400} />);

    // "Waiting" and not "running": nothing streams step progress - the
    // manager runs the steps and answers with one report - so a row
    // claiming to be the step in flight would be the UI asserting
    // something it was never told. The elapsed counter is what says the
    // request is alive, which is the job #669 wanted a current-step
    // marker for.
    expect(outcomeOf("write")).toBe("waiting");
    expect(outcomeOf("credentials")).toBe("skipped");
    expect(screen.getByTestId("probe-elapsed")).toHaveTextContent("2.4s");
  });
});

describe("when the check passed", () => {
  it("renders each step's own outcome and the engine's own sentence", () => {
    render(
      <ManifestProbeSteps
        manifest={syntheticManifest()}
        report={verifiedReport()}
        running={false}
        elapsedMs={1400}
      />
    );

    expect(outcomeOf("write")).toBe("passed");
    expect(within(row("read_back")).getByTestId("probe-step-detail")).toHaveTextContent(
      "the object was read back and is byte for byte what was written"
    );
    expect(screen.getByTestId("probe-elapsed")).toHaveTextContent("1.4s");
  });

  it("keeps a declared skip skipped even in a passing report", () => {
    render(
      <ManifestProbeSteps
        manifest={syntheticManifest()}
        report={verifiedReport()}
        running={false}
        elapsedMs={1400}
      />
    );

    // The report says ok. That must not promote a step nobody ran into
    // one that passed: a green tick on a write that was skipped is the
    // single most expensive thing this screen could say.
    expect(outcomeOf("credentials")).toBe("skipped");
  });
});

describe("when the check was refused", () => {
  it("names the failing step and repeats the service's reason verbatim", () => {
    render(
      <ManifestProbeSteps
        manifest={syntheticManifest()}
        report={refusedReport()}
        running={false}
        elapsedMs={900}
      />
    );

    expect(outcomeOf("reach")).toBe("passed");
    expect(outcomeOf("write")).toBe("failed");

    // Verbatim, and with the category beside it rather than folded into
    // the sentence: the category is what an operator branches on when
    // deciding whose problem this is. Reformatting the sentence is how a
    // surface ends up interpolating what the operator typed into it,
    // which is what mediumcreds.go's "refusals by shape, never by
    // content" forbids.
    const detail = within(row("write")).getByTestId("probe-step-detail");
    expect(detail).toHaveTextContent(
      "the destination refused the write; the credential and the container are right and the policy is not"
    );
    expect(outcomeOf("write")).not.toContain("AccessDenied");
    expect(within(row("write")).getByTestId("probe-step-category")).toHaveTextContent("permission");
  });

  it("renders the steps after the failure as never tried, with the reason the report gave", () => {
    render(
      <ManifestProbeSteps
        manifest={syntheticManifest()}
        report={refusedReport()}
        running={false}
        elapsedMs={900}
      />
    );

    expect(outcomeOf("read_back")).toBe("skipped");
    expect(within(row("read_back")).getByTestId("probe-step-detail")).toHaveTextContent(
      "the write did not happen, so there is nothing to read back"
    );
  });

  it("invents no sentence of its own and echoes nothing the operator typed", () => {
    const { container } = render(
      <ManifestProbeSteps
        manifest={syntheticManifest()}
        report={refusedReport()}
        running={false}
        elapsedMs={900}
      />
    );

    // The canary is a value an operator would have typed into the form.
    // It is not in the report, so it must not be on screen: the only way
    // it could get here is a renderer building its own sentence out of
    // the draft, which is the failure mode "refusals by shape, never by
    // content" names.
    expect(container.textContent).not.toContain("canary-locker-7f3a");

    // Every sentence rendered against a step came from the report or
    // from the manifest. Asserted as a set difference rather than by
    // eyeballing, so a helpful paraphrase added later fails here.
    const details = screen
      .getAllByTestId("probe-step-detail")
      .map((d) => d.textContent ?? "");
    const allowed = new Set<string>([
      ...refusedReport().checks.map((c) => c.detail),
      ...syntheticManifest().probe.steps.map((s) => s.reason ?? "")
    ]);
    for (const text of details) {
      expect(allowed.has(text), `rendered a sentence nobody sent: ${text}`).toBe(true);
    }
  });
});

describe("a report step the manifest does not declare", () => {
  it("is rendered rather than dropped", () => {
    // #622 gave the drive on this machine a ninth step, `space`, which
    // mediumcheck emits and the wire union carries; the manifest
    // vocabulary is the eight. A renderer that drew only the declared
    // rows would silently discard a real answer about a real
    // destination, and the one it discarded would be the one about
    // running out of disk.
    const report = verifiedReport();
    report.checks.push({
      step: "space",
      outcome: "passed",
      detail: "1.2 TB free, above the floor this deployment declares"
    });

    render(<ManifestProbeSteps manifest={syntheticManifest()} report={report} running={false} elapsedMs={1400} />);

    expect(rowsInOrder()).toEqual([...DECLARED_ORDER, "space"]);
    expect(outcomeOf("space")).toBe("passed");
  });
});

function verifiedReport(): MediumPreflight {
  return {
    medium: "locker_one",
    ok: true,
    checks: [
      { step: "credentials", outcome: "skipped", detail: "this backend declares no credential" },
      { step: "reach", outcome: "passed", detail: "the locker answered" },
      { step: "deliverable", outcome: "passed", detail: "the shelf reads on demand" },
      { step: "write", outcome: "passed", detail: "an object was written" },
      {
        step: "read_back",
        outcome: "passed",
        detail: "the object was read back and is byte for byte what was written"
      },
      { step: "storage_class", outcome: "skipped", detail: "this backend has one shelf" },
      { step: "verification", outcome: "passed", detail: "the content class is what the read-back step just did" },
      { step: "delete", outcome: "passed", detail: "the probe object was deleted, and the locker confirms it is gone" }
    ]
  };
}

/**
 * #669's drawn refusal, which is the instructive one: reads succeeded and
 * the write was refused, so the credential is valid, the container is
 * right, and the policy is missing the write permission for that prefix.
 */
function refusedReport(): MediumPreflight {
  return {
    medium: "locker_one",
    ok: false,
    checks: [
      { step: "credentials", outcome: "skipped", detail: "this backend declares no credential" },
      { step: "reach", outcome: "passed", detail: "the locker answered" },
      { step: "deliverable", outcome: "passed", detail: "the shelf reads on demand" },
      {
        step: "write",
        outcome: "failed",
        category: "permission",
        detail:
          "the destination refused the write; the credential and the container are right and the policy is not"
      },
      {
        step: "read_back",
        outcome: "skipped",
        detail: "the write did not happen, so there is nothing to read back"
      },
      { step: "storage_class", outcome: "skipped", detail: "this backend has one shelf" },
      {
        step: "verification",
        outcome: "skipped",
        detail: "nothing was written, so no verification class could be proven"
      },
      { step: "delete", outcome: "skipped", detail: "there is no probe object to delete" }
    ]
  };
}
