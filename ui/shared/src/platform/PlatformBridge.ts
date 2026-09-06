/**
 * The bridge type, re-exported, and the assertion that the shared UI never
 * invents one.
 *
 * The re-export is what makes the import path read the same as every other
 * platform import here, so a component asks `@shared/platform/PlatformBridge`
 * rather than reaching into `@shared/types`. The assertion is the reason
 * the file has code in it at all: a missing bridge means a provider shell
 * did not call bootstrap, and failing loudly at that point is far cheaper
 * than degrading to generic behaviour that looks plausible on every screen
 * and is wrong on the platform-specific ones.
 */
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
