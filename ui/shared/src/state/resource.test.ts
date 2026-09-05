/**
 * The two failure modes resource.ts exists to prevent, driven directly.
 *
 * Both are invisible to types and to a casual read. The stale-response
 * cases resolve their promises deliberately out of order, which is the
 * only way to distinguish "the last response wins" from "the last call
 * wins", and the identity cases assert on object identity rather than on
 * equality, because equality passes either way and identity is what the
 * polling interval upstream actually depends on.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { graph, resetGraphForTests } from "./graph";
import { createResourceNode, fetchResource, useResource } from "./resource";

interface Deferred<T> {
  promise: Promise<T>;
  resolve(value: T): void;
  reject(error: unknown): void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** Proves the mandatory-review item-1 fix: fetchResource must drop a
 *  response that is not the latest one issued for its node, so two
 *  overlapping fetches resolving out of order can never let the OLDER
 *  response win. */
describe("fetchResource stale-response guard", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("keeps the newest resolved value when an older fetch resolves later", async () => {
    const node = createResourceNode<string>("test.resource.stale-race-1");
    const first = deferred<string>();
    const second = deferred<string>();

    fetchResource(node, () => first.promise);
    fetchResource(node, () => second.promise);

    // Resolve the SECOND (latest) call first, then the first (stale) one —
    // the classic "poll tick overlapping a manual reload" ordering.
    second.resolve("second");
    await second.promise;
    // Let the .then commit land before the stale one resolves.
    await Promise.resolve();
    await Promise.resolve();

    first.resolve("first");
    await first.promise;
    await Promise.resolve();
    await Promise.resolve();

    expect(graph.read(node).data).toBe("second");
  });

  it("does not let a stale rejection overwrite a later success", async () => {
    const node = createResourceNode<string>("test.resource.stale-race-2");
    const first = deferred<string>();
    const second = deferred<string>();

    fetchResource(node, () => first.promise);
    fetchResource(node, () => second.promise);

    second.resolve("current");
    await second.promise;
    await Promise.resolve();
    await Promise.resolve();

    first.reject(new Error("stale failure"));
    await first.promise.catch(() => {});
    await Promise.resolve();
    await Promise.resolve();

    const state = graph.read(node);
    expect(state.data).toBe("current");
    expect(state.error).toBeNull();
    expect(state.loading).toBe(false);
  });
});

/** Proves the mandatory-review item-2 fix: useResource's returned object
 *  (and its .reload) must keep the same reference across re-renders when
 *  nothing on the node actually changed. App.tsx's `reloadAll` is a
 *  useCallback keyed on the three useResource() return values themselves
 *  (health/sets/operations), so if useResource hands back a fresh object
 *  literal on every call, reloadAll's identity churns on every render of
 *  App, which tears down and rebuilds usePolling's setInterval almost
 *  every time instead of letting it run for a full 30s. */
describe("useResource return-value identity", () => {
  afterEach(() => {
    resetGraphForTests();
  });

  it("returns the same object (and the same .reload) across re-renders when the node has not changed", () => {
    const node = createResourceNode<string>("test.resource.memo-1");
    // Never resolves — this test only cares about identity across
    // re-renders, not about the fetch actually completing.
    const fetchFn = vi.fn(() => new Promise<string>(() => {}));

    const { result, rerender } = renderHook(() => useResource(node, fetchFn, []));

    const first = result.current;
    rerender();
    const second = result.current;

    expect(second).toBe(first);
    expect(second.reload).toBe(first.reload);
  });

  it("only returns a new object when the underlying node actually changes", async () => {
    const node = createResourceNode<string>("test.resource.memo-2");
    const first = deferred<string>();
    const fetchFn = vi.fn(() => first.promise);

    const { result, rerender } = renderHook(() => useResource(node, fetchFn, []));
    const beforeResolve = result.current;

    await act(async () => {
      first.resolve("value");
      await first.promise;
      await Promise.resolve();
      await Promise.resolve();
    });
    rerender();

    expect(result.current).not.toBe(beforeResolve);
    expect(result.current.data).toBe("value");

    const afterResolve = result.current;
    rerender();
    expect(result.current).toBe(afterResolve);
  });
});
