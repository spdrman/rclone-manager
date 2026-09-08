import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { Banner } from "@shared/components/Banner";
import type { BannerTone } from "@shared/components/Banner";
import { WarningBanner } from "@shared/components/WarningBanner";
import { HaltBanner } from "@shared/components/HaltBanner";
import { PlacementList } from "@shared/components/PlacementList";
import { ErrorState } from "@shared/components/EmptyState";
import { DashboardPage } from "@shared/pages/DashboardPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { BackupManagerApi } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { resetGraphForTests } from "@shared/state/graph";
import type { BackupSet } from "@shared/types/backup";
import type { SystemHealth } from "@shared/types/operation";
// The real stylesheet, not a stub (vite.config.ts's `css: true`). "Top
// right" is a claim about CSS and nowhere else: the close control is
// appended after the banner's content, so in the DOM it is last, and the
// only thing that puts it in the corner is the absolute positioning below.
// With a stubbed stylesheet a control sitting at the bottom of a
// column-direction banner passes every other assertion in this file.
import "@shared/design-system/components.css";

/**
 * Issue #620: a banner an operator has read can be put away, and putting
 * it away is a viewer-side act and nothing more.
 *
 * The two halves are asserted separately because they fail separately. A
 * close control that removes the banner is easy and is most of the visible
 * feature; the part worth a test suite is what it must NOT do. This app
 * refuses through banners, so a dismissal that reached the service, or
 * that outlived the render it happened in, would be a dismissal an
 * operator could mistake for "handled". That is a safety defect rather
 * than a cosmetic one, and none of it is visible from looking at the
 * control.
 *
 * # What "brings it back" is taken to mean here
 *
 * #620 asks that re-rendering the same condition bring the banner back.
 * Read literally as "any re-render", that contradicts the acceptance
 * criterion one line above it: DashboardPage re-renders on every poll and
 * on every activity frame, so a banner that reset on re-render would be
 * back on screen within a second or two and the control would read as
 * broken. So the reading here is the one that keeps both halves true: the
 * dismissal lives in the banner's own state and nothing else, so it dies
 * with the render tree. Rendering the surface again from the condition (a
 * navigation, a reload, a remount) shows the banner again, and a banner
 * whose report CHANGES comes back without waiting for any of that, which
 * is the case that actually matters: one dismissal must not swallow the
 * next condition. Both are pinned below, and so is the polling case, so
 * the decision is asserted rather than left to be rediscovered.
 */

const TONES: BannerTone[] = ["info", "ok", "warn", "danger"];

/** The one accessible name every close control answers to unless a caller
 *  gives it a better one. Written once here so a rename is one edit. */
const DISMISS = "Dismiss this notice";

const close = () => screen.getByRole("button", { name: DISMISS });

/** The banner box a sentence is drawn in, for the cases that ask which
 *  banner a control belongs to rather than whether one exists. */
function bannerAround(text: string | RegExp): HTMLElement {
  return screen.getByText(text).closest(".banner") as HTMLElement;
}

