import { Component } from "react";
import type { ErrorInfo, ReactNode } from "react";

interface Props {
  children: ReactNode;
}

interface State {
  error: Error | null;
}

/**
 * Boundary of last resort for the whole mount tree (see createApp.tsx).
 * Without this, a component throwing during render — usePlatform() called
 * outside its provider, a misused useCausl(node) call throwing via
 * useSyncExternalStore, or any other render-time exception in a page this
 * shared shell has never seen — reaches React with nothing catching it,
 * unmounts the whole tree, and leaves a blank white screen with no
 * operator-facing message. That is the worst possible failure mode for a
 * product whose whole design point is surfacing failure states legibly
 * (§37) rather than as errors to dismiss, and this is the one shared boot
 * path every future provider shell calls into unchanged, so it is worth
 * fixing once, here.
 *
 * Deliberately a class component: `componentDidCatch` /
 * `getDerivedStateFromError` have no hook equivalent in React — this is
 * the one place in ui/shared a class component is the correct tool, not
 * an oversight.
 */
export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    // No telemetry backend exists yet (core/internal/obs is "structured
    // event logging, not yet called by anything" per the README), so
    // console.error is the honest floor: at least visible in the browser
    // console during development and in a bundled support log, rather
    // than silently swallowed.
    console.error("Backup Manager UI crashed:", error, info.componentStack);
  }

  render() {
    if (this.state.error) {
      return (
        <div style={{ minHeight: "100vh", display: "grid", placeItems: "center", padding: 24 }}>
          <div
            className="banner banner--danger"
            role="alert"
            style={{ maxWidth: "56ch", flexDirection: "column" }}
          >
            <div style={{ fontWeight: 600, fontSize: 15 }}>Backup Manager hit an unexpected error</div>
            <div style={{ marginTop: 6, fontSize: 13, color: "var(--text-2)" }}>
              Reloading the page is the safest next step. No backup or deletion action runs from
              this screen, so nothing here is a data-loss risk — this only stops the page from
              continuing to display something it can no longer render correctly.
            </div>
            <button
              className="btn btn--sm"
              style={{ marginTop: 10, alignSelf: "flex-start" }}
              onClick={() => window.location.reload()}
            >
              Reload
            </button>
          </div>
        </div>
      );
    }

    return this.props.children;
  }
}
