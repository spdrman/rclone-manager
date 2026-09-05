/**
 * Whether the React binding keeps causl's promises: a commit re-renders a
 * reader with no props, two readers of one node never disagree within a
 * commit, and a render carrying no commit reads the same reference back.
 *
 * All three are properties of the hook, not of any screen, which is why
 * every case builds its own throwaway graph instead of the app singleton.
 * The third is the one that pays for the caching in useCausl.ts, and it
 * uses an object-valued node on purpose: with a number it would pass
 * whether the cache existed or not.
 */
import { describe, expect, it } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { createCausl } from "@causlts/core";
import { createCauslHook } from "./useCausl";

/** These are graph-mechanics tests: each one builds its own throwaway
 *  `createCausl()` graph (and its own bound hook, via `createCauslHook`)
 *  rather than the app's shared singleton (`@shared/state/graph`), so they
 *  say nothing about `PlatformContext` specifically and everything about
 *  whether the hook itself upholds the causl contract (§3 Theorem 1
 *  determinism, §15.1 read-identity). */
describe("useCausl", () => {
  it("re-renders a component reading a node on a commit to that node, with no parent passing new props", () => {
    const graph = createCausl();
    const useCausl = createCauslHook(graph);
    const nameNode = graph.input("test.name", "before");

    // Reader takes no props at all: the only way its rendered output can
    // change is the graph, never a parent re-rendering it with something new.
    function Reader() {
      const name = useCausl(nameNode);
      return <span data-testid="name">{name}</span>;
    }

    function Parent() {
      return <Reader />;
    }

    render(<Parent />);
    expect(screen.getByTestId("name").textContent).toBe("before");

    act(() => {
      graph.commit("rename", (tx) => tx.set(nameNode, "after"));
    });

    expect(screen.getByTestId("name").textContent).toBe("after");
  });

  it("never lets two independent consumers of the same node observe different values at the same commit", () => {
    const graph = createCausl();
    const useCausl = createCauslHook(graph);
    const countNode = graph.input("test.count", 0);

    function ConsumerA() {
      return <span data-testid="a">{useCausl(countNode)}</span>;
    }
    function ConsumerB() {
      return <span data-testid="b">{useCausl(countNode)}</span>;
    }

    render(
      <>
        <ConsumerA />
        <ConsumerB />
      </>
    );

    expect(screen.getByTestId("a").textContent).toBe(screen.getByTestId("b").textContent);

    act(() => {
      graph.commit("bump", (tx) => tx.set(countNode, 1));
    });

    // Glitch-freedom (§3 Theorem 2): after ONE commit, both consumers must
    // already agree — never one updated and the other still on the stale
    // value, even transiently.
    expect(screen.getByTestId("a").textContent).toBe("1");
    expect(screen.getByTestId("b").textContent).toBe("1");
  });

  it("returns a stable value across renders that carry no commit, even for an object-valued node", () => {
    // The read()-identity gotcha: graph.read() is not contractually
    // guaranteed to return the same reference twice, so a naive
    // `useSyncExternalStore(subscribe, () => graph.read(node))` can appear
    // to change on every render and spin. useCausl must not do that.
    const graph = createCausl();
    const useCausl = createCauslHook(graph);
    const objectNode = graph.input("test.object", { n: 1 });

    const seen: number[] = [];
    function Reader() {
      const value = useCausl(objectNode);
      seen.push(value.n);
      return <span data-testid="n">{value.n}</span>;
    }

    const { rerender } = render(<Reader />);
    rerender(<Reader />);
    rerender(<Reader />);

    expect(screen.getByTestId("n").textContent).toBe("1");
    expect(new Set(seen).size).toBe(1);
  });
});
