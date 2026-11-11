import { describe, expect, it } from "vitest";
import type { Toast } from "../api";
import { LINGER_MS, LINGER_WITH_ACTION_MS, ToastQueue, type Clock } from "./toasts";

/** A clock the test moves by hand. */
class FakeClock implements Clock {
  now = 0;
  private next = 1;
  private timers = new Map<number, { at: number; fn: () => void }>();

  set(fn: () => void, ms: number): unknown {
    const id = this.next++;
    this.timers.set(id, { at: this.now + ms, fn });
    return id;
  }

  clear(handle: unknown): void {
    this.timers.delete(handle as number);
  }

  advance(ms: number): void {
    this.now += ms;
    for (const [id, timer] of [...this.timers].sort((a, b) => a[1].at - b[1].at)) {
      if (timer.at <= this.now && this.timers.delete(id)) timer.fn();
    }
  }
}

const toast = (id: string, extra: Partial<Toast> = {}): Toast => ({
  id,
  text: `text ${id}`,
  sub: "",
  tone: "yours",
  moment: null,
  action: null,
  target: { screen: "today" },
  ...extra,
});

function setup() {
  const clock = new FakeClock();
  const seen: string[][] = [];
  const queue = new ToastQueue((visible) => seen.push(visible.map((t) => t.id)), clock);
  return { clock, seen, queue };
}

describe("toast queue", () => {
  it("shows up to three and queues the rest", () => {
    const { queue, seen } = setup();
    for (const id of ["a", "b", "c", "d", "e"]) queue.push(toast(id));
    expect(queue.shown.map((t) => t.id)).toEqual(["a", "b", "c"]);
    expect(queue.queued).toBe(2);
    expect(seen.at(-1)).toEqual(["a", "b", "c"]);
  });

  it("lets each one go after its time and brings the next in", () => {
    const { queue, clock } = setup();
    for (const id of ["a", "b", "c", "d"]) queue.push(toast(id));
    clock.advance(LINGER_MS);
    // a, b and c were armed together; d took the first free place.
    expect(queue.shown.map((t) => t.id)).toEqual(["d"]);
    expect(queue.queued).toBe(0);
    clock.advance(LINGER_MS);
    expect(queue.shown).toEqual([]);
  });

  it("keeps pop-ups with a button for longer", () => {
    const { queue, clock } = setup();
    queue.push(toast("plain"));
    queue.push(toast("ask", { action: { label: "Review", target: { screen: "device" } } }));
    clock.advance(LINGER_MS);
    expect(queue.shown.map((t) => t.id)).toEqual(["ask"]);
    clock.advance(LINGER_WITH_ACTION_MS - LINGER_MS);
    expect(queue.shown).toEqual([]);
  });

  it("replaces a pop-up with the same id instead of stacking it", () => {
    const { queue, clock } = setup();
    queue.push(toast("n:1", { text: "first" }));
    clock.advance(LINGER_MS - 1000);
    queue.push(toast("n:1", { text: "second" }));
    expect(queue.shown).toHaveLength(1);
    expect(queue.shown[0]?.text).toBe("second");
    // Its time starts again.
    clock.advance(1500);
    expect(queue.shown).toHaveLength(1);
    clock.advance(LINGER_MS);
    expect(queue.shown).toEqual([]);
  });

  it("does not take one away while the pointer rests on it", () => {
    const { queue, clock } = setup();
    queue.push(toast("a"));
    queue.hold("a");
    clock.advance(LINGER_MS * 3);
    expect(queue.shown.map((t) => t.id)).toEqual(["a"]);
    queue.release("a");
    clock.advance(LINGER_MS - 1);
    expect(queue.shown).toHaveLength(1);
    clock.advance(1);
    expect(queue.shown).toEqual([]);
  });

  it("dismisses by hand, from the screen or from the queue", () => {
    const { queue, seen } = setup();
    for (const id of ["a", "b", "c", "d", "e"]) queue.push(toast(id));
    queue.dismiss("e"); // still waiting: nothing on screen changes
    expect(queue.queued).toBe(1);
    const changes = seen.length;
    queue.dismiss("b");
    expect(queue.shown.map((t) => t.id)).toEqual(["a", "c", "d"]);
    expect(seen.length).toBe(changes + 1);
    queue.dismiss("nope");
    expect(seen.length).toBe(changes + 1);
  });

  it("clears everything", () => {
    const { queue, clock } = setup();
    for (const id of ["a", "b", "c", "d"]) queue.push(toast(id));
    queue.clear();
    expect(queue.shown).toEqual([]);
    expect(queue.queued).toBe(0);
    clock.advance(LINGER_MS * 2);
    expect(queue.shown).toEqual([]);
  });
});
