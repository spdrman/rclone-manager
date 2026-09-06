import type { ReactNode } from "react";
import { WarningBanner } from "./WarningBanner";
import type { BackupSet } from "@shared/types/backup";

/**
 * The banner for a backup set the manager could not connect to (#245).
 *
 * One component for both screens that raise it, so the dashboard and the
 * set's own page cannot describe the same refusal differently. Before this
 * they carried two hand-written copies of the host-key wording and neither
 * could fire at all, because `haltReason` had no producer anywhere in the
 * running system.
 *
 * It renders nothing when no reason is on record. That is the whole
 * behaviour absent is supposed to buy: a set nothing is known about gets
 * silence rather than a reassuring "everything is fine", and a reason this
 * build does not recognise never reaches here (api/client.ts maps an
 * unknown one to absent) so there is no case where a banner appears with
 * words that do not fit it.
 *
 * # Why it offers no way out
 *
 * §77 invariant 5: re-trusting a changed SSH host key is an explicit
 * administrator action taken out of band, and this manager will not do it
 * on its own. So this reports, and any `actions` a page passes are for
 * navigating to the evidence, never for dismissing, retrying or resuming.
 * The two buttons that used to sit on the detail page's copy of this
 * banner, "Compare fingerprints" and "Keep set halted", had no onClick at
 * all: controls that looked like actions and were not, which is the same
 * defect one level along from the field this issue gave a producer.
 */

type HaltCopy = { eyebrow: string; title: string; body: string };

const HALT_COPY: Record<NonNullable<BackupSet["haltReason"]>, (host: string) => HaltCopy> = {
  "host-key-changed": (host) => ({
    eyebrow: "Security warning",
    title: "The SSH host key for " + host + " has changed",
    body:
      "Backup operations for this set have been stopped until the new fingerprint is " +
      "independently verified. No remote artifacts will be deleted while the set is halted."
  }),
  "authentication-failed": (host) => ({
    eyebrow: "Connection refused",
    title: "Backup Manager could not log in to " + host,
    body:
      "The host rejected the credentials this backup set is configured with, so no backup ran " +
      "and no remote artifacts were deleted. Check the key or the account this set uses on the " +
      "server itself; the manager will not try anything else on its own."
  }),
  "key-permissions": () => ({
    eyebrow: "Key permissions",
    title: "The SSH key for this backup set has the wrong permissions",
    body:
      "The private key on disk no longer has the permissions it was imported with, so the " +
      "manager refused to use it before ever reaching out to the server: no backup ran and no " +
      "remote artifacts were deleted. Fix the key file's permissions on this machine (or " +
      "re-import the key) and the next run will pick it up on its own."
  })
};

export function HaltBanner({ set, actions }: { set: BackupSet; actions?: ReactNode }) {
  if (!set.haltReason) return null;
  const copy = HALT_COPY[set.haltReason](set.host);
  return (
    <WarningBanner tone="danger" eyebrow={copy.eyebrow} title={copy.title} actions={actions}>
      {copy.body}
    </WarningBanner>
  );
}
