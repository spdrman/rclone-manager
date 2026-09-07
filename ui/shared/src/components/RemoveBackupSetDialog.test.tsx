/**
 * What the removal confirmation promises, and what it must not claim.
 *
 * The dialog's whole job is the safety story of an action a button cannot
 * undo, and its promise is a real one the backend keeps: collection stops,
 * every backup already taken stays on NAS storage and stays listed under
 * Backups. That sentence used to carry a count and a byte total in front
 * of it, and neither was a number this frontend had. `retainedCount` and
 * `retainedBytes` are literals in `fromWireBackupSet`, zero on every set
 * in every real deployment, because nothing in core/service computes a
 * per-set retained aggregate and neither `BackupSet` nor `BackupSetHealth`
 * carries one.
 *
 * So an operator removing a set holding forty-one backups read "0 retained
 * backups (0 B) stay on NAS storage and remain listed under Backups." That
 * is not a cosmetic wrong number. It is reassurance-shaped copy in a
 * destructive-adjacent dialog that reads as "there is nothing here to
 * lose", about a set holding everything, at the exact moment somebody
 * decides whether to click.
 *
 * Removing a false claim does not need the true number, which is why the
 * copy is quantifier-free rather than corrected. If it reads oddly without
 * one, that oddness is honest, and it can be filled in once the aggregate
 * exists.
 *
 * The second case is the one that matters most and it is deliberately NOT
 * about zero. The mock builds BackupSet objects directly, with plausible
 * counters, so every test and every design review saw a believable number
 * while the only path a real browser takes produced 0. A component that
 * merely stopped saying "0" would still be stating a count it cannot
 * stand behind the day the fixture says 41.
 */
import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { RemoveBackupSetDialog } from "./RemoveBackupSetDialog";
import { ApiProvider } from "@shared/api/ApiContext";
import { createMockApi } from "@shared/api/mock";
import type { BackupSet } from "@shared/types/backup";

const SET: BackupSet = {
  id: "production/postgres-primary",
  source: "production",
  set: "postgres-primary",
  name: "Production PostgreSQL",
  host: "prod-db-01.internal",
  port: 22,
  username: "backup-agent",
  remoteFolder: "/backups/postgresql/",
  includePatterns: ["*.dump.zst"],
  excludePatterns: [],
  completionMethod: "completion-marker",
  stableForSeconds: 0,
  destination: "/data/backups/production/postgres/",
  retentionIsOverride: false,
  validations: ["transfer", "checksum"],
  state: "healthy",
  stateNote: "Verified nightly dump.",
  enabled: true,
  readOnly: false,
  readOnlyRetainedCount: 0,
  newestKnownGoodAt: "2026-08-29T02:01:01+02:00",
  lastRunAt: null,
  lastValidation: "not-run",
  expectedIntervalHours: 0,
  retainedCount: 0,
  retainedBytes: 0,
  hostFingerprint: "",
  fingerprintTrustedAt: null
};

function open(set: BackupSet) {
  render(
    <ApiProvider api={createMockApi()}>
      <RemoveBackupSetDialog set={set} open onCancel={() => {}} onRemoved={() => {}} />
    </ApiProvider>
  );
  return screen.getByRole("dialog");
}

describe("the removal confirmation", () => {
  it("keeps the promise the backend actually makes", () => {
    const dialog = open(SET);
    expect(within(dialog).getByText(/stay on NAS storage and remain listed under Backups/)).toBeTruthy();
  });

  it("states no count of what stays, on a set reporting none", () => {
    const dialog = open(SET);
    const text = dialog.textContent ?? "";
    // "0 retained backups (0 B)" is what every real deployment rendered,
    // because the two fields behind it are literals rather than reads.
    expect(text).not.toMatch(/0 retained backups/);
    expect(text).not.toMatch(/0 B/);
  });

  it("states no count of what stays even when a count is available to state", () => {
    // The important half. This frontend cannot stand behind either number
    // whatever they happen to hold, so a dialog that only stopped saying
    // "0" would be wrong again the moment a fixture said 41. What has to
    // be true is that the sentence does not quantify at all.
    const dialog = open({ ...SET, retainedCount: 41, retainedBytes: 12 * 1024 ** 3 });
    const text = dialog.textContent ?? "";
    expect(text).not.toMatch(/41/);
    expect(text).not.toMatch(/\d+(\.\d+)?\s?(B|KB|MB|GB|TB)\b/);
    expect(text).not.toMatch(/retained backups/);
  });
});
