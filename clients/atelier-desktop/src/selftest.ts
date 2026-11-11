// A scripted walk through the app, for checking a build with nobody at the
// keyboard: first run against the studio address given in
// ATELIER_DESKTOP_SELFTEST, then every screen. It drives the real window
// (types, clicks, reads what is drawn) and reports through `selftest_log`,
// which prints to the app's standard error. It never runs unless the app
// was started with that variable.

import { api } from "./api";
import type { Shell } from "./shell";

const log = (line: string) => api.selftestLog(line).catch(() => {});

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

async function waitFor<T>(what: string, find: () => T | null | undefined | false, timeout = 15000): Promise<T> {
  const until = Date.now() + timeout;
  for (;;) {
    const found = find();
    if (found) return found;
    if (Date.now() > until) throw new Error(`timed out waiting for ${what}`);
    await sleep(100);
  }
}

const text = (el: Element | null | undefined) => (el as HTMLElement | null)?.innerText?.replace(/\s+/g, " ").trim() ?? "";

function button(label: string, within: ParentNode = document): HTMLButtonElement | null {
  return (
    [...within.querySelectorAll<HTMLButtonElement>("button")].find(
      (b) => b.textContent?.replace(/\s+/g, " ").trim() === label && !b.disabled && b.offsetParent !== null,
    ) ?? null
  );
}

const sees = (words: string) => document.body.innerText.toLowerCase().includes(words.toLowerCase());

/** Tells the harness this is a good moment for a picture, and holds still. */
async function shot(name: string): Promise<void> {
  await log(`shot ${name}`);
  await sleep(1400);
}

async function click(label: string, timeout = 15000): Promise<void> {
  (await waitFor(`the “${label}” button`, () => button(label), timeout)).click();
}

async function screen(shell: Shell, name: string, go: () => void, ready: () => unknown): Promise<void> {
  go();
  try {
    await waitFor(name, ready, 10000);
    await sleep(300);
    await log(`screen ${name}: ${text(document.querySelector(".stage")).slice(0, 420)}`);
    await shot(name);
  } catch (e) {
    await log(`FAIL screen ${name}: ${String(e)} :: ${text(document.querySelector(".stage")).slice(0, 300)}`);
  }
  void shell;
}

export async function runSelfTest(shell: Shell, address: string): Promise<void> {
  // Every pop-up is worth a line: errors show up as red ones.
  new MutationObserver(() => {
    for (const toast of document.querySelectorAll<HTMLElement>(".toast:not([data-logged])")) {
      toast.dataset.logged = "1";
      void log(`popup: ${text(toast)} [${toast.className}]`);
    }
  }).observe(document.body, { childList: true, subtree: true });
  window.addEventListener("error", (e) => void log(`FAIL script error: ${e.message}`));
  window.addEventListener("unhandledrejection", (e) => void log(`FAIL unhandled: ${String(e.reason)}`));
  try {
    const state = await api.appState();
    await log(`start signed_in=${state.signed_in} os=${state.os} fonts=${[...document.fonts].length}`);

    if (!state.signed_in) {
      // First run, step by step, as a person would.
      const input = await waitFor("the address field", () => document.querySelector<HTMLInputElement>("input"));
      await log(`welcome: ${text(document.querySelector("#app")).slice(0, 260)}`);
      await shot("welcome-1");
      input.value = address;
      input.dispatchEvent(new Event("input", { bubbles: true }));
      await click("Connect");
      await waitFor("the code", () => sees("AMBER-"));
      await log(`code shown: ${/[A-Z]+-\d{4}/.exec(document.body.innerText)?.[0] ?? "?"}`);
      await waitFor("approval", () => sees("Signed in as"), 30000);
      await log(`approved: ${text(document.querySelector("main")).slice(0, 320)}`);
      await shot("welcome-2");
      await click("Continue");
      await waitFor("the folders", () => sees("Matched") || sees("No git folders"), 30000);
      await log(`folders: ${text(document.querySelector("main")).slice(0, 420)}`);
      await shot("welcome-3");
      await click("Continue");
      await waitFor("the runner step", () => button("Finish"));
      await sleep(600);
      await log(`runner: ${text(document.querySelector("main")).slice(0, 360)}`);
      await shot("welcome-4");
      await click("Finish");
    }

    await waitFor("the sidebar", () => document.querySelector(".sidebar"));
    await waitFor("Today", () => sees("Needs you") && shell.ctx.overview() !== null, 30000);
    await sleep(500);
    await log(`sidebar: ${text(document.querySelector(".sidebar"))}`);
    await log(`today: ${text(document.querySelector(".stage")).slice(0, 900)}`);
    const style = getComputedStyle(document.body);
    const head = document.querySelector(".h-screen");
    await log(
      `look: ground=${style.backgroundColor} font=${style.fontFamily.split(",")[0]} heading=${head ? getComputedStyle(head).fontFamily.split(",")[0] : "?"} inter=${document.fonts.check('13px "Inter"')} mono=${document.fonts.check('12px "JetBrains Mono"')}`,
    );

    // The harness posts a notify event to the mock studio when it sees this.
    await log("ready-for-notify");
    try {
      const toast = await waitFor("a pop-up", () => document.querySelector(".toast"), 20000);
      await log(`toast: ${text(toast)} [${toast.className}]`);
      await shot("today");
    } catch (e) {
      await log(`FAIL toast: ${String(e)}`);
    }

    const overview = shell.ctx.overview();
    const copy = overview?.copies[0];
    await screen(shell, "pulls", () => shell.go({ screen: "pulls" }), () => sees("open across your studio") && sees("Conversation"));
    if (copy) {
      await screen(shell, "changes", () => shell.go({ screen: "repo", path: copy.path, tab: "changes" }), () => sees("Changed files") || sees("Nothing to commit"));
      const file = document.querySelector<HTMLElement>(".stage [aria-label^='Include']");
      await log(`changes: first file checkbox ${file ? "present" : "absent"}; diff rows ${document.querySelectorAll(".stage [class*='diff'] > *").length}`);
      await screen(shell, "history", () => button("History")?.click(), () => document.querySelector(".stage svg circle"));
      await screen(shell, "branches", () => [...document.querySelectorAll<HTMLButtonElement>(".stage button")].find((b) => b.textContent?.startsWith("Branches"))?.click(), () => sees("New branch"));
    } else {
      await log("FAIL no working copy to open");
    }
    await screen(shell, "device", () => shell.go({ screen: "device" }), () => sees("When to work"));
    await screen(shell, "settings", () => shell.go({ screen: "settings" }), () => sees("Who you commit as") && sees("Remote access"));
    await screen(shell, "messages", () => shell.go({ screen: "messages" }), () => sees("After a push") && sees("Try it"));
    await screen(shell, "add", () => shell.go({ screen: "add", tab: "clone" }), () => sees("Add a repository") && sees("In your studio"));
    shell.go({ screen: "today" });
    // The menu bar popover, shown without a click on its icon.
    await api.selftestPopover().catch(() => {});
    await sleep(1500);
    await shot("tray");
    await log("done");
  } catch (e) {
    await log(`FAIL ${String(e)} :: ${document.body.innerText.replace(/\s+/g, " ").slice(0, 400)}`);
  }
}
