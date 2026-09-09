/**
 * The preferences this UI remembers in the browser, and the one-time move
 * off the key names it used before 0.3.3.
 *
 * # What is stored here and what is not
 *
 * Only per-viewer chrome: which theme is on, and whether the docked
 * terminal is open, how tall it is and what it is filtered to. None of it
 * is configuration, none of it reaches the engine, and every reader below
 * has a default that is correct when nothing is stored at all. A browser
 * with site data blocked throws on the very first access rather than
 * returning null, so both helpers swallow that and answer as if nothing
 * was ever written: a blocked browser gets the defaults, not a page that
 * throws into an operator's face.
 *
 * # Why each key carries a second name
 *
 * 0.3.3 renamed the CLI to `rbm`, and the naming pass that followed it
 * renamed this namespace from `backup-manager.*` to `rclone-manager.*` so
 * the product spells itself one way everywhere. A bare rename would have
 * been a silent reset: a key that is not there is indistinguishable from
 * one that was never set, so every operator upgrading across 0.3.3 would
 * have had their theme flipped back to light and their terminal back to
 * 240px, with nothing on screen saying why.
 *
 * So the rename is a rename plus an adoption. LEGACY_KEYS names what each
 * key used to be called, and the first read that finds the new key absent
 * and the old one present takes the old value and writes it back under the
 * new name. From the second read onwards the new key answers on its own.
 *
 * The adoption COPIES rather than moves. Leaving the old key where it is
 * costs a few bytes in one origin's storage and means an operator who
 * rolls back to 0.3.2 still finds their preferences, which is worth more
 * than the tidiness of deleting it.
 *
 * # Deleting this
 *
 * The whole migration is LEGACY_KEYS and the one branch in readStored that
 * reads it. Once nobody is upgrading across 0.3.3 any more, delete both
 * and the keys stand on their own; nothing else has to change, and
 * browserStorage.test.ts's "adopts" cases are the ones that go with them.
 */

/** The keys, spelled once. */
export const STORAGE_KEYS = {
  theme: "rclone-manager.theme",
  dockOpen: "rclone-manager.dock.open",
  dockHeight: "rclone-manager.dock.height",
  dockFilter: "rclone-manager.dock.filter"
} as const;

export type StorageKey = (typeof STORAGE_KEYS)[keyof typeof STORAGE_KEYS];

/** What each key was called before 0.3.3. Delete with the migration. */
export const LEGACY_KEYS: Record<StorageKey, string> = {
  [STORAGE_KEYS.theme]: "backup-manager.theme",
  [STORAGE_KEYS.dockOpen]: "backup-manager.dock.open",
  [STORAGE_KEYS.dockHeight]: "backup-manager.dock.height",
  [STORAGE_KEYS.dockFilter]: "backup-manager.dock.filter"
};

function readRaw(key: string): string | null {
  try {
    return window.localStorage.getItem(key);
  } catch {
    // A browser with site data blocked is a browser where the panel opens
    // at its default, not one where the panel throws into a page an
    // operator is reading.
    return null;
  }
}

function writeRaw(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value);
  } catch {
    /* see readRaw */
  }
}

/** Read one preference, adopting the pre-0.3.3 value the first time.
 *
 *  Returns null when neither name holds anything, which is the caller's
 *  cue to use its own default. */
export function readStored(key: StorageKey): string | null {
  const current = readRaw(key);
  if (current !== null) return current;

  // The migration. A value under the new name always wins, so an operator
  // who has changed a preference since upgrading is never dragged back to
  // what the old key still says.
  const inherited = readRaw(LEGACY_KEYS[key]);
  if (inherited === null) return null;
  writeRaw(key, inherited);
  return inherited;
}

/** Write one preference under its current name, and only that one. */
export function writeStored(key: StorageKey, value: string): void {
  writeRaw(key, value);
}
