// jest-dom registers the DOM matchers the tests already use
// (toBeDisabled, toBeEnabled). Without it those assertions do not exist
// and tsc rejects them.
import "@testing-library/jest-dom/vitest";
import "@testing-library/react";

// jsdom, as vitest configures it here, ships no Storage implementation:
// `window.localStorage` is undefined, not an empty store. Nothing noticed
// until issue #275, because no test had ever rendered <App/> itself, and
// App.tsx reads the theme out of localStorage on its very first render, so
// the whole application threw before any assertion could run. That absence
// is a large part of why an App-level defect (which surface renders at all)
// had no test that could have caught it.
//
// A real in-memory Storage, not a no-op: a component that writes a
// preference and reads it back has to see what it wrote.
function storageIsUsable(): boolean {
  try {
    // Node 26 defines a `localStorage` global of its own that throws (or
    // reads back undefined) unless the process was started with
    // --localstorage-file, so an `in` check is not enough: the property is
    // present and unusable. Use it, and believe the answer.
    const probe = window.localStorage;
    if (!probe) return false;
    probe.setItem("backup-manager.storage-probe", "1");
    probe.removeItem("backup-manager.storage-probe");
    return true;
  } catch {
    return false;
  }
}

if (typeof window !== "undefined" && !storageIsUsable()) {
  const store = new Map<string, string>();
  const storage: Storage = {
    get length() {
      return store.size;
    },
    clear: () => store.clear(),
    getItem: (key) => store.get(key) ?? null,
    key: (index) => [...store.keys()][index] ?? null,
    removeItem: (key) => void store.delete(key),
    setItem: (key, value) => void store.set(key, String(value))
  };
  Object.defineProperty(window, "localStorage", { value: storage, configurable: true });
}
