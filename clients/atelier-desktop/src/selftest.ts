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

/** One part of the walk; a failure is logged and the walk goes on. */
async function step(name: string, work: () => Promise<void>): Promise<void> {
  try {
    await work();
  } catch (e) {
    await log(`FAIL step ${name}: ${String(e)} :: ${text(document.querySelector(".dialog") ?? document.querySelector(".stage")).slice(0, 300)}`);
    // Leave nothing open for the next step to trip over.
    document.querySelectorAll(".scrim, .menu").forEach((el) => el.remove());
  }
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

    if (state.signed_in && !state.prefs.folders_done) {
      // Signed in elsewhere (with `atl`): "Find your folders" shows once.
      await waitFor("the folder suggestions", () => sees("Folders to look through"), 20000);
      await sleep(400);
      await log(`atl folders before: ${text(document.querySelector("main")).slice(0, 520)}`);
      await shot("atl-folders-1");
      await click("Look through these");
      await waitFor("the matches", () => sees("In your studio as"), 20000);
      await sleep(400);
      await log(`atl folders after: ${text(document.querySelector("main")).slice(0, 700)}`);
      await shot("atl-folders-2");
      await click("Continue");
      await waitFor("the runner step", () => button("Finish"));
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

    const toggle = document.querySelector<HTMLButtonElement>(".sidebar [data-group=':others'] .group-toggle");
    if (toggle) {
      toggle.click();
      await sleep(300);
      await log(`other folders: ${text(document.querySelector(".sidebar .repos"))}`);
      await shot("sidebar-others");
    }
    const overview = shell.ctx.overview();
    const copy = overview?.copies[0];
    await screen(shell, "pulls", () => shell.go({ screen: "pulls" }), () => sees("open across your studio") && sees("Conversation"));
    await log(`pulls checks (all repos): ${[...document.querySelectorAll(".pulls-card")].map((c) => `${text(c.querySelector(".pulls-card-title"))} => ${text(c.querySelector(".checks-chip"))}`).join(" | ")}`);
    // Asked again and again once the harness is told to flip the switch.
    let allowedNow = false;
    const poll = setInterval(() => {
      void api.appState(true).then((state) => (allowedNow = state.can_admin));
    }, 500);
    if (copy) {
      const stage = () => document.querySelector(".stage") as HTMLElement;
      const tabNow = () => text(document.querySelector(".repo-tab[aria-current='page']"));
      const tab = (id: string) => document.querySelector<HTMLButtonElement>(`.repo-tab[data-tab='${id}']`);
      const sidebar = () => text(document.querySelector(".sidebar .repos"));
      const row = (name: string) =>
        [...document.querySelectorAll<HTMLElement>(".sidebar .side-row")].find((r) => text(r.querySelector(".side-repo .clip")) === name) ?? null;
      const type = (input: HTMLInputElement | HTMLTextAreaElement, value: string) => {
        input.value = value;
        input.dispatchEvent(new Event("input", { bubbles: true }));
      };

      // 5: a repository opens on History, from the sidebar, with no tab asked for.
      await step("default-tab", async () => {
        (await waitFor("the sidebar row", () => row(copy.name)?.querySelector<HTMLButtonElement>(".side-repo"))).click();
        await waitFor("the history graph", () => document.querySelector(".stage svg circle"));
        await log(`default tab: ${tabNow()} | order: ${[...document.querySelectorAll(".repo-tab")].map((t) => text(t)).join(", ")}`);
        await shot("history");
      });
      await screen(shell, "changes", () => tab("changes")?.click(), () => sees("Changed files") || sees("Nothing to commit"));
      const file = document.querySelector<HTMLElement>(".stage [aria-label^='Include']");
      await log(`changes: first file checkbox ${file ? "present" : "absent"}; diff rows ${document.querySelectorAll(".stage [class*='diff'] > *").length}`);
      await screen(shell, "branches", () => tab("branches")?.click(), () => sees("New branch"));

      // 1 and 4: this repository's pull requests, each with its checks at a glance.
      await step("repo-pulls", async () => {
        tab("pulls")?.click();
        await waitFor("the repository's pull requests", () => sees(`open in ${copy.name}`) && sees("Conversation"));
        await sleep(300);
        const cards = [...stage().querySelectorAll(".pulls-card")];
        await log(`repo pulls: tab=${tabNow()} count="${text(stage().querySelector(".pulls-head"))}" rows: ${cards.map((c) => `${text(c.querySelector(".pulls-card-title"))} [${text(c.querySelector(".muted"))}] => ${text(c.querySelector(".checks-chip"))} (${c.querySelector(".checks-chip")?.className})`).join(" | ")}`);
        await log(`repo pulls detail: ${text(stage().querySelector(".pulls-detail")).slice(0, 200)}`);
        await shot("repo-pulls");
      });

      // 8: issues.
      await step("issues", async () => {
        tab("issues")?.click();
        await waitFor("the issues", () => stage().querySelector(".issue-card") && sees("Conversation"));
        await sleep(300);
        await log(`issues open: ${[...stage().querySelectorAll(".issue-card")].map((c) => `${text(c)} (${c.className})`).join(" | ")}`);
        await log(`issue detail: ${text(stage().querySelector(".pulls-detail")).slice(0, 420)}`);
        await log(`issue markup kept as text: script elements in the pane = ${stage().querySelectorAll(".pulls-detail script").length}`);
        await shot("issues");
        const box = await waitFor("the comment box", () => stage().querySelector<HTMLTextAreaElement>(".issue-box"));
        type(box, "Seen it too, on battery.");
        await click("Comment");
        await waitFor("the new comment", () => sees("Seen it too, on battery."));
        await log(`issue after comment: ${text(stage().querySelector(".pulls-detail .section")).slice(0, 260)}`);
        await click("Close issue");
        await waitFor("the issue to close", () => button("Reopen issue"));
        await sleep(300);
        await log(`issues open after closing: ${[...stage().querySelectorAll(".issue-card")].map((c) => text(c.querySelector(".pulls-card-title"))).join(" | ")}`);
        [...stage().querySelectorAll<HTMLButtonElement>(".issues-state")].find((b) => text(b) === "Closed")?.click();
        await waitFor("the closed issues", () => [...stage().querySelectorAll(".issue-card")].some((c) => text(c).includes("Tray icon flickers")));
        await log(`issues closed: ${[...stage().querySelectorAll(".issue-card")].map((c) => text(c.querySelector(".pulls-card-title"))).join(" | ")}`);
        await click("Reopen issue");
        await waitFor("the issue to reopen", () => button("Close issue"));
        await click("New issue");
        const title = await waitFor("the new issue dialog", () => document.querySelector<HTMLInputElement>("#compose-title"));
        type(title, "Walk-through issue");
        type(document.querySelector("#compose-body") as HTMLTextAreaElement, "Opened by the self-test.");
        await click("Open issue");
        await waitFor("the new issue", () => text(stage().querySelector(".pulls-title")) === "Walk-through issue");
        await sleep(300);
        await log(`issues after new: ${[...stage().querySelectorAll(".issue-card")].map((c) => text(c.querySelector(".pulls-card-title"))).join(" | ")} :: ${text(stage().querySelector(".pulls-detail")).slice(0, 160)}`);
        await shot("issues-new");
      });

      // 4: runs gathered by pull request.
      await step("actions", async () => {
        tab("actions")?.click();
        await waitFor("the runs", () => stage().querySelector(".runs-row"));
        await log(`actions: ${[...stage().querySelectorAll(".runs-group")].map((g) => `${text(g.querySelector("h3"))} => ${[...g.querySelectorAll(".runs-row")].map((r) => text(r)).join("; ")}`).join(" | ")}`);
        await shot("actions");
      });

      // 6: pin from the bar, unpin from the sidebar.
      await step("pin", async () => {
        (await waitFor("the pin button", () => document.querySelector<HTMLButtonElement>(".repo-pin"))).click();
        await waitFor("the Pinned group", () => document.querySelector(".sidebar [data-group=':pinned']"));
        await log(`pinned: ${sidebar()} | bar pin pressed=${document.querySelector(".repo-pin")?.getAttribute("aria-pressed")} | prefs.pinned=${JSON.stringify((await api.appState()).prefs.pinned.map((p) => p.split("/").pop()))}`);
        await shot("pinned");
        (await waitFor("the row's menu button", () => row(copy.name)?.querySelector<HTMLButtonElement>(".side-more"))).click();
        await log(`sidebar menu: ${[...document.querySelectorAll(".menu .menu-item")].map((i) => text(i)).join(", ")}`);
        await click("Unpin");
        await waitFor("the Pinned group to go", () => !document.querySelector(".sidebar [data-group=':pinned']"));
        await log(`unpinned: ${sidebar()} | prefs.pinned=${JSON.stringify((await api.appState()).prefs.pinned)}`);
      });

      // 3: fold an owner's group; it is remembered in the config.
      await step("collapse", async () => {
        const head = () => document.querySelector<HTMLButtonElement>(".sidebar .group[data-group]:not([data-group^=':']) .group-toggle");
        const key = head()?.closest<HTMLElement>(".group")?.dataset.group ?? "?";
        await log(`owner groups: ${[...document.querySelectorAll<HTMLElement>(".sidebar .group")].map((g) => `${g.dataset.group}=${text(g.querySelector(".group-toggle"))}`).join(" | ")}`);
        head()?.click();
        await waitFor("the group to fold", () => head()?.getAttribute("aria-expanded") === "false");
        await sleep(400);
        await log(`collapsed ${key}: ${sidebar()} | prefs.collapsed=${JSON.stringify((await api.appState()).prefs.collapsed)}`);
        await shot("collapsed");
        head()?.click();
        await waitFor("the group to open", () => head()?.getAttribute("aria-expanded") === "true");
        await sleep(400);
        await log(`expanded ${key}: prefs.collapsed=${JSON.stringify((await api.appState()).prefs.collapsed)}`);
      });

      // 2: rename. The device isn't allowed at first (as in a real studio
      // until the owner switches it on): the app says so before asking for
      // a name. The harness then flips the switch in the mock studio.
      await step("rename-refused", async () => {
        await log(`can_admin at first: ${(await api.appState(true)).can_admin}`);
        (await waitFor("the row's menu button", () => row(copy.name)?.querySelector<HTMLButtonElement>(".side-more"))).click();
        await click("Rename…");
        await waitFor("the refusal", () => button("Open Devices settings"));
        await log(`rename refused: ${text(document.querySelector(".dialog"))}`);
        await shot("rename-refused");
        await click("Cancel");
        await log("ready-for-admin");
        await waitFor("the device to be allowed", () => allowedNow, 20000);
      });
      await step("rename", async () => {
        (await waitFor("the bar's menu", () => document.querySelector<HTMLButtonElement>(".repo-more"))).click();
        await log(`bar menu: ${[...document.querySelectorAll(".menu .menu-item")].map((i) => text(i)).join(", ")}`);
        await click("Rename…");
        const input = await waitFor("the name field", () => document.querySelector<HTMLInputElement>("#prompt-input"));
        await log(`rename dialog: ${text(document.querySelector(".dialog"))} value=${input.value}`);
        type(input, "Studio");
        await shot("rename");
        await click("Rename");
        await waitFor("the new name", () => text(document.querySelector(".repo-title")).endsWith("Studio") && row("Studio"));
        await sleep(500);
        const view = await api.repoView(copy.path);
        await log(`renamed: title="${text(document.querySelector(".repo-title"))}" full_name=${view.full_name} sidebar: ${sidebar()}`);
        await shot("renamed");
      });

      // 2: delete, with the name typed. The folder stays.
      await step("delete", async () => {
        (await waitFor("the bar's menu", () => document.querySelector<HTMLButtonElement>(".repo-more"))).click();
        await click("Delete from studio…");
        const input = await waitFor("the confirmation field", () => document.querySelector<HTMLInputElement>("#typed-confirm-input"));
        const confirm = () => [...document.querySelectorAll<HTMLButtonElement>(".dialog button")].find((b) => text(b) === "Delete from studio");
        await log(`delete dialog: ${text(document.querySelector(".dialog"))}`);
        type(input, "studio");
        await log(`delete button with the wrong name: disabled=${confirm()?.disabled}`);
        type(input, "Studio");
        await log(`delete button with the right name: disabled=${confirm()?.disabled}`);
        await shot("delete");
        confirm()?.click();
        await waitFor("the repository to leave the studio", () => sees("Not in your studio yet") && !row("Studio")?.closest("[data-group='demiinfinity']"));
        await sleep(500);
        const others = document.querySelector<HTMLButtonElement>(".sidebar [data-group=':others'] .group-toggle");
        if (others?.getAttribute("aria-expanded") === "false") others.click();
        await sleep(300);
        const view = await api.repoView(copy.path);
        await log(`deleted: full_name=${view.full_name} folder still opens=${view.path === copy.path} tabs: ${[...document.querySelectorAll(".repo-tab")].map((t) => text(t)).join(", ")} | sidebar: ${sidebar()}`);
        await shot("deleted");
      });
    } else {
      await log("FAIL no working copy to open");
    }
    clearInterval(poll);
    await screen(shell, "device", () => shell.go({ screen: "device" }), () => sees("When to work"));
    await screen(shell, "settings", () => shell.go({ screen: "settings" }), () => sees("Who you commit as") && sees("Remote access") && sees("Clones go to"));
    await log(`folders card: ${text([...document.querySelectorAll(".stage section")].find((el) => text(el).startsWith("Folders")))}`);
    await screen(shell, "messages", () => shell.go({ screen: "messages" }), () => sees("After a push") && sees("Try it"));
    await screen(shell, "add", () => shell.go({ screen: "add", tab: "clone" }), () => sees("Add a repository") && sees("In your studio"));
    await screen(shell, "add-open", () => shell.go({ screen: "add", tab: "open" }), () => sees("Open a folder on this"));
    // 7: who a published folder belongs to. Any folder the studio doesn't
    // know will do (the one just deleted from the studio, say).
    const other = shell.ctx.overview()?.others[0];
    if (other) {
      await screen(shell, "publish-other", () => shell.go({ screen: "add", tab: "publish", path: other.path }), () => sees("A git folder") && sees("This belongs to"));
      const options = () => [...document.querySelectorAll<HTMLElement>("#add-publish-owner .owner-option")];
      const chosen = () => options().map((o) => `${text(o)}${o.querySelector<HTMLInputElement>("input")?.checked ? " (chosen)" : ""}`).join(" | ");
      await log(`owner chooser: ${text(document.querySelector("#add-publish-owner legend"))}: ${chosen()}`);
      options()[1]?.querySelector<HTMLInputElement>("input")?.click();
      await sleep(200);
      await log(`owner chooser after a click: ${chosen()} | publish button: ${text(document.querySelector(".add-foot .primary"))} disabled=${document.querySelector<HTMLButtonElement>(".add-foot .primary")?.disabled}`);
      await shot("owner-chooser");
      [...document.querySelectorAll<HTMLButtonElement>(".add-tab")].find((b) => text(b) === "Start new")?.click();
      await sleep(200);
      await log(`owner carried to Start new: ${[...document.querySelectorAll<HTMLElement>("#add-new-owner .owner-option")].map((o) => `${text(o)}${o.querySelector<HTMLInputElement>("input")?.checked ? " (chosen)" : ""}`).join(" | ")}`);
    } else {
      await log("FAIL no folder outside the studio to publish");
    }
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
