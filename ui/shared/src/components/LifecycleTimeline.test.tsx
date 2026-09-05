/**
 * The one ordering rule the timeline exists to keep: nothing can show the
 * remote original as deleted before the local copy is committed.
 *
 * Asserted against `buildPhases` rather than the rendered list, because
 * the property is about the data the component is given and a DOM
 * assertion would also be testing the layout. The rendering case is here
 * too, but only to prove the phases reach the screen at all.
 */
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import type { BackupArtifact } from "@shared/types/backup";
import { buildPhases, LifecycleTimeline } from "./LifecycleTimeline";

const BASE: BackupArtifact = {
  id: "art_1",
  setId: "set_1",
  setName: "Test set",
  filename: "test.tar.zst",
  remoteOriginalPath: "host:/remote/test.tar.zst",
  localPath: "/local/test.tar.zst",
  producedAt: "2026-08-29T02:00:11+02:00",
  receivedAt: "2026-08-29T02:00:53+02:00",
  sizeBytes: 1024,
  checksum: "abc123",
  checksumAlgorithm: "sha256",
  validation: "verified",
  retentionClasses: ["daily"],
  remoteSourceRemovedAt: "2026-08-29T02:01:01+02:00",
  quarantine: null,
  placements: [
    {
      medium: "local", mediumType: "local", location: "/local/test.tar.zst",
      sizeBytes: 1024, storageClass: "",
      verificationClass: "content", verifiedAt: "2026-08-29T02:00:53+02:00",
      access: "immediate", status: "ACTIVE"
    }
  ]
};

const ORDER = [
  "DISCOVERED", "TRANSFERRED", "VERIFIED", "COMMITTED",
  "SAFE STATE PERSISTED", "REMOTE SOURCE DELETED"
];

describe("buildPhases", () => {
  it("marks every phase reached, in the documented order, once remote deletion has completed", () => {
    const phases = buildPhases(BASE);
    expect(phases.map((p) => p.label)).toEqual(ORDER);
    expect(phases.every((p) => p.at !== null)).toBe(true);
    expect(phases.at(-1)?.terminalRemote).toBe(true);
  });

  // §15/§28: remote deletion pending is the NORMAL case, not a fault, so
  // every earlier phase stays reached even while it is still unreached.
  it("leaves remote deletion unreached while the remote original is still retained", () => {
    const artifact: BackupArtifact = { ...BASE, remoteSourceRemovedAt: null };
    const phases = buildPhases(artifact);
    const last = phases.at(-1);
    expect(last?.label).toBe("REMOTE SOURCE DELETED");
    expect(last?.at).toBeNull();
    expect(last?.detail).toMatch(/still retained/);
    expect(phases.slice(0, -1).every((p) => p.at !== null)).toBe(true);
  });

  it("never marks verification/commit phases reached before validation has actually passed", () => {
    const artifact: BackupArtifact = { ...BASE, validation: "pending", remoteSourceRemovedAt: null };
    const phases = buildPhases(artifact);
    const byLabel = Object.fromEntries(phases.map((p) => [p.label, p]));
    expect(byLabel.DISCOVERED.at).not.toBeNull();
    expect(byLabel.TRANSFERRED.at).not.toBeNull();
    expect(byLabel.VERIFIED.at).toBeNull();
    expect(byLabel.COMMITTED.at).toBeNull();
    expect(byLabel["SAFE STATE PERSISTED"].at).toBeNull();
    expect(byLabel["REMOTE SOURCE DELETED"].at).toBeNull();
  });
});

describe("LifecycleTimeline", () => {
  it("renders the phases in the one correct chronological order", () => {
    render(<LifecycleTimeline artifact={BASE} />);
    const text = screen.getByRole("list").textContent ?? "";
    const positions = ORDER.map((label) => text.indexOf(label));
    for (const p of positions) expect(p).toBeGreaterThan(-1);
    for (let i = 1; i < positions.length; i++) {
      expect(positions[i]).toBeGreaterThan(positions[i - 1]);
    }
  });

  it("renders one list item per phase", () => {
    render(<LifecycleTimeline artifact={BASE} />);
    expect(screen.getAllByRole("listitem")).toHaveLength(ORDER.length);
  });
});