describe("every banner tone carries a close control (issue #620)", () => {
  afterEach(cleanup);

  it.each(TONES)("the %s tone renders one, with an accessible name", (tone) => {
    render(<Banner tone={tone}>Something happened</Banner>);

    expect(close()).toBeTruthy();
  });

  it.each(TONES)("WarningBanner's %s tone renders one too", (tone) => {
    render(<WarningBanner tone={tone} title="Something happened" />);

    expect(close()).toBeTruthy();
  });

  it("puts it at the top right, which is a claim about the shipped CSS", () => {
    render(<Banner tone="danger">Something happened</Banner>);

    const box = screen.getByText("Something happened").closest(".banner") as HTMLElement;
    // Without this the absolute positioning below resolves against the
    // page rather than against the banner, and the control lands in the
    // corner of the viewport.
    expect(getComputedStyle(box).position).toBe("relative");

    const style = getComputedStyle(close());
    expect(style.position).toBe("absolute");
    // Top and right are pinned; bottom and left are not. Asserting the
    // pair that is absent is what makes this a test of the CORNER rather
    // than of "some offsets were written".
    expect(style.top).toMatch(/^\d/);
    expect(style.right).toMatch(/^\d/);
    expect(style.bottom === "" || style.bottom === "auto").toBe(true);
    expect(style.left === "" || style.left === "auto").toBe(true);
  });

  it("reserves room for it, so the copy does not run underneath it", () => {
    render(<Banner tone="info">Something happened</Banner>);

    const box = screen.getByText("Something happened").closest(".banner") as HTMLElement;
    const padding = getComputedStyle(box).paddingRight;
    // A dismissible banner pads its right edge past the control's own
    // width. The control is out of flow, so nothing else would stop a
    // long sentence sliding under it.
    expect(padding).not.toBe("");
    expect(padding).not.toBe("0px");
  });

  it("is a real button, so a keyboard reaches it and Enter operates it", async () => {
    const user = userEvent.setup();
    render(<Banner tone="warn">Something happened</Banner>);

    const control = close();
    expect(control.tagName).toBe("BUTTON");
    // Every form in this app has a submit button, and a button with no
    // type inside a form submits it. Banners sit inside forms (the wizard,
    // the settings pages), so this is load-bearing rather than tidy.
    expect(control.getAttribute("type")).toBe("button");

    await user.tab();
    expect(document.activeElement).toBe(control);

    await user.keyboard("{Enter}");
    expect(screen.queryByText("Something happened")).toBeNull();
  });

  it("takes its name from visually hidden text rather than an aria-label", () => {
    render(<Banner tone="info">Something happened</Banner>);

    // FieldHelp's module doc argues this for its own close control and the
    // argument carries: an aria-label makes a button answer to that name
    // in every label-based lookup, and text inside the button is what
    // survives translation and find-in-page. A banner is not inside a
    // <label>, so the reason PasswordInput diverges does not apply here.
    expect(close().getAttribute("aria-label")).toBeNull();
    expect(close().textContent).toContain(DISMISS);
  });

  it("lets a caller name it, for a surface carrying more than one", () => {
    render(
      <>
        <Banner tone="warn" dismissLabel="Dismiss the storage warning">
          Storage is nearly full
        </Banner>
        <Banner tone="info" dismissLabel="Dismiss the retention hint">
          Retention runs nightly
        </Banner>
      </>
    );

    expect(screen.getByRole("button", { name: "Dismiss the storage warning" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Dismiss the retention hint" })).toBeTruthy();
  });

  it("removes the banner from view when it is pressed", async () => {
    const user = userEvent.setup();
    render(<WarningBanner tone="danger" title="A backup set is halted" />);

    await user.click(close());

    expect(screen.queryByText("A backup set is halted")).toBeNull();
    expect(screen.queryByRole("button", { name: DISMISS })).toBeNull();
  });
});

/**
 * Every case in here asserts an ABSENCE, which is a shape that passes on
 * its own for the wrong reason: before the close control existed at all,
 * all three of these were green. So each one renders a dismissible banner
 * beside the banner under test and counts, rather than asking whether any
 * control is on screen. The control is the positive half that makes the
 * absence mean something.
 */
describe("dismissing is opt-out, not unconditional (issue #620)", () => {
  afterEach(cleanup);

  it("a banner can say it is not dismissible, and then offers no control", () => {
    render(
      <>
        <WarningBanner tone="danger" title="A backup set is halted" dismissible={false} />
        <WarningBanner tone="danger" title="Something else went wrong" />
      </>
    );

    const controls = screen.getAllByRole("button", { name: DISMISS });
    expect(controls).toHaveLength(1);
    expect(bannerAround("A backup set is halted").contains(controls[0])).toBe(false);
  });

  it("the halt banner opts out, because its own doc says it will not offer to dismiss", () => {
    render(
      <>
        <HaltBanner set={haltedSet()} />
        <WarningBanner tone="danger" title="Something else went wrong" />
      </>
    );

    // WarningBanner's module doc records the decision this is honouring:
    // the halt banner "will not offer to dismiss, retry or re-trust". §77
    // invariant 5 is why. A close control on a changed-host-key banner is
    // the same defect as a Keep set halted button that did nothing, one
    // step further along: it looks like a decision an operator is allowed
    // to make about the halt, and it is not.
    const controls = screen.getAllByRole("button", { name: DISMISS });
    expect(controls).toHaveLength(1);
    expect(bannerAround(/SSH host key/i).contains(controls[0])).toBe(false);
  });

  it("the failure surface opts out, because dismissing it would leave nothing", () => {
    render(
      <>
        <ErrorState message="Could not reach the backup manager" onRetry={() => {}} />
        <WarningBanner tone="danger" title="Something else went wrong" />
      </>
    );

    // ErrorState is not a notice over a page, it IS the page: every caller
    // renders it instead of the content it could not load. Dismissing it
    // would leave a blank panel and take its own Try again button with
    // it, which is the second half of #620's opt-out rule.
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
    const controls = screen.getAllByRole("button", { name: DISMISS });
    expect(controls).toHaveLength(1);
    expect(bannerAround("Could not reach the backup manager").contains(controls[0])).toBe(false);
  });
});

describe("dismissing is viewer-side and nothing else (issue #620)", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("changes no server state and writes nothing down", async () => {
    const user = userEvent.setup();
    const api = createMockApi();
    const calls = countEveryCall(api);
    const setItem = vi.spyOn(Storage.prototype, "setItem");

    renderDashboard(api, [await staleSet()]);
    await screen.findByText(/Stale/);

    // Everything the page loads on the way to drawing the banner is
    // allowed; what this pins is that pressing the control adds nothing.
    const before = calls();
    await user.click(close());

    expect(screen.queryByText(/Stale/)).toBeNull();
    expect(calls()).toBe(before);
    // Covers localStorage and sessionStorage both: a dismissal that
    // survived a reload would be a dismissal the operator cannot undo by
    // reloading, which is the one escape hatch left when a banner is put
    // away by accident.
    expect(setItem).not.toHaveBeenCalled();
  });

  it("brings the banner back when the surface is rendered again from the same condition", async () => {
    const user = userEvent.setup();
    const set = await staleSet();

    renderDashboard(createMockApi(), [set]);
    await screen.findByText(/Stale/);
    await user.click(close());
    expect(screen.queryByText(/Stale/)).toBeNull();

    // A navigation away and back, or a reload. Nothing carried the
    // dismissal across it, so the condition draws its banner again.
    cleanup();
    resetGraphForTests();
    renderDashboard(createMockApi(), [set]);

    expect(await screen.findByText(/Stale/)).toBeTruthy();
  });

  it("brings it back without a remount when the banner starts reporting something else", async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <WarningBanner tone="danger" title="Backup Manager could not log in to nas-01" />
    );

    await user.click(close());
    expect(screen.queryByText(/could not log in/i)).toBeNull();

    // The case that makes this a safety property rather than a
    // convenience: a dismissal is about the sentence that was on screen,
    // so it cannot be inherited by the next one. Without this, an
    // operator who put away "could not log in" would never see "the SSH
    // host key has changed" arrive in its place.
    rerender(<WarningBanner tone="danger" title="The SSH host key for nas-01 has changed" />);

    expect(screen.getByText("The SSH host key for nas-01 has changed")).toBeTruthy();
    expect(close()).toBeTruthy();
  });

  it("stays dismissed while the same report is re-rendered, so a polling page does not put it back", async () => {
    const user = userEvent.setup();
    const { rerender } = render(<WarningBanner tone="warn" title="Stale · nightly" />);

    await user.click(close());
    rerender(<WarningBanner tone="warn" title="Stale · nightly" />);

    // This is the half of #620's "re-rendering brings it back" that had to
    // be decided rather than taken literally, and the file's module doc
    // argues it. DashboardPage re-renders on every poll; a banner that
    // came back on each one would make the control look broken and would
    // fail the acceptance criterion it sits beside.
    expect(screen.queryByText("Stale · nightly")).toBeNull();
  });
});

