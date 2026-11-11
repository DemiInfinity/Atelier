// What every screen is handed: the app's state, navigation, pop-ups and
// dialogs. Screens never reach for globals.

import type { AppState, Events, NeedAction, Overview, Target, Toast, Tone } from "./api";
import type { Route } from "./routes";

export interface ConfirmOptions {
  title: string;
  /** One or two plain sentences. */
  body: string;
  /** The confirming button's words (`Discard changes`). */
  confirm: string;
  /** Red confirming button, for things that can't be undone. */
  danger?: boolean;
}

export interface PromptOptions {
  title: string;
  label: string;
  value?: string;
  placeholder?: string;
  confirm: string;
  mono?: boolean;
}

export interface Ctx {
  /** The app's state as last read (studio, accounts, editors, preferences). */
  app(): AppState;
  /** Reads the app's state again (after changing a setting, say). */
  reloadApp(): Promise<AppState>;
  /** After `api.disconnect()`: forget everything and show first run again. */
  signedOut(): Promise<void>;
  /** The overview as last read: needs, working copies, runner. May be null early on. */
  overview(): Overview | null;
  /** Reads the overview again; the sidebar and `onOverview` follow. */
  refresh(): Promise<void>;

  /** Goes to a screen. */
  go(route: Route): void;
  /** Follows a click target: a screen, or a page of the studio in the browser. */
  follow(target: Target): void;

  /** Shows a pop-up. */
  toast(toast: Toast): void;
  /** Shows a short pop-up of the app's own (`Fetched.`). */
  say(text: string, sub?: string, tone?: Tone): void;
  /** Shows what went wrong, from a rejected command. */
  fail(error: unknown): void;

  /** Asks before something destructive or far-reaching. Resolves true on confirm. */
  confirm(options: ConfirmOptions): Promise<boolean>;
  /** Asks for one line of text. Resolves null when cancelled. */
  prompt(options: PromptOptions): Promise<string | null>;
  /** Shows a failed run's log in a dialog. */
  showLog(repo: string, runId: number, webPath?: string | null): void;
  /** Does what a "needs you" (or check) button says. */
  runNeed(action: NeedAction): Promise<void>;

  /**
   * Runs `work` with `button` disabled and marked busy; shows the error and
   * resolves undefined if it fails.
   */
  busy<T>(button: HTMLButtonElement | null, work: () => Promise<T>): Promise<T | undefined>;

  /** Opens a small menu under `anchor`. Resolves when it closes. */
  menu(anchor: HTMLElement, items: MenuItem[]): void;
}

export type MenuItem =
  | { label: string; hint?: string; current?: boolean; mono?: boolean; pick: () => void }
  | { separator: true }
  | { note: string };

/** A screen of the main window. */
export interface Screen {
  el: HTMLElement;
  /** Called when the screen is replaced: stop timers, drop listeners. */
  dispose?(): void;
  /** A fresh overview arrived (after `ctx.refresh()` or a studio event). */
  onOverview?(overview: Overview): void;
  /** An event from the Rust side (`status`, `runner`, `remote`, …). */
  onEvent?<K extends keyof Events>(name: K, payload: Events[K]): void;
}
