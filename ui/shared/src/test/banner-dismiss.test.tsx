import { afterEach, describe, expect, it, vi } from "vitest";
import ts from "typescript";
import { cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Link, MemoryRouter, Route, Routes } from "react-router-dom";
import { Banner } from "@shared/components/Banner";
import type { BannerTone } from "@shared/components/Banner";
import { WarningBanner } from "@shared/components/WarningBanner";
import { HaltBanner } from "@shared/components/HaltBanner";
import { PlacementList } from "@shared/components/PlacementList";
import { ErrorState } from "@shared/components/EmptyState";
import { App } from "@shared/App";
import { DashboardPage } from "@shared/pages/DashboardPage";
import { BackupSetDetailPage } from "@shared/pages/BackupSetDetailPage";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";
import { backupSetPath } from "@shared/utilities/routes";
import type { BackupdApi } from "@shared/api/contracts";
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
 *  banner a control belongs to rather than whether one exists. Takes the
 *  element too, so a case that already awaited its text does not have to
 *  look the same text up twice. */
function bannerAround(text: string | RegExp | HTMLElement): HTMLElement {
  const node = typeof text === "string" || text instanceof RegExp ? screen.getByText(text) : text;
  return node.closest(".banner") as HTMLElement;
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
        <ErrorState message="Could not reach the backupd" onRetry={() => {}} />
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
    expect(bannerAround("Could not reach the backupd").contains(controls[0])).toBe(false);
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
      <WarningBanner tone="danger" title="Backupd could not log in to nas-01" />
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

/**
 * The banners that live ABOVE the router outlet, and why they opt out.
 *
 * App.tsx renders these two over `<Routes>` rather than inside a page, so
 * they are mounted once for the life of the session and no navigation ever
 * unmounts them. That is the one arrangement in which "a dismissal dies
 * with the render tree" buys nothing at all: there is no later render of
 * the surface, because the surface never went away. Both would have stayed
 * gone until a hard reload.
 *
 * Both are also squarely #620's own opt-out rule. The first-run banner's
 * body says "Until that is done nothing is backed up", and an operator who
 * clears it and forgets believes they have backups they do not have. The
 * version-mismatch banner is the ONLY thing on screen explaining why every
 * management control is disabled, so clearing it turns a stated refusal
 * into an application that silently does nothing.
 */
describe("the root banners cannot be cleared off the screen (issue #620)", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("offers no way to dismiss the first-run banner", async () => {
    renderApp(createMockApi("first-run"));

    const banner = bannerAround(await screen.findByText(/has no configuration yet/i));
    expect(within(banner).queryByRole("button", { name: DISMISS })).toBeNull();
  });

  it("offers no way to dismiss the version-mismatch banner", async () => {
    renderApp(createMockApi("version-mismatch"));

    const banner = bannerAround(await screen.findByText(/update required/i));
    expect(within(banner).queryByRole("button", { name: DISMISS })).toBeNull();
  });

  /**
   * The mechanism the two cases above are protecting against, on a harness
   * rather than on App, so it stays readable and cannot go stale when
   * App's copy changes. This is what a dismissible banner above the outlet
   * really does, and it is why neither of them may be one: a navigation
   * re-renders what is INSIDE `<Routes>` and leaves everything above it
   * mounted, dismissal and all.
   */
  it("a dismissible banner above the outlet keeps its dismissal across a navigation", async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={["/"]}>
        <Banner tone="warn">Above the outlet</Banner>
        <Routes>
          <Route path="/" element={<Link to="/elsewhere">Go elsewhere</Link>} />
          <Route path="/elsewhere" element={<p>Elsewhere</p>} />
        </Routes>
      </MemoryRouter>
    );

    await user.click(close());
    expect(screen.queryByText("Above the outlet")).toBeNull();

    await user.click(screen.getByRole("link", { name: "Go elsewhere" }));
    expect(await screen.findByText("Elsewhere")).toBeTruthy();
    expect(screen.queryByText("Above the outlet")).toBeNull();
  });
});

/**
 * A dismissal is about the sentence that was on screen, and for these two
 * banners the sentence is in the BODY rather than in the title.
 *
 * WarningBanner used to derive its reset key from the tone, the eyebrow and
 * the title alone, which is fine until a call site keeps a fixed title over
 * a body that moves. The dashboard's stale banner is exactly that: the
 * title is "Stale ยท <name>" and every word an operator acts on is in
 * `stateNote` underneath it. Nothing unmounts while the same set stays
 * stale, so one dismissal used to swallow every later note for that set.
 */