describe("the notices that were raw divs dismiss too (issue #620)", () => {
  afterEach(cleanup);

  it("the copies list's explanation of an empty list can be put away", async () => {
    const user = userEvent.setup();
    render(<PlacementList placements={[]} />);

    expect(screen.getByText("No confirmed copy yet")).toBeTruthy();
    await user.click(close());

    // The card itself stays; only the notice inside it went.
    expect(screen.queryByText("No confirmed copy yet")).toBeNull();
    expect(screen.getByRole("region", { name: "Copies" })).toBeTruthy();
  });
});

/**
 * Nothing in the shipped UI writes `className="banner"` by hand any more.
 *
 * This is the guard that stops the thirty-four divs #620 converted from
 * growing back one call site at a time. A close control cannot be added to
 * a class name, so every one of those divs was a banner the operator could
 * not put away, and the only thing that had kept them consistent until now
 * was that everybody copied the line above them.
 *
 * It scans source rather than rendered output for the reason
 * jsx-literal-escapes.test.tsx gives for doing the same: most of these
 * sites are inside a conditional that only one failure path reaches, and a
 * rendered sweep sees only the branches some test happens to drive.
 *
 * The allowlist is a subset check rather than an equality one, so a file
 * that gets converted later needs no edit here.
 */
const SHIPPED_TSX: Record<string, string> = import.meta.glob("../**/*.tsx", {
  query: "?raw",
  import: "default",
  eager: true
});

