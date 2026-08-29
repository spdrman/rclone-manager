import { afterEach, describe, expect, it } from "vitest";
import { graph, resetGraphForTests } from "./graph";
import { createResourceNode, fetchResource } from "./resource";

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
