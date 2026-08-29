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
