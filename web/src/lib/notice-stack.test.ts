import { describe, expect, it } from "vitest";

import { NOTICE_CEILING, orderNotices, type StackedNotice } from "./notice-stack";

/* ------------------------------------------------------------------ *
 * The notice ceiling
 *
 * Seven conditions on the app page can produce a banner and four of
 * them can be true at once, each `size="md"` — which put the tab strip
 * off the first screen. The rule (ADR §53 D6) is: a notice about the
 * app's state lives above the tabs, at most two show, and the rest
 * collapse to one line.
 *
 * Both halves are worth a test. The ceiling is arithmetic. The order is
 * the half that could be got subtly wrong: a *stable* sort is what
 * keeps the deliberate order within each half — dependency notices
 * last, because neither means the app stopped working — and JavaScript
 * only guarantees stability if the comparator returns 0 for equals,
 * which a `serving ? 1 : -1` would not.
 * ------------------------------------------------------------------ */

const n = (key: string, serving: boolean): StackedNotice => ({ key, serving, node: key });

describe("the notice stack", () => {
  it("says nothing when nothing fired", () => {
    expect(orderNotices([]).shown).toEqual([]);
    expect(orderNotices([false, undefined, null]).shown).toEqual([]);
    expect(orderNotices([false]).hidden).toBe(0);
  });

  it("drops the conditions that did not fire, keeping the ones that did", () => {
    const { shown, hidden } = orderNotices([false, n("a", true), undefined]);
    expect(shown.map((s) => s.key)).toEqual(["a"]);
    expect(hidden).toBe(0);
  });

  it("leads with the notices that mean the app is not doing its job", () => {
    const { shown } = orderNotices([n("source-blocked", true), n("crashed", false)], true);
    expect(shown.map((s) => s.key)).toEqual(["crashed", "source-blocked"]);
  });

  it("keeps the declared order within each half", () => {
    // The dependency pair is declared last and must stay last among the
    // still-serving notices, whatever else is on screen with it.
    const { shown } = orderNotices(
      [
        n("source-blocked", true),
        n("deploying", false),
        n("crashed", false),
        n("uploaded", true),
        n("malware", true),
      ],
      true,
    );
    expect(shown.map((s) => s.key)).toEqual([
      "deploying",
      "crashed",
      "source-blocked",
      "uploaded",
      "malware",
    ]);
  });

  it("shows two and counts the rest", () => {
    const { shown, hidden } = orderNotices([
      n("a", true),
      n("b", true),
      n("c", true),
      n("d", true),
    ]);
    expect(shown).toHaveLength(NOTICE_CEILING);
    expect(hidden).toBe(2);
  });

  it("hides nothing once opened", () => {
    const all = [n("a", true), n("b", true), n("c", true)];
    expect(orderNotices(all, true).shown).toHaveLength(3);
    expect(orderNotices(all, true).hidden).toBe(0);
  });

  it("never collapses a notice that means the app is down behind one that does not", () => {
    // The failure mode worth naming: four still-serving notices and one crash
    // must not push the crash under "4 more notices about this app".
    const { shown } = orderNotices([
      n("source-blocked", true),
      n("uploaded", true),
      n("malware", true),
      n("blocked", true),
      n("crashed", false),
    ]);
    expect(shown[0].key).toBe("crashed");
  });
});

describe("the ceiling counts events, not notices", () => {
  const n = (key: string, serving: boolean, about?: string) => ({
    key,
    serving,
    node: null,
    ...(about ? { about } : {}),
  });

  it("lets two notices about one event share a slot", () => {
    // A failed deploy raises the console and the banner naming the
    // consequence. Both are `serving: false`, so they sorted to the front and
    // took the whole ceiling between them — collapsing the notice that
    // carries the way out.
    const { shown, hidden } = orderNotices([
      n("deploying", false, "deploy"),
      n("deploy-failed", false, "deploy"),
      n("source-blocked", true),
    ]);
    expect(shown.map((s) => s.key)).toEqual([
      "deploying",
      "deploy-failed",
      "source-blocked",
    ]);
    expect(hidden).toBe(0);
  });

  it("still stops at two events", () => {
    const { shown, hidden } = orderNotices([
      n("deploying", false, "deploy"),
      n("deploy-failed", false, "deploy"),
      n("crashed", false),
      n("source-blocked", true),
      n("unapplied-env", true),
    ]);
    expect(shown.map((s) => s.key)).toEqual(["deploying", "deploy-failed", "crashed"]);
    expect(hidden).toBe(2);
  });

  it("counts a notice with no event as its own", () => {
    const { shown, hidden } = orderNotices([
      n("locked", false),
      n("crashed", false),
      n("unapplied-env", true),
    ]);
    expect(shown.map((s) => s.key)).toEqual(["locked", "crashed"]);
    expect(hidden).toBe(1);
  });

  it("shows everything when expanded, and hides nothing", () => {
    const { shown, hidden } = orderNotices(
      [n("deploying", false, "deploy"), n("deploy-failed", false, "deploy"), n("a", true), n("b", true)],
      true,
    );
    expect(shown).toHaveLength(4);
    expect(hidden).toBe(0);
  });
});
