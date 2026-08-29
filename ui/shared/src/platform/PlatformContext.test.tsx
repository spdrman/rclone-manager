import { afterEach, describe, expect, it } from "vitest";
import { act, cleanup, render } from "@testing-library/react";
import type { AuthContext, PlatformBridge } from "@shared/types/platform";
import { resetGraphForTests } from "@shared/state/graph";
import { PlatformProvider, usePlatform } from "./PlatformContext";

interface Deferred<T> {
  promise: Promise<T>;
  resolve(value: T): void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

function testBridge(getAuthContext: () => Promise<AuthContext>): PlatformBridge {
  return {
    id: "generic",
    name: "Test Platform",
    integration: "standalone",
    deployment: { label: "Test harness", storageMount: "/data", adapterVersion: "test 0.0.0" },
    capabilities: () => ({
      nativeAuth: false,
      nativeNotifications: false,
      storagePicker: false,
      embeddedWindow: false,
      appStorePackaging: false
    }),
    getAuthContext
  };
}

// Flushes both the microtask queue (the .then/.catch/.finally chain
// refetchAuth builds) and, via the graph commit each leg makes, any
// pending useSyncExternalStore re-render.
function flush() {
  return act(() => new Promise((resolve) => setTimeout(resolve, 0)));
}

describe("PlatformProvider / usePlatform", () => {
  afterEach(() => {
    // Unmount BEFORE resetting the graph: resetting while a component
    // reading bridgeNode is still mounted commits it back to `null` and
    // makes usePlatform() throw mid-render, which is a test-isolation
    // artifact, not the thing under test.
    cleanup();
    resetGraphForTests();
  });

  it("keeps the LAST refreshAuth() response even when an earlier one resolves later (item 1)", async () => {
    const calls: Array<Deferred<AuthContext>> = [];
    const bridge = testBridge(() => {
      const d = deferred<AuthContext>();
      calls.push(d);
      return d.promise;
    });

    let latest: ReturnType<typeof usePlatform> | undefined;
    function Reader() {
      latest = usePlatform();
      return null;
    }

    render(
      <PlatformProvider bridge={bridge}>
        <Reader />
      </PlatformProvider>
    );

    // The mount effect already issued call #1. Issue call #2 before #1
    // has resolved, exactly the "manual reload racing the poll" scenario
    // item 1 describes.
    act(() => {
      latest!.refreshAuth();
    });
    expect(calls).toHaveLength(2);

    // Resolve the NEWER call first, then the STALE one — the
    // out-of-order-resolution shape a naive implementation gets wrong.
    calls[1].resolve({ authenticated: true, username: "second", mode: "local-account" });
    await flush();
    calls[0].resolve({ authenticated: true, username: "first", mode: "local-account" });
    await flush();

    expect(latest!.auth?.username).toBe("second");
  });

  it("does not let a stale rejection clobber a later success (item 1)", async () => {
    const calls: Array<Deferred<AuthContext>> = [];
    const bridge = testBridge(() => {
      const d = deferred<AuthContext>();
      calls.push(d);
      return d.promise;
    });

    let latest: ReturnType<typeof usePlatform> | undefined;
    function Reader() {
      latest = usePlatform();
      return null;
    }

    render(
      <PlatformProvider bridge={bridge}>
        <Reader />
      </PlatformProvider>
    );

    act(() => {
      latest!.refreshAuth();
    });
    expect(calls).toHaveLength(2);

    calls[1].resolve({ authenticated: true, username: "current", mode: "local-account" });
    await flush();
    // The stale call now fails. Attach a rejection handler synchronously
    // so an unhandled-rejection warning does not leak into other tests.
    calls[0].promise.catch(() => {});
    calls[0].resolve = () => {};
    await flush();

    expect(latest!.auth?.username).toBe("current");
    expect(latest!.authLoading).toBe(false);
  });

  it("commits the bridge exactly once across repeated renders with the same bridge reference (item 2)", async () => {
    const bridge = testBridge(() => Promise.resolve({ authenticated: false, username: null, mode: "local-account" }));

    let renderCount = 0;
    function Reader() {
      renderCount += 1;
      const { bridge: seen } = usePlatform();
      return <span>{seen.name}</span>;
    }

    const { rerender } = render(
      <PlatformProvider bridge={bridge}>
        <Reader />
      </PlatformProvider>
    );
    rerender(
      <PlatformProvider bridge={bridge}>
        <Reader />
      </PlatformProvider>
    );
    rerender(
      <PlatformProvider bridge={bridge}>
        <Reader />
      </PlatformProvider>
    );

    // The regression this guards against: comparing against graph.read()
    // in the render body would recommit (and, post-WASM-swap, could even
    // cascade) on every one of these re-renders. It must not throw, and
    // the child must keep seeing the one stable bridge.
    expect(renderCount).toBeGreaterThanOrEqual(3);

    // Let the mount effect's auth fetch settle before the test (and its
    // cleanup) ends, so React does not warn about a post-test update.
    await flush();
  });
});