/** BackupSetWizardPage holds six of them and is owned by another branch in
 *  EPIC H, so converting it here would collide. Named rather than left to
 *  read as an oversight; it is the one file #620 did not finish. */
const ALLOWED_RAW_BANNERS = ["/pages/BackupSetWizardPage.tsx"];

describe("the banner box has one owner (issue #620)", () => {
  it("no shipped component writes the banner class by hand, except the one named file", () => {
    const offenders = Object.entries(SHIPPED_TSX)
      .filter(([path]) => !path.includes(".test.") && !path.includes("/test/"))
      // Anchored on the box's own class rather than on the prefix:
      // Banner.tsx writes className="banner__close" on the control it
      // adds, and a bare prefix match would count the component that
      // fixed this as one of the offenders.
      .filter(([, source]) => /className="banner[ "]/.test(source))
      .map(([path]) => path);

    const unexpected = offenders.filter(
      (path) => !ALLOWED_RAW_BANNERS.some((allowed) => path.endsWith(allowed))
    );

    expect(unexpected).toEqual([]);
  });
});

/** Counts every call through the api, whatever it was for. A dismissal
 *  must add nothing at all, so the assertion is about the total rather
 *  than about any one method: naming methods would only catch the calls
 *  somebody thought to name. */
function countEveryCall(api: BackupManagerApi): () => number {
  let count = 0;
  const record = api as unknown as Record<string, unknown>;
  for (const key of Object.keys(record)) {
    const original = record[key];
    if (typeof original !== "function") continue;
    record[key] = (...args: unknown[]) => {
      count += 1;
      return (original as (...a: unknown[]) => unknown).apply(api, args);
    };
  }
  return () => count;
}

const IDLE_HEALTH: SystemHealth = {
  generatedAt: "2026-08-30T10:00:00Z",
  serviceRunning: true,
  backupHealth: "healthy",
  backupHealthReason: "every set has a fresh known-good backup",
  newestVerifiedBackupAt: "2026-08-30T09:40:00Z",
  lastCompletedBackupAt: "2026-08-30T09:40:00Z",
  oldestSetFreshnessHours: 1,
  setsHealthy: 1,
  setsDegraded: 0,
  setsStale: 0,
  setsFailing: 0,
  quarantinedCount: 0,
  readOnlyRetainedCount: 0,
  storageFreeBytes: 1,
  storageTotalBytes: 2,
  storageState: "nominal",
  storageReadingsUnavailable: 0
};

/** A set the dashboard raises its stale banner for: a real fixture with
 *  one field moved, so the banner under test is the product's own and not
 *  a shape invented here. */
async function staleSet(): Promise<BackupSet> {
  const sets = await createMockApi().listSets();
  const next: BackupSet = { ...sets[0], state: "stale", stateNote: "No backup in 3 days." };
  delete next.haltReason;
  return next;
}

function haltedSet(): BackupSet {
  return {
    source: "nas-01",
    set: "nightly",
    name: "nightly",
    host: "nas-01",
    state: "failing",
    haltReason: "host-key-changed"
  } as BackupSet;
}

function renderDashboard(api: BackupManagerApi, sets: BackupSet[]) {
  return render(
    <MemoryRouter>
      <ApiProvider api={api}>
        <DashboardPage
          health={{ data: IDLE_HEALTH, error: null, loading: false, reload: () => {} }}
          sets={{ data: sets, error: null, loading: false, reload: () => {} }}
          readOnly={false}
        />
      </ApiProvider>
    </MemoryRouter>
  );
}
