// Unit tests for the suite's port resolution (e2e/port.ts). A vitest file,
// not a Playwright spec: it runs in `npm test` with the rest of the unit
// suite, so the rule it guards is checked on every gate run rather than
// only when someone runs the browser suite by hand.
import { describe, expect, it } from "vitest";
import { PORT_FLOOR, PORT_SLOTS, derivePort, resolveE2EPort } from "./port";

const ROOT_A = "/Users/dev/workspace/rclone-manager";
const ROOT_B = "/Users/dev/workspace/rclone-manager/.claude/worktrees/172-e2e-ordering-flake";

describe("derivePort", () => {
  it("stays inside the derived range", () => {
    for (let i = 0; i < 500; i++) {
      const port = derivePort(ROOT_A + "/worktrees/w" + i);
      expect(port).toBeGreaterThanOrEqual(PORT_FLOOR);
      expect(port).toBeLessThan(PORT_FLOOR + PORT_SLOTS);
    }
  });

  it("is stable for one checkout, so a URL stays bookmarkable", () => {
    expect(derivePort(ROOT_A)).toBe(derivePort(ROOT_A));
  });

  it("separates a worktree from the checkout it was made from", () => {
    // The #172 case: the main checkout and a worktree of it, running the
    // suite at the same time, must not both aim at one port.
    expect(derivePort(ROOT_A)).not.toBe(derivePort(ROOT_B));
  });

  it("spreads fifty worktrees across the range without piling up", () => {
    const ports = new Set<number>();
    for (let i = 0; i < 50; i++) ports.add(derivePort(ROOT_A + "/.claude/worktrees/branch-" + i));
    // 50 draws from 1800 slots: a collision is possible, a pile-up is not.
    expect(ports.size).toBeGreaterThanOrEqual(48);
  });
});

describe("resolveE2EPort", () => {
  it("derives from the checkout when E2E_PORT is unset", () => {
    expect(resolveE2EPort({}, ROOT_A)).toBe(derivePort(ROOT_A));
  });

  it("takes an explicit E2E_PORT over the derived default", () => {
    expect(resolveE2EPort({ E2E_PORT: "5273" }, ROOT_A)).toBe(5273);
    expect(resolveE2EPort({ E2E_PORT: " 6100 " }, ROOT_A)).toBe(6100);
  });

  it("treats an empty E2E_PORT as unset rather than as port 0", () => {
    // `Number(process.env.E2E_PORT ?? 5273)` used to make this 0, because
    // ?? only substitutes for null and undefined. A shell wrapper or a CI
    // matrix entry expanding an unset variable is where "" comes from.
    expect(resolveE2EPort({ E2E_PORT: "" }, ROOT_A)).toBe(derivePort(ROOT_A));
    expect(resolveE2EPort({ E2E_PORT: "   " }, ROOT_A)).toBe(derivePort(ROOT_A));
  });

  it("rejects a value that is not a port, naming the variable and the value", () => {
    for (const bad of ["abc", "0", "65536", "-1", "5273.5", "5273x"]) {
      expect(() => resolveE2EPort({ E2E_PORT: bad }, ROOT_A)).toThrow(/E2E_PORT/);
      expect(() => resolveE2EPort({ E2E_PORT: bad }, ROOT_A)).toThrow(new RegExp(JSON.stringify(bad).slice(1, -1)));
    }
  });
});
