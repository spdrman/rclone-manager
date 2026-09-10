import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SettingsPage } from "@shared/pages/SettingsPage";
import { ApiProvider } from "@shared/api/ApiContext";
import type { AppSettings, StorageMedium } from "@shared/api/contracts";
import { LOCAL_DESTINATION_ID } from "@shared/api/contracts";
import { createMockApi } from "@shared/api/mock";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { genericBridge } from "../../../../apps/generic/frontend/platform";
import { graph, resetGraphForTests } from "@shared/state/graph";
import { versionNode } from "@shared/state/appNodes";
import type { VersionInfo } from "@shared/types/operation";

/**
 * I2.3 (#671): handing the default from one destination to another.
 *
 * The default is a role exactly one destination holds, and whatever holds
 * it cannot be removed. So the only way to remove the current default is
 * to give the role away, and that transfer has to be a real, visible act
 * rather than a side effect of something else.
 *
 * # Why every case here is about a control that is present
 *
 * The failure this file exists to prevent is not a wrong request. It is a
 * MISSING CONTROL. A row that omits "Make default" because the button
 * would do nothing, or omits "Remove" because the backend would refuse
 * it, is a row that sends an operator hunting through settings for a
 * transfer that is in fact one click away on the row they are already
 * looking at. #622 built it that way and #671 reverses it: disabled and
 * explained, never hidden.
 *
 * So "is it disabled" is never asserted on its own here. A disabled
 * control with no reason beside it is the same dead end as an absent one,
 * one hover later, and the assertions pair the disabled attribute with
 * the sentence that says what would enable it.
 *
 * # The reason has to be right for ANY default
 *
 * #670 deleted RemoveStorageMedium's local-shaped special case: the local
 * drive is now a declared destination like any other and its
 * undeletability falls out of the general rules, so the refusal an
 * operator meets is the generic ErrStorageMediumIsDefault. A line
 * explaining Remove that talked about the local drive would be wrong on
 * every deployment whose default is a bucket, which is why the same
 * sentence is asserted with `local` default and with `offsite_s3`
 * default.
 *
 * These drive the real Settings page against a mock API rather than
 * rendering the row on its own, for the reason tier-destination-picker's
 * cases give: every claim is about what an operator can reach.
 */

const VERSION: VersionInfo = {
  api: "v1", service: "1.3.0", buildCommit: "9f4c1ab", goVersion: "go1.27.0",
  engine: "1.68.2", configRevision: "cfg_9f4c1ab", ready: true, compatible: true
};

const SCHEMA = {
  granularities: ["day", "week", "month", "quarter", "half_year", "year", "days"],
  windowUnits: ["day", "week", "month", "quarter", "half_year", "year"],
  tierNamePattern: "^[a-z][a-z0-9_]*$",
  reservedTierName: "last_known_good",
  keepMax: 10000,
  periodDaysMax: 3650,
  defaultTiers: [{ name: "daily", granularity: "day", keep: 7, medium: LOCAL_DESTINATION_ID }]
};

const STORAGE = {
  verificationClasses: [],
  mediumDisclosure: "Backups that only this tier keeps will live only on that storage medium.",
  retrievalDisclosure: "Reading a copy back off a storage medium is billed by your provider."
};

/** The seeded local volume (#670): a DECLARED destination, instance zero
 *  of the local_volume backend, and the default until somebody moves it. */
const LOCAL: StorageMedium = {
  id: LOCAL_DESTINATION_ID,
  type: "local_volume",
  bucket: "",
  path: "/srv/backups",
  storageClass: "",
  uploadVerification: "readback",
  readsRequireRestore: false,
  isLocal: true,
  isDefault: true,
  connectionUnverified: false
};

const OFFSITE: StorageMedium = {
  id: "offsite_s3", type: "s3", bucket: "nas-backups", region: "us-east-1",
  storageClass: "STANDARD_IA", uploadVerification: "readback",
  readsRequireRestore: false, isLocal: false, isDefault: false,
  connectionUnverified: false
};

function settingsFixture(mediums: StorageMedium[]): AppSettings {
  return {
    retention: {
      timezone: "UTC",
      weekStartsOn: "monday",
      tiers: [{ name: "daily", granularity: "day", keep: 7, medium: LOCAL_DESTINATION_ID }],
      protectLastKnownGood: true
    },
    capacity: {
      capBytes: 0, warningFreeBytes: 0, criticalFreeBytes: 0, safetyMarginBytes: 0,
      backupRoot: "/srv/backups", backupRootConfigured: false
    },
    mediums,
    schema: { retention: SCHEMA, storage: STORAGE }
  };
}