describe("a changed body brings the banner back (issue #620)", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("counts a scalar body in the derived key, so a moved sentence is a new report", async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <WarningBanner tone="warn" title="Stale ยท nightly">No backup in 3 days.</WarningBanner>
    );

    await user.click(close());
    expect(screen.queryByText("No backup in 3 days.")).toBeNull();

    rerender(<WarningBanner tone="warn" title="Stale ยท nightly">No backup in 9 days.</WarningBanner>);

    expect(screen.getByText("No backup in 9 days.")).toBeTruthy();
  });

  it("brings the dashboard's stale banner back when the set's note changes under it", async () => {
    const user = userEvent.setup();
    const set = await staleSet();

    const { rerender } = render(dashboardTree(createMockApi(), [set]));
    await screen.findByText("No backup in 3 days.");
    await user.click(close());
    expect(screen.queryByText("No backup in 3 days.")).toBeNull();

    // Same set, still stale, still the same title. Only the sentence the
    // operator is meant to act on has moved, and that is the whole report.
    rerender(dashboardTree(createMockApi(), [{ ...set, stateNote: "No backup in 9 days." }]));

    expect(await screen.findByText("No backup in 9 days.")).toBeTruthy();
  });
});

/**
 * Clearing a set's override swaps one sentence for its opposite in place.
 *
 * RetentionPanel is keyed on `retention.data.isOverride`, which reads like
 * a remount that would rescue this and is not one: the clear path calls
 * `apply`, `apply` calls `setR` on panel-local state, and `retention.data`
 * is never refetched, so the key never moves. The banner keeps its
 * position, its tone and its instance, and only the words inside change.
 *
 * Those words are the one line on the card saying WHICH policy decides
 * deletions for this set, so a dismissal that outlives the change hides the
 * answer to the only question the card exists to answer.
 */
