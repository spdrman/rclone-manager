import { useEffect, useState } from "react";
import UGOSCore from "@ugreen-nas/core";
import CloudWindow from "@ugreen-nas/core/cloudWindow";

// The real JSSDK's actual shape (read from @ugreen-nas/core's own shipped .d.ts files,
// not guessed — see apps/ugos/docs/upk-proof-procedure.md "What 'session context' means
// for the real JSSDK"):
//
//   - UGOSCore.init(): Promise<string> performs the host handshake and resolves with the
//     active locale.
//   - UGOSCore.isHost is true only when this page is actually running inside a real UGOS
//     host frame — the honest signal this proof is actually looking for.
//   - the default export of '@ugreen-nas/core/cloudWindow' is already the page's singleton
//     CloudWindow instance (not the class), so its getSizeInfo() is a second, independent
//     live round trip to the host (real window geometry only the host can answer), used
//     here purely as a second confirmation that the channel is live, not a fake/cached
//     resolution.
//   - window.ugosAppId / window.ugosAppVersion are set by the host once it recognizes the
//     package (they mirror project.yaml's app_id/version).
//
// getThirdToken (the actual per-request auth exchange) is Work Package 1.3 (#92) and is
// deliberately not called here — this proof only needs the JSSDK channel to be alive.

type SessionContext = {
  isHost: boolean;
  locale: string;
  appId: string | null;
  appVersion: string | null;
  sizeInfo: unknown;
};

type BootState =
  | { phase: "booting" }
  | { phase: "ready"; session: SessionContext }
  | { phase: "error"; message: string };

type HealthState = { phase: "checking" } | { phase: "ok"; body: string } | { phase: "failed"; message: string };

function withTimeout<T>(promise: Promise<T>, ms: number, label: string): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const timer = setTimeout(() => {
      reject(new Error(`${label} did not respond within ${ms}ms (no real UGOS host answered)`));
    }, ms);
    promise.then(
      (value) => {
        clearTimeout(timer);
        resolve(value);
      },
      (err) => {
        clearTimeout(timer);
        reject(err);
      }
    );
  });
}

function readHostGlobal(name: "ugosAppId" | "ugosAppVersion"): string | null {
  const value = (window as unknown as Record<string, unknown>)[name];
  return typeof value === "string" ? value : null;
}

export default function App() {
  const [boot, setBoot] = useState<BootState>({ phase: "booting" });
  const [health, setHealth] = useState<HealthState>({ phase: "checking" });

  useEffect(() => {
    let cancelled = false;

    (async () => {
      try {
        // The actual bootstrap call: wires up the CloudWindow channel and performs the
        // ready() handshake with the host.
        const locale = await withTimeout(UGOSCore.init(), 5000, "UGOSCore.init()");

        // A second, independent host round trip, best-effort: not calling this at all
        // wouldn't disprove the session context above, so a failure here degrades the
        // page (shown as "unavailable") rather than the whole bootstrap.
        let sizeInfo: unknown = "unavailable";
        try {
          sizeInfo = await withTimeout(CloudWindow.getSizeInfo(), 5000, "CloudWindow.getSizeInfo()");
        } catch (err) {
          sizeInfo = `unavailable: ${err instanceof Error ? err.message : String(err)}`;
        }

        if (cancelled) return;
        setBoot({
          phase: "ready",
          session: {
            isHost: UGOSCore.isHost,
            locale,
            appId: readHostGlobal("ugosAppId"),
            appVersion: readHostGlobal("ugosAppVersion"),
            sizeInfo
          }
        });
      } catch (err) {
        if (cancelled) return;
        setBoot({ phase: "error", message: err instanceof Error ? err.message : String(err) });
      }
    })();

    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;

    fetch("/health/live")
      .then(async (res) => {
        const body = await res.text();
        if (cancelled) return;
        if (res.ok) {
          setHealth({ phase: "ok", body });
        } else {
          setHealth({ phase: "failed", message: `HTTP ${res.status}: ${body}` });
        }
      })
      .catch((err) => {
        if (!cancelled) setHealth({ phase: "failed", message: err instanceof Error ? err.message : String(err) });
      });

    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <main style={{ fontFamily: "system-ui, sans-serif", padding: "2rem", maxWidth: 720 }}>
      <h1>Backup Manager — UPK proof</h1>
      <p>Minimal hardware proof for issue #91 (EPIC B, Work Package 1.2).</p>

      <section aria-label="JSSDK bootstrap">
        <h2>React/JSSDK bootstrap</h2>
        {boot.phase === "booting" && <p>booting…</p>}
        {boot.phase === "error" && (
          <p style={{ color: "crimson" }}>
            not hosted by a real UGOS desktop: {boot.message}
          </p>
        )}
        {boot.phase === "ready" && (
          <dl>
            <dt>isHost</dt>
            <dd>{String(boot.session.isHost)}</dd>
            <dt>locale (from UGOSCore.init())</dt>
            <dd>{boot.session.locale}</dd>
            <dt>window.ugosAppId</dt>
            <dd>{boot.session.appId ?? "(none)"}</dd>
            <dt>window.ugosAppVersion</dt>
            <dd>{boot.session.appVersion ?? "(none)"}</dd>
            <dt>CloudWindow.getSizeInfo()</dt>
            <dd>
              <pre>{JSON.stringify(boot.session.sizeInfo, null, 2)}</pre>
            </dd>
          </dl>
        )}
      </section>

      <section aria-label="health check">
        <h2>/health/live</h2>
        {health.phase === "checking" && <p>checking…</p>}
        {health.phase === "ok" && <p style={{ color: "green" }}>reachable: {health.body}</p>}
        {health.phase === "failed" && <p style={{ color: "crimson" }}>unreachable: {health.message}</p>}
      </section>
    </main>
  );
}
