/**
 * The composition root: where the provider tree is assembled and every
 * app-wide choice is made once.
 *
 * The nesting order below is load-bearing rather than incidental. The
 * error boundary is outermost so it can still render when the thing that
 * threw is a provider itself, the platform provider comes before the API
 * one because auth is what decides whether anything under it should
 * render at all, and the router is innermost because it is the only part
 * that pages read on every navigation.
 *
 * The API default is the one place the mock is chosen, keyed on the dev
 * build. A provider shell that wants something else passes it, which is
 * how the browser suite drives real screens against fixtures without this
 * file knowing that is happening.
 */
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { App } from "@shared/App";
import { ApiProvider } from "@shared/api/ApiContext";
import { PlatformProvider } from "@shared/platform/PlatformContext";
import { httpApi } from "@shared/api/client";
import { createMockApi, scenarioFromLocation } from "@shared/api/mock";
import type { BackupManagerApi } from "@shared/api/contracts";
import type { PlatformBridge } from "@shared/types/platform";
import { ErrorBoundary } from "./ErrorBoundary";

/** The single mount path every provider shell calls. A provider supplies its
 *  bridge and, optionally, its own API instance. Nothing provider-specific
 *  lives below this line. */
export function createApp(
  container: HTMLElement,
  bridge: PlatformBridge,
  api: BackupManagerApi = import.meta.env.DEV ? createMockApi(scenarioFromLocation()) : httpApi
) {
  createRoot(container).render(
    <StrictMode>
      <ErrorBoundary>
        <PlatformProvider bridge={bridge}>
          <ApiProvider api={api}>
            <BrowserRouter>
              <App />
            </BrowserRouter>
          </ApiProvider>
        </PlatformProvider>
      </ErrorBoundary>
    </StrictMode>
  );
}