describe("which policy governs a set survives a dismissal (issue #620)", () => {
  afterEach(() => {
    cleanup();
    resetGraphForTests();
    vi.restoreAllMocks();
  });

  it("shows the deployment sentence after the override is cleared, even if its predecessor was dismissed", async () => {
    const user = userEvent.setup();
    render(
      <MemoryRouter initialEntries={[backupSetPath("media", "weekly-archive")]}>
        <ApiProvider api={createMockApi()}>
          <Routes>
            <Route path="/sets/:source/:set" element={<BackupSetDetailPage readOnly={false} />} />
          </Routes>
        </ApiProvider>
      </MemoryRouter>
    );

    const own = await screen.findByText(/Retained under this backup set's own policy/);
    await user.click(within(bannerAround(own)).getByRole("button", { name: DISMISS }));
    expect(screen.queryByText(/Retained under this backup set's own policy/)).toBeNull();

    await user.click(screen.getByRole("button", { name: "Return to the deployment's policy" }));
    const confirm = await screen.findByRole("dialog");
    await user.click(within(confirm).getByRole("button", { name: "Return to the deployment's policy" }));

    expect(
      await screen.findByText(/Retained under the deployment's retention policy/)
    ).toBeTruthy();
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
 * Nothing in the shipped UI builds the banner box by hand any more.
 *
 * This is the guard that stops the thirty-four divs #620 converted from
 * growing back one call site at a time. A close control cannot be added to
 * a class name, so every one of those divs was a banner an operator could
 * not put away, and the only thing that had kept them consistent until now
 * was that everybody copied the line above them.
 *
 * It parses rather than pattern-matches, for the reason
 * jsx-literal-escapes.test.tsx gives for doing the same one file over: the
 * class can be spelled several ways and a regex over raw source cannot tell
 * the ones that matter from the ones that do not. This started as
 * `/className="banner[ "]/` and that is worse than it looks, because the
 * spelling it misses is the one the code it replaced actually used:
 * WarningBanner built its own box as `className={"banner banner--" + tone}`
 * until this branch changed it, so a reintroduction written the way the
 * original was written walked straight past the guard.
 *
 * The rule is the first class token, whatever the expression around it. The
 * `banner` token means "this element IS the box", which is what Banner.tsx
 * now owns; `banner__close` and `banner--warn` are different tokens and are
 * left alone. Banner.tsx's own `classes.join(" ")` has no literal in the
 * attribute at all, so the component that fixed this is not reported as one
 * of the offenders.
 */
const SHIPPED_TSX: Record<string, string> = {
  ...import.meta.glob("../**/*.tsx", { query: "?raw", import: "default", eager: true }),
  // The provider shells too, for the reason jsx-literal-escapes.test.tsx
  // takes them: they are the other half of what reaches an operator, and
  // they are JSX written by the same hands.
  ...import.meta.glob("../../../../apps/*/frontend/**/*.tsx", { query: "?raw", import: "default", eager: true })
};

/** Every className in one file whose first class token is `banner`, which
 *  is a banner box written by hand. Exported so the cases below can drive
 *  it against each spelling directly, rather than hoping the tree happens
 *  to contain one. */
export function findHandWrittenBanners(fileName: string, source: string): number[] {
  const sourceFile = ts.createSourceFile(fileName, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
  const lines: number[] = [];

  /** The leftmost string literal under a node, which for `"a " + b + c` is
   *  the one that decides the first class token. */
  const firstLiteral = (node: ts.Node): string | undefined => {
    if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) return node.text;
    if (ts.isTemplateExpression(node)) return node.head.text;
    for (const child of node.getChildren(sourceFile)) {
      const found = firstLiteral(child);
      if (found !== undefined) return found;
    }
    return undefined;
  };

  const visit = (node: ts.Node) => {
    if (
      ts.isJsxAttribute(node) &&
      ts.isIdentifier(node.name) &&
      node.name.text === "className" &&
      node.initializer !== undefined
    ) {
      const literal = firstLiteral(node.initializer);
      if (literal !== undefined && literal.trim().split(/\s+/)[0] === "banner") {
        lines.push(sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line + 1);
      }
    }
    ts.forEachChild(node, visit);
  };

  visit(sourceFile);
  return lines;
}

describe("the guard's own matcher (issue #620)", () => {
  const caught = (source: string) => findHandWrittenBanners("probe.tsx", source).length;

  it("catches the plain string spelling", () => {
    expect(caught(`const a = <div className="banner banner--info">x</div>;`)).toBe(1);
  });

  it("catches the spelling WarningBanner itself used, which the first version of this guard missed", () => {
    expect(caught(`const a = <div className={"banner banner--" + tone}>x</div>;`)).toBe(1);
  });

  it("catches it behind a template literal and behind a longer concatenation", () => {
    expect(caught("const a = <div className={`banner banner--${tone}`}>x</div>;")).toBe(1);
    expect(caught(`const a = <div className={"banner " + tone + extra}>x</div>;`)).toBe(1);
  });

  it("leaves the control, the modifier and every other class alone", () => {
    for (const source of [
      `const a = <button className="banner__close" />;`,
      `const a = <div className="banner--warn" />;`,
      `const a = <div className="card" />;`,
      `const a = <div className="table-scroll banner-ish" />;`
    ]) {
      expect(caught(source)).toBe(0);
    }
  });

  it("leaves Banner.tsx's own joined list alone, which is the form that replaced all of them", () => {
    expect(caught(`const a = <Tag className={classes.join(" ")}>x</Tag>;`)).toBe(0);
  });
});

describe("the banner box has one owner (issue #620)", () => {
  const shipped = Object.keys(SHIPPED_TSX)
    .filter((path) => !path.includes(".test.") && !path.includes("/test/"))
    .sort();

  /** An empty result has two explanations, and "the glob walked nothing"
   *  is the one that would make the assertion below pass for the wrong
   *  reason. */
  it("actually reads the files it claims to check", () => {
    // 60 at the time of writing, and the floor is deliberately under it:
    // this is here to fail when the glob walks NOTHING, which is the one
    // way the equality below passes for the wrong reason.
    expect(shipped.length).toBeGreaterThan(45);
    expect(shipped).toContain("../components/Banner.tsx");
    expect(shipped).toContain("../pages/BackupSetWizardPage.tsx");
    expect(shipped).toContain("../../../../apps/generic/frontend/bootstrap.tsx");
  });

  /**
   * The assertion is an equality against the one file that really does
   * still carry six of them, not a subset check against an allowlist.
   * BackupSetWizardPage belongs to another branch in EPIC H, so #620 could
   * not convert it, and that makes it the positive control this guard would
   * otherwise have to invent: the day the matcher stops matching, or the
   * glob stops walking, this list goes empty and the case fails.
   */
  it("finds them only in the one file #620 could not convert", () => {
    const offenders = shipped.filter((path) => findHandWrittenBanners(path, SHIPPED_TSX[path]).length > 0);

    expect(offenders).toEqual(["../pages/BackupSetWizardPage.tsx"]);
    expect(findHandWrittenBanners(
      "../pages/BackupSetWizardPage.tsx",
      SHIPPED_TSX["../pages/BackupSetWizardPage.tsx"]
    )).toHaveLength(6);
  });
});

/** Counts every call through the api, whatever it was for. A dismissal
 *  must add nothing at all, so the assertion is about the total rather
 *  than about any one method: naming methods would only catch the calls
 *  somebody thought to name. */
function countEveryCall(api: BackupdApi): () => number {
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

/** The dashboard as an element rather than a rendered tree, so a case can
 *  re-render the same position with a moved fixture. */
function dashboardTree(api: BackupdApi, sets: BackupSet[]) {
  return (
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

function renderDashboard(api: BackupdApi, sets: BackupSet[]) {
  return render(dashboardTree(api, sets));
}

/** Signed in, on the generic shell, which is what puts App's own two
 *  banners above the router outlet in the first place. */
const AUTHENTICATED: AuthContext = { authenticated: true, username: "bm-admin", mode: "local-account" };
const BRIDGE: PlatformBridge = { ...genericBridge, getAuthContext: () => Promise.resolve(AUTHENTICATED) };

function renderApp(api: BackupdApi, route = "/") {
  return render(
    <MemoryRouter initialEntries={[route]}>
      <ApiProvider api={api}>
        <PlatformProvider bridge={BRIDGE}>
          <App />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
}
