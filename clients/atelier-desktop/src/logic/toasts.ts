// The pop-up queue: at most three on screen, newest at the bottom, each
// leaving by itself after a while unless the pointer is resting on it.

import type { Toast } from "../api";

export interface Clock {
  set(fn: () => void, ms: number): unknown;
  clear(handle: unknown): void;
}

const realClock: Clock = {
  set: (fn, ms) => setTimeout(fn, ms),
  clear: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
};

export const MAX_VISIBLE = 3;
export const LINGER_MS = 6000;
/** Pop-ups with a button stay longer: there is something to decide. */
export const LINGER_WITH_ACTION_MS = 12000;

export class ToastQueue {
  private visible: Toast[] = [];
  private waiting: Toast[] = [];
  private timers = new Map<string, unknown>();
  private held = new Set<string>();

  constructor(
    private readonly onChange: (visible: readonly Toast[]) => void,
    private readonly clock: Clock = realClock,
  ) {}

  get shown(): readonly Toast[] {
    return this.visible;
  }

  get queued(): number {
    return this.waiting.length;
  }

  /** Adds a pop-up. One with an id already on screen or waiting replaces it. */
  push(toast: Toast): void {
    const at = this.visible.findIndex((t) => t.id === toast.id);
    if (at >= 0) {
      this.visible[at] = toast;
      this.arm(toast);
      this.changed();
      return;
    }
    const queuedAt = this.waiting.findIndex((t) => t.id === toast.id);
    if (queuedAt >= 0) {
      this.waiting[queuedAt] = toast;
      return;
    }
    if (this.visible.length < MAX_VISIBLE) {
      this.show(toast);
    } else {
      this.waiting.push(toast);
    }
  }

  /** Takes a pop-up away (clicked, "Later", or its time ran out). */
  dismiss(id: string): void {
    const before = this.visible.length;
    this.visible = this.visible.filter((t) => t.id !== id);
    this.waiting = this.waiting.filter((t) => t.id !== id);
    this.disarm(id);
    this.held.delete(id);
    if (this.visible.length === before) return;
    const next = this.waiting.shift();
    if (next) this.show(next);
    else this.changed();
  }

  /** The pointer is on it: don't take it away. */
  hold(id: string): void {
    if (!this.visible.some((t) => t.id === id)) return;
    this.held.add(id);
    this.disarm(id);
  }

  /** The pointer left: start its time again. */
  release(id: string): void {
    if (!this.held.delete(id)) return;
    const toast = this.visible.find((t) => t.id === id);
    if (toast) this.arm(toast);
  }

  clear(): void {
    for (const id of [...this.timers.keys()]) this.disarm(id);
    this.visible = [];
    this.waiting = [];
    this.held.clear();
    this.changed();
  }

  private show(toast: Toast): void {
    this.visible.push(toast);
    this.arm(toast);
    this.changed();
  }

  private arm(toast: Toast): void {
    this.disarm(toast.id);
    if (this.held.has(toast.id)) return;
    const ms = toast.action ? LINGER_WITH_ACTION_MS : LINGER_MS;
    this.timers.set(
      toast.id,
      this.clock.set(() => this.dismiss(toast.id), ms),
    );
  }

  private disarm(id: string): void {
    const handle = this.timers.get(id);
    if (handle !== undefined) {
      this.clock.clear(handle);
      this.timers.delete(id);
    }
  }

  private changed(): void {
    this.onChange(this.visible);
  }
}
