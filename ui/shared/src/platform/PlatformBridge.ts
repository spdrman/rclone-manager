import type { PlatformBridge } from "@shared/types/platform";

export type { PlatformBridge };

/** Guard rail: the shared UI must never construct a bridge itself. A provider
 *  shell passes one in at bootstrap. Throwing here surfaces a wiring mistake
 *  immediately instead of silently degrading to generic behaviour. */
export function assertBridge(bridge: PlatformBridge | null): PlatformBridge {
  if (!bridge) {
    throw new Error(
      "No PlatformBridge provided. A provider shell in apps/<id>/frontend must " +
        "call bootstrap() with its bridge."
    );
  }
  return bridge;
}
