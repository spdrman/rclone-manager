/**
 * The pre-0.3.3 storage-key migration.
 *
 * The rename from `backup-manager.*` to `rclone-manager.*` is only safe
 * because of the adoption in readStored, and an untested migration is
 * worse than none: it is code that exists solely to be silently wrong
 * later, on the one machine nobody is watching, at the one moment nobody
 * repeats. So the adopt path is asserted directly, and so is the case that
 * would make it harmful, an old value overwriting a newer choice.
 *
 * The "returns null" cases carry the weight people usually skip. Every
 * caller treats null as "use the default", so a migration that quietly
 * returned "" or "null" instead would flip a theme or collapse a panel
 * without ever failing an assertion about the value it returned.
 */
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { LEGACY_KEYS, STORAGE_KEYS, readStored, writeStored } from "@shared/utilities/browserStorage";

function clear(): void {
  try {
    window.localStorage.clear();
  } catch {
    /* a browser with site data blocked */
  }
}

beforeEach(clear);
afterEach(clear);

describe("the pre-0.3.3 key names", () => {
  // A pin, not a tautology: these four strings are what is actually
  // sitting in an upgrading operator's browser, so a typo here would make
  // the migration a no-op that every other case in this file still
  // passes, because they all read LEGACY_KEYS rather than the literal.
  it("are the exact names the UI wrote before the rename", () => {
    expect(LEGACY_KEYS[STORAGE_KEYS.theme]).toBe("backup-manager.theme");
    expect(LEGACY_KEYS[STORAGE_KEYS.dockOpen]).toBe("backup-manager.dock.open");
    expect(LEGACY_KEYS[STORAGE_KEYS.dockHeight]).toBe("backup-manager.dock.height");
    expect(LEGACY_KEYS[STORAGE_KEYS.dockFilter]).toBe("backup-manager.dock.filter");
  });

  it("are all distinct from the names in use now", () => {
    for (const key of Object.values(STORAGE_KEYS)) {
      expect(LEGACY_KEYS[key]).not.toBe(key);
    }
  });
});

describe("adopting a value written before the rename", () => {
  it("returns the old key's value when only the old key is set", () => {
    window.localStorage.setItem(LEGACY_KEYS[STORAGE_KEYS.theme], "dark");

    expect(readStored(STORAGE_KEYS.theme)).toBe("dark");
  });

  it("writes the adopted value back under the new name, so it is adopted once", () => {
    window.localStorage.setItem(LEGACY_KEYS[STORAGE_KEYS.dockHeight], "400");

    expect(readStored(STORAGE_KEYS.dockHeight)).toBe("400");
    // The half that makes it a migration rather than a permanent fallback.
    expect(window.localStorage.getItem(STORAGE_KEYS.dockHeight)).toBe("400");
  });

  it("leaves the old key in place, so a rollback to 0.3.2 still finds it", () => {
    window.localStorage.setItem(LEGACY_KEYS[STORAGE_KEYS.dockFilter], "problems");

    readStored(STORAGE_KEYS.dockFilter);

    expect(window.localStorage.getItem(LEGACY_KEYS[STORAGE_KEYS.dockFilter])).toBe("problems");
  });

  it("adopts every key, not just the one that got tested", () => {
    for (const key of Object.values(STORAGE_KEYS)) {
      clear();
      window.localStorage.setItem(LEGACY_KEYS[key], "inherited");

      expect(readStored(key)).toBe("inherited");
      expect(window.localStorage.getItem(key)).toBe("inherited");
    }
  });

  it("adopts a falsy stored value rather than reading it as absent", () => {
    // "0" is how the dock spells "collapsed", and it is the value a
    // truthiness check would drop on the floor. An operator who collapsed
    // the panel before upgrading has to find it collapsed after.
    window.localStorage.setItem(LEGACY_KEYS[STORAGE_KEYS.dockOpen], "0");

    expect(readStored(STORAGE_KEYS.dockOpen)).toBe("0");
    expect(window.localStorage.getItem(STORAGE_KEYS.dockOpen)).toBe("0");
  });
});

describe("not adopting", () => {
  it("prefers the new key when both are set, so a newer choice is never undone", () => {
    // The case that makes a careless migration actively harmful: the
    // operator upgraded, switched back to light, and the old key still
    // says dark. Reading the old one here would fight every change they
    // make, on every reload, forever.
    window.localStorage.setItem(LEGACY_KEYS[STORAGE_KEYS.theme], "dark");
    window.localStorage.setItem(STORAGE_KEYS.theme, "light");

    expect(readStored(STORAGE_KEYS.theme)).toBe("light");
    expect(window.localStorage.getItem(STORAGE_KEYS.theme)).toBe("light");
  });

  it("returns null when neither name holds anything, so the caller's default wins", () => {
    expect(readStored(STORAGE_KEYS.theme)).toBeNull();
    expect(readStored(STORAGE_KEYS.dockOpen)).toBeNull();
    expect(readStored(STORAGE_KEYS.dockHeight)).toBeNull();
    expect(readStored(STORAGE_KEYS.dockFilter)).toBeNull();
  });

  it("writes nothing under either name when there was nothing to adopt", () => {
    readStored(STORAGE_KEYS.dockHeight);

    expect(window.localStorage.getItem(STORAGE_KEYS.dockHeight)).toBeNull();
    expect(window.localStorage.getItem(LEGACY_KEYS[STORAGE_KEYS.dockHeight])).toBeNull();
  });
});

describe("writeStored", () => {
  it("writes the new name only, and never touches the old one", () => {
    window.localStorage.setItem(LEGACY_KEYS[STORAGE_KEYS.theme], "dark");

    writeStored(STORAGE_KEYS.theme, "light");

    expect(window.localStorage.getItem(STORAGE_KEYS.theme)).toBe("light");
    expect(window.localStorage.getItem(LEGACY_KEYS[STORAGE_KEYS.theme])).toBe("dark");
  });

  it("round-trips through readStored without going near the migration", () => {
    writeStored(STORAGE_KEYS.dockFilter, "mine");

    expect(readStored(STORAGE_KEYS.dockFilter)).toBe("mine");
  });
});
