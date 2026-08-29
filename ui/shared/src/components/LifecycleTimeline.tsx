import type { BackupArtifact } from "@shared/types/backup";
import { clock } from "@shared/utilities/format";

interface Phase {
  label: string;
  detail: string;
  at: string | null;
  terminalRemote?: boolean;
}

/** The timeline is derived, not authored: remote deletion can only ever render
 *  AFTER commit because it is read from remoteSourceRemovedAt, which the service
 *  sets only once the safe state is persisted (§14, §15). */
export function buildPhases(artifact: BackupArtifact): Phase[] {
  const phases: Phase[] = [
    { label: "DISCOVERED", detail: "Completion signal seen on the remote server", at: artifact.producedAt },
    { label: "TRANSFERRED", detail: "Received over SFTP", at: artifact.receivedAt },
    {
      label: "VERIFIED",
      detail: artifact.checksumAlgorithm.toUpperCase() + " matched the producer manifest",
      at: artifact.validation === "verified" ? artifact.receivedAt : null
    },
    {
      label: "COMMITTED",
      detail: "Durably written and fsynced to NAS storage",
      at: artifact.validation === "verified" ? artifact.receivedAt : null
    },
    {
      label: "SAFE STATE PERSISTED",
      detail: "Catalog records this artifact as known-good",
      at: artifact.validation === "verified" ? artifact.receivedAt : null
    },
    {
      label: "REMOTE SOURCE DELETED",
      detail: artifact.remoteSourceRemovedAt
        ? "Original removed from the remote server after commit"
        : "Pending \u2014 the remote original is still retained",
      at: artifact.remoteSourceRemovedAt,
      terminalRemote: true
    }
  ];
  return phases;
}

export function LifecycleTimeline({ artifact }: { artifact: BackupArtifact }) {
  const phases = buildPhases(artifact);

  return (
    <ol style={{ margin: 0, padding: "20px 22px 22px", listStyle: "none" }}>
      {phases.map((p, i) => {
        const reached = p.at !== null;
        const last = i === phases.length - 1;
        return (
          <li
            key={p.label}
            style={{
              display: "grid", gridTemplateColumns: "18px 1fr auto",
              gap: 14, alignItems: "start"
            }}
          >
            <span
              aria-hidden="true"
              style={{
                display: "grid", placeItems: "center", paddingTop: 3, position: "relative",
                background: last
                  ? undefined
                  : "linear-gradient(var(--border), var(--border)) no-repeat center / 1px 100%"
              }}
            >
              <span
                style={{
                  display: "block", width: 9, height: 9, borderRadius: "50%",
                  outline: "3px solid var(--surface)",
                  background: !reached
                    ? "var(--border-strong)"
                    : p.terminalRemote
                      ? "var(--warn)"
                      : "var(--ok)"
                }}
              />
            </span>
            <div style={{ paddingBottom: 18 }}>
              <div
                style={{
                  fontFamily: "var(--font-mono)", fontSize: "var(--text-sm)",
                  letterSpacing: "0.07em", fontWeight: 600,
                  color: reached ? "var(--text)" : "var(--text-3)"
                }}
              >
                {p.label}
              </div>
              <div style={{ marginTop: 3, fontSize: "var(--text-sm)", color: "var(--text-2)" }}>
                {p.detail}
              </div>
            </div>
            <div
              style={{
                fontFamily: "var(--font-mono)", fontSize: "var(--text-sm)",
                color: "var(--text-3)", paddingTop: 1
              }}
            >
              {p.at ? clock(p.at) : "\u2014"}
            </div>
          </li>
        );
      })}
    </ol>
  );
}