/** The engine as this feature meets it: one list, one default, and a
 *  transfer that moves the mark in a single answer. Stateful rather than
 *  stubbed per call, because "the OLD default became removable" is a
 *  claim about the list AFTER the move and a fixed stub cannot make it. */
function engine(initial: StorageMedium[]) {
  let mediums = initial.map((m) => ({ ...m }));
  const setDefaultStorageMedium = vi.fn((id: string) => {
    mediums = mediums.map((m) => ({ ...m, isDefault: m.id === id }));
    return Promise.resolve(mediums.find((m) => m.id === id)!);
  });
  const removeStorageMedium = vi.fn((id: string) => {
    mediums = mediums.filter((m) => m.id !== id);
    return Promise.resolve();
  });
  return {
    setDefaultStorageMedium,
    removeStorageMedium,
    api: {
      ...createMockApi(),
      getSettings: vi.fn(() => Promise.resolve(settingsFixture(mediums))),
      listStorageMediums: vi.fn(() => Promise.resolve(mediums)),
      setDefaultStorageMedium,
      removeStorageMedium
    }
  };
}

async function renderSettings(initial: StorageMedium[]) {
  const e = engine(initial);
  act(() => {
    graph.commit("test/seed-version", (tx) =>
      tx.set(versionNode, { data: VERSION, error: null, loading: false })
    );
  });
  render(
    <MemoryRouter>
      <ApiProvider api={e.api}>
        <PlatformProvider bridge={genericBridge}>
          <SettingsPage readOnly={false} />
        </PlatformProvider>
      </ApiProvider>
    </MemoryRouter>
  );
  await act(async () => {});
  await waitFor(() => expect(screen.getByRole("group", { name: "Storage destination " + initial[0].id })).toBeTruthy());
  return e;
}

const row = (id: string) => within(screen.getByRole("group", { name: "Storage destination " + id }));
const control = (id: string, name: string) => row(id).getByRole("button", { name }) as HTMLButtonElement;

/** The sentence a disabled control points at.
 *
 *  Read through aria-describedby rather than by searching the row for
 *  some text, because "there is an explanation somewhere on this row" is
 *  satisfied by a row that happens to carry any prose at all. What has to
 *  be true is that THIS control says why it is off, which is also what a
 *  screen reader announces. */
function explanationFor(button: HTMLButtonElement): string {
  const id = button.getAttribute("aria-describedby");
  expect(id, "a disabled control with no aria-describedby explains nothing").toBeTruthy();
  const node = document.getElementById(id!);
  expect(node, "aria-describedby names #" + id + ", which is not on the page").toBeTruthy();
  return node!.textContent ?? "";
}

describe("handing the default from one destination to another (#671)", () => {
  beforeEach(() => resetGraphForTests());
  afterEach(() => {
    cleanup();
    resetGraphForTests();
  });

  it("offers Make default on every destination that is not the default", async () => {
    await renderSettings([LOCAL, OFFSITE]);

    expect(control("offsite_s3", "Make default")).toBeEnabled();
    // And names the command it is equivalent to, on the row, which is
    // EPIC G's standing rule and the line an operator scripts from.
    expect(row("offsite_s3").getByText("rbm medium default offsite_s3")).toBeTruthy();
  });

  // The reversal of #622's choice, and the whole point of the issue. The
  // control that would move the mark OFF this row is the one an operator
  // looking at the row they cannot delete needs to find, and it is on the
  // OTHER rows. Hiding it here leaves them with a row that refuses two
  // things and offers no way out.
  it("keeps Make default on the default's own row, disabled and explained", async () => {
    await renderSettings([LOCAL, OFFSITE]);

    const button = control(LOCAL_DESTINATION_ID, "Make default");
    expect(button).toBeDisabled();
    expect(explanationFor(button)).toMatch(/already/i);
  });

  it("keeps Remove on the default's own row, disabled and explained", async () => {
    await renderSettings([LOCAL, OFFSITE]);

    const button = control(LOCAL_DESTINATION_ID, "Remove");
    expect(button).toBeDisabled();
    expect(explanationFor(button)).toMatch(/cannot be removed while/i);
    // The way out is named. A refusal that does not say what would lift
    // it is the dead end this issue is about.
    expect(explanationFor(button)).toMatch(/another destination/i);
  });

  // #670 deleted the local-shaped refusal, so the reason is generic. The
  // same row, the same sentence, with a bucket in the role: a line that
  // only made sense about the local drive would fail here.
  it("explains the refusal the same way whatever destination is the default", async () => {
    await renderSettings([{ ...LOCAL, isDefault: false }, { ...OFFSITE, isDefault: true }]);

    const said = explanationFor(control("offsite_s3", "Remove"));
    expect(said).toMatch(/cannot be removed while/i);
    expect(said).not.toMatch(/local|hard drive/i);
  });

  // #670's own service test asserts that local is removable once it is
  // not the default. Before this issue there was no way to reach that
  // state from a browser: the mark could not be moved off local from the
  // list, and local's Remove was absent on top of that.
  it("makes the old default removable, and local is no exception", async () => {
    const e = await renderSettings([LOCAL, OFFSITE]);

    expect(control(LOCAL_DESTINATION_ID, "Remove")).toBeDisabled();

    fireEvent.click(control("offsite_s3", "Make default"));
    fireEvent.click(await screen.findByRole("button", { name: /Make offsite_s3 the default/ }));
    await waitFor(() => expect(e.setDefaultStorageMedium).toHaveBeenCalledWith("offsite_s3"));

    await waitFor(() => expect(control(LOCAL_DESTINATION_ID, "Remove")).toBeEnabled());
    fireEvent.click(control(LOCAL_DESTINATION_ID, "Remove"));
    await waitFor(() => expect(e.removeStorageMedium).toHaveBeenCalledWith(LOCAL_DESTINATION_ID));
  });

  // Two things change and only one of them is the thing the operator
  // clicked. An operator told half of what their click did has been
  // misled by omission, and the half they are not told is the one that
  // makes a destination deletable.
  it("names both consequences before the transfer, and sends nothing until confirmed", async () => {
    const e = await renderSettings([LOCAL, OFFSITE]);

    fireEvent.click(control("offsite_s3", "Make default"));

    const said = (await screen.findByRole("dialog")).textContent ?? "";
    expect(said).toMatch(/offsite_s3/);
    // Consequence one: the new destination takes the role, and stops
    // being deletable.
    expect(said).toMatch(/becomes? the default/i);
    // Consequence two: the old one gives it up, and becomes deletable.
    expect(said).toMatch(new RegExp(LOCAL_DESTINATION_ID));
    expect(said).toMatch(/remov(able|ed)/i);
    // And the thing that does NOT change, because an operator will assume
    // otherwise: no copy already written moves.
    expect(said).toMatch(/no backup|nothing already|no copy/i);

    expect(e.setDefaultStorageMedium).not.toHaveBeenCalled();
  });

  it("moves the mark and leaves exactly one default", async () => {
    const e = await renderSettings([LOCAL, OFFSITE]);

    fireEvent.click(control("offsite_s3", "Make default"));
    fireEvent.click(await screen.findByRole("button", { name: /Make offsite_s3 the default/ }));

    await waitFor(() => expect(e.setDefaultStorageMedium).toHaveBeenCalledWith("offsite_s3"));
    await waitFor(() => expect(screen.getAllByText("Default")).toHaveLength(1));
    expect(row("offsite_s3").getByText("Default")).toBeTruthy();
    expect(control("offsite_s3", "Make default")).toBeDisabled();
    expect(e.setDefaultStorageMedium).toHaveBeenCalledTimes(1);
  });

  // Nothing unverified is seeded any more, but the state is still
  // reachable: a destination added and abandoned before its connection
  // test sits exactly there. Making it the default would point every tier
  // that follows the default at something nobody proved, so the control
  // is off BEFORE the click rather than failing after it.
  it("refuses the role to a destination nobody has proven, and says so before the click", async () => {
    const e = await renderSettings([LOCAL, { ...OFFSITE, connectionUnverified: true }]);

    const button = control("offsite_s3", "Make default");
    expect(button).toBeDisabled();
    expect(explanationFor(button)).toMatch(/never been proven|has not been proven|test connection/i);

    fireEvent.click(button);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(e.setDefaultStorageMedium).not.toHaveBeenCalled();
  });

  // The last destination cannot hand the role away because there is
  // nobody to hand it to, so its Make default is moot and its Remove
  // stays off. That is the same rule rather than a special case, and it
  // is the one row where BOTH controls are dead: it had better say why.
  it("leaves the last remaining destination with both controls off and both explained", async () => {
    await renderSettings([LOCAL]);

    expect(control(LOCAL_DESTINATION_ID, "Make default")).toBeDisabled();
    expect(explanationFor(control(LOCAL_DESTINATION_ID, "Make default"))).toMatch(/already/i);
    expect(control(LOCAL_DESTINATION_ID, "Remove")).toBeDisabled();
    expect(explanationFor(control(LOCAL_DESTINATION_ID, "Remove"))).toMatch(/cannot be removed while/i);
  });
});
