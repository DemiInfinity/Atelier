// First run: four steps in a full window, no sidebar. Connect to the
// studio, approve this machine in the browser, look for git folders that
// are already here, and see whether a runner is set up.
//
// Nothing on disk changes in steps 3 and 4: they only look. (Step 3 can add
// folders to the app's own list of places to look, nothing more.)
//
// Someone who signed in another way (with `atl`, say) never saw this, so the
// app can also open it straight on step 3.

import "../styles/welcome.css";
import { api, asError, type CmdError, type Connected, type ConnectStarted, type Folders, type FoundFolder, type RunnerProbe, type Suggestion } from "../api";
import type { Ctx, Screen } from "../ctx";
import { checkbox, h, icon, mark, mount, pill, plural, type Child } from "../dom";

// ------------------------------------------------------------------ words

/** The four steps as the side column names them. */
export function stepNames(machine: string): { title: string; detail: string }[] {
  return [
    { title: "Connect your studio", detail: "Tell the app where Atelier lives" },
    { title: `Approve this ${machine}`, detail: "With your passkey, in the browser" },
    { title: "Find your folders", detail: "Match the git folders you already have to repositories" },
    { title: "Lend a hand", detail: `Optionally let this ${machine} run jobs and agents` },
  ];
}

/** How a step is drawn in the side column, given the step we are on. */
export function stepMark(index: number, current: number): "done" | "current" | "todo" {
  if (index < current) return "done";
  return index === current ? "current" : "todo";
}

/** Puts `, ` and ` and ` between items: `a, b and c`. */
export function sentenceList<T>(items: readonly T[]): (T | string)[] {
  const out: (T | string)[] = [];
  items.forEach((item, i) => {
    if (i > 0) out.push(i === items.length - 1 ? " and " : ", ");
    out.push(item);
  });
  return out;
}

/** `another 5 minutes`, `less than a minute`. */
export function timeLeft(seconds: number): string {
  if (seconds < 60) return "less than a minute";
  return `another ${plural(Math.ceil(seconds / 60), "minute", "minutes")}`;
}

/** The sentence about this machine's SSH key after approval. */
export function sshLine(connected: Pick<Connected, "ssh" | "ssh_error">, machine: string): { text: string; fingerprint: string | null; failed: boolean } | null {
  if (connected.ssh) {
    return {
      text: connected.ssh.generated
        ? `A new SSH key was made for this ${machine} and added to your studio`
        : `This ${machine}'s SSH key was added to your studio`,
      fingerprint: connected.ssh.fingerprint,
      failed: false,
    };
  }
  if (connected.ssh_error) return { text: connected.ssh_error, fingerprint: null, failed: true };
  return null;
}

/**
 * Which suggested folders "Look through these" sends: the ticked ones that
 * aren't looked through already (those are shown ticked, but there is
 * nothing to add).
 */
export function pathsToSend(suggestions: readonly Pick<Suggestion, "path" | "watched">[], ticked: ReadonlySet<string>): string[] {
  return suggestions.filter((s) => !s.watched && ticked.has(s.path)).map((s) => s.path);
}

// ----------------------------------------------------------------- screen

export function welcomeScreen(ctx: Ctx, done: () => void, startAt?: "folders"): Screen {
  const machine = ctx.app().machine;
  const names = stepNames(machine);

  let disposed = false;
  // Every step change gets a new number, so answers that arrive for a step
  // we already left are dropped.
  let turn = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let address = "";

  const steps = h("ol", { class: "welcome-steps" });
  const main = h("main", { class: "welcome-main" });

  const el = h(
    "div",
    { class: "welcome" },
    h(
      "aside",
      { class: "welcome-aside" },
      h("div", { class: "drag-strip", "data-tauri-drag-region": true }),
      h("div", { class: "welcome-brand" }, mark(40), h("span", { class: "welcome-brand-name", text: "Atelier Desktop" })),
      steps,
      h("span", {
        class: "welcome-footnote",
        text: `Works alongside the git tools you already use. Nothing on this ${machine} changes until you say so.`,
      }),
    ),
    h("div", { class: "welcome-right" }, h("div", { class: "drag-strip", "data-tauri-drag-region": true }), main),
  );

  const alive = (mine: number) => !disposed && mine === turn;

  function later(work: () => void, ms: number): void {
    clearTimeout(timer);
    timer = setTimeout(work, ms);
  }

  /** Starts a step: lights it in the side column and stops the last one's timer. */
  function enter(step: number): number {
    turn += 1;
    clearTimeout(timer);
    mount(
      steps,
      names.map((name, i) => {
        const state = stepMark(i, step);
        return h(
          "li",
          { class: ["welcome-step-row", state], "aria-current": state === "current" ? "step" : undefined },
          h("span", { class: "welcome-step-n", "aria-hidden": "true" }, state === "done" ? icon("check", 14, 2.4) : String(i + 1)),
          h(
            "span",
            { class: "col" },
            h("span", { class: "welcome-step-title", text: name.title }),
            h("span", { class: "welcome-step-detail", text: name.detail }),
          ),
        );
      }),
    );
    return turn;
  }

  /** Puts a step's content in the main area and moves focus to its heading. */
  function show(heading: string, ...children: Child[]): void {
    const title = h("h1", { class: "welcome-title", tabindex: "-1", text: heading });
    mount(main, h("div", { class: "welcome-step" }, title, children));
    main.scrollTop = 0;
    title.focus();
  }

  const lead = (...children: Child[]) => h("p", { class: "welcome-lead" }, children);
  const quiet = (text: string) => h("span", { class: "welcome-quiet", text });
  const skip = (go: () => void) => h("button", { type: "button", class: "btn lg link welcome-skip", text: "Skip", onclick: go });

  function problem(err: CmdError): HTMLElement {
    return h(
      "span",
      { class: "welcome-error", role: "alert" },
      h("span", { text: err.message || "Something went wrong." }),
      err.hint ? h("span", { class: "welcome-error-hint", text: err.hint }) : null,
    );
  }

  // --------------------------------------------------- 1. where is it?

  function connectStep(): void {
    const mine = enter(0);
    const input = h("input", {
      id: "welcome-addr",
      class: "welcome-input selectable",
      type: "text",
      inputmode: "url",
      placeholder: "https://atelier.example.lan",
      autocomplete: "off",
      autocapitalize: "off",
      spellcheck: "false",
      value: address,
    });
    const button = h("button", { type: "submit", class: "btn primary xl", text: "Connect" });
    const status = h("div", { class: "welcome-status", "aria-live": "polite" });

    const connect = async () => {
      const value = input.value.trim();
      if (!value || button.disabled) return;
      address = value;
      button.disabled = true;
      mount(status, h("span", { class: "welcome-wait", text: "Looking for your studio…" }));
      try {
        const started = await api.connectStart(value);
        if (!alive(mine)) return;
        mount(
          status,
          h("span", { class: "welcome-found-line" }, h("span", { class: "dot sm ok", "aria-hidden": "true" }), "Found Atelier at this address"),
        );
        // Long enough to read the green line, then on to the code.
        later(() => approveStep(started), 900);
      } catch (e) {
        if (!alive(mine)) return;
        // Under the field, not as a pop-up: this is the thing being typed.
        mount(status, problem(asError(e)));
        button.disabled = false;
        input.focus();
      }
    };

    show(
      "Where is your studio?",
      lead("Type the address you use to open Atelier in your browser."),
      h(
        "form",
        {
          class: "welcome-form",
          onsubmit: (e) => {
            e.preventDefault();
            void connect();
          },
        },
        h("label", { for: "welcome-addr", class: "welcome-label", text: "Studio address" }),
        h("div", { class: "welcome-form-row" }, input, button),
        status,
      ),
      h(
        "div",
        { class: "welcome-box" },
        h("span", { class: "welcome-box-title", text: "What happens next" }),
        h("span", {
          class: "soft",
          text: `Your studio opens in the browser and asks you to approve this ${machine} with your passkey. The app gets its own key, which you can revoke at any time. Your password is never typed here.`,
        }),
      ),
      quiet("Don't have a studio yet? Set one up first, then come back here."),
    );
    input.focus();
  }

  // ------------------------------------------------ 2. approve this Mac

  function approveStep(started: ConnectStarted): void {
    const mine = enter(1);
    const waiting = h("span", { class: "welcome-wait", "aria-live": "polite", text: "Waiting for you to approve…" });
    const every = Math.max(1, started.interval) * 1000;

    const poll = async () => {
      try {
        const answer = await api.connectPoll();
        if (!alive(mine)) return;
        if (answer.status === "approved") {
          approved(answer);
          return;
        }
        waiting.textContent = `Waiting for you to approve… The code works for ${timeLeft(answer.seconds_left)}.`;
        later(() => void poll(), every);
      } catch (e) {
        if (!alive(mine)) return;
        refused(asError(e));
      }
    };

    const reopen = h("button", {
      type: "button",
      class: "btn lg",
      text: "Open the page again",
      onclick: () => void ctx.busy(reopen, () => api.connectReopen()),
    });
    const over = h("button", {
      type: "button",
      class: "btn lg",
      text: "Start over",
      onclick: () =>
        void ctx.busy(over, async () => {
          await api.connectCancel();
          if (alive(mine)) connectStep();
        }),
    });

    show(
      `Approve this ${machine} in your browser`,
      lead(
        "Your studio at ",
        h("span", { class: "mono selectable", text: started.studio_host }),
        " is asking in the browser. Check that the code there matches this one, then approve with your passkey.",
      ),
      h("div", { class: "welcome-code selectable", "aria-label": "Your code", text: started.user_code }),
      started.opened
        ? null
        : h(
            "div",
            { class: "welcome-box" },
            h("span", { class: "welcome-box-title", text: "The browser couldn't be opened from here" }),
            h("span", { class: "soft", text: "Open this address yourself:" }),
            h("span", { class: "welcome-url mono selectable", text: started.verify_url }),
          ),
      waiting,
      h("div", { class: "welcome-actions" }, reopen, over),
    );
    later(() => void poll(), every);
  }

  function approved(connected: Connected): void {
    const mine = enter(1);
    const ssh = sshLine(connected, machine);
    const next = h("button", {
      type: "button",
      class: "btn primary xl",
      text: "Continue",
      onclick: () => {
        if (alive(mine)) foldersStep();
      },
    });
    show(
      `This ${machine} is approved`,
      h(
        "div",
        { class: "welcome-box welcome-facts" },
        h(
          "span",
          { class: "welcome-fact" },
          h("span", { class: "dot sm ok", "aria-hidden": "true" }),
          h("span", { text: connected.studio_name ? `Signed in as ${connected.person_name} · ${connected.studio_name}` : `Signed in as ${connected.person_name}` }),
        ),
        ssh
          ? h(
              "span",
              { class: ["welcome-fact", ssh.failed && "warn"] },
              h("span", { class: ["dot sm", ssh.failed ? "warn" : "ok"], "aria-hidden": "true" }),
              h(
                "span",
                { class: "col" },
                h("span", { text: ssh.text }),
                ssh.fingerprint ? h("span", { class: "welcome-print mono selectable", text: ssh.fingerprint }) : null,
              ),
            )
          : null,
      ),
      connected.token_place === "file"
        ? quiet("There is no keychain here, so the app's token is kept in a file only you can read.")
        : null,
      h("div", { class: "welcome-actions" }, next),
    );
    next.focus();
  }

  function refused(err: CmdError): void {
    enter(1);
    const again = h("button", {
      type: "button",
      class: "btn primary xl",
      text: "Start again",
      onclick: () => {
        // The request is over on the studio's side; this only tidies up here.
        void api.connectCancel().catch(() => undefined);
        connectStep();
      },
    });
    show(`This ${machine} wasn't approved`, problem(err), h("div", { class: "welcome-actions" }, again));
  }

  // -------------------------------------------- 3. find your folders

  /** `already`: signed in before the app was first opened, so steps 1 and 2 never showed. */
  function foldersStep(already = false): void {
    const mine = enter(2);
    const body = h("div", { class: "welcome-found" });
    const suggest = h("div", { class: "welcome-suggest hidden" });

    // What is ticked among the suggestions. A folder starts ticked the first
    // time it is seen; after that the person's own choice is kept.
    const ticked = new Set<string>();
    const seen = new Set<string>();
    let suggestions: Suggestion[] = [];

    const loadFound = async () => {
      mount(body, h("span", { class: "welcome-wait", text: "Looking through your folders…" }));
      try {
        const found = await api.findFolders();
        if (alive(mine)) mount(body, drawFolders(found));
      } catch (e) {
        if (!alive(mine)) return;
        mount(
          body,
          problem(asError(e)),
          h("div", null, h("button", { type: "button", class: "btn", text: "Try again", onclick: () => void loadFound() })),
        );
      }
    };

    const loadSuggestions = async () => {
      try {
        const list = await api.suggestFolders();
        if (!alive(mine)) return;
        suggestions = list;
        for (const s of list) {
          if (!seen.has(s.path) && !s.watched) ticked.add(s.path);
          seen.add(s.path);
        }
      } catch (e) {
        if (!alive(mine)) return;
        // The step works without suggestions; say why they are missing.
        suggestions = [];
        ctx.fail(e);
      }
      drawSuggestions();
    };

    const load = () => Promise.all([loadSuggestions(), loadFound()]);

    const through = h("button", {
      type: "button",
      class: "btn lg",
      text: "Look through these",
      onclick: () =>
        void ctx.busy(through, async () => {
          const paths = pathsToSend(suggestions, ticked);
          if (paths.length === 0) return;
          await api.addSuggestedFolders(paths);
          if (alive(mine)) await load();
          // `busy` enables the button again when it is done; put it back in
          // line with what is ticked (nothing new, usually).
        }).then(syncThrough),
    });
    const look = h("button", {
      type: "button",
      class: "btn lg",
      text: "Look in another folder…",
      onclick: () =>
        void ctx.busy(look, async () => {
          const added = await api.addWatchFolder();
          if (added !== null && alive(mine)) await load();
        }),
    });
    const go = () => {
      if (alive(mine)) runnerStep();
    };
    const skipButton = skip(go);
    const actions = h(
      "div",
      { class: "welcome-actions" },
      h("button", { type: "button", class: "btn primary xl", text: "Continue", onclick: go }),
      look,
      skipButton,
    );

    const syncThrough = () => {
      through.disabled = pathsToSend(suggestions, ticked).length === 0;
    };

    function drawSuggestions(): void {
      suggest.classList.toggle("hidden", suggestions.length === 0);
      if (suggestions.length === 0) {
        // Nothing to suggest: the chooser stays with Continue, as before.
        mount(suggest);
        actions.insertBefore(look, skipButton);
        return;
      }
      mount(
        suggest,
        h("span", { class: "welcome-box-title", text: "Folders to look through" }),
        h("div", { class: "table welcome-suggest-list" }, suggestions.map(suggestionRow)),
        // With suggestions on screen the chooser belongs next to them.
        h("div", { class: "welcome-actions" }, through, look),
      );
      syncThrough();
    }

    function suggestionRow(s: Suggestion): HTMLElement {
      const row = checkbox({
        checked: s.watched || ticked.has(s.path),
        disabled: s.watched,
        label: [
          h("span", { class: "welcome-suggest-path mono", text: s.display }),
          h("span", { class: "welcome-suggest-count", text: plural(s.repos, "git folder", "git folders") }),
          s.watched ? h("span", { class: "welcome-suggest-count", text: "· already looked through" }) : null,
        ],
        onchange: (next) => {
          if (next) ticked.add(s.path);
          else ticked.delete(s.path);
          syncThrough();
        },
      });
      row.classList.add("welcome-suggest-row");
      return row;
    }

    show(
      "Find your folders",
      already ? lead(`You're already connected. Let's find the repositories that are on this ${machine}.`) : null,
      suggest,
      body,
      quiet(
        `This step only looks. Nothing inside your folders is changed; adding a folder only adds it to Atelier's own list of places to look.`,
      ),
      actions,
    );
    void load();
  }

  function drawFolders(found: Folders): Child[] {
    const roots = sentenceList(found.roots.map((root) => h("span", { class: "mono", text: root })));
    const intro =
      found.roots.length > 0
        ? lead("Atelier looked through ", roots, " for git folders you already have.")
        : lead("Atelier isn't looking in any folder yet. Pick one to look through.");
    if (found.folders.length === 0) {
      return [
        intro,
        h("div", { class: "empty", text: "No git folders were found there. You can look in another folder, or carry on and add repositories later." }),
      ];
    }
    return [
      intro,
      h("div", { class: "table welcome-list" }, found.folders.map(folderRow)),
      h("span", {
        class: "soft",
        text: `Matched ${found.matched} of ${plural(found.folders.length, "folder", "folders")} to your ${plural(found.studio_repos, "repository", "repositories")}`,
      }),
    ];
  }

  function folderRow(folder: FoundFolder): HTMLElement {
    return h(
      "div",
      { class: "welcome-folder" },
      h(
        "span",
        { class: "col grow" },
        h(
          "span",
          { class: "welcome-folder-top" },
          h("span", { class: "welcome-folder-name clip", text: folder.name }),
          folder.branch ? h("span", { class: "welcome-folder-branch mono clip", text: folder.branch }) : null,
        ),
        h("span", { class: "welcome-folder-path mono clip", text: folder.display }),
      ),
      folder.repo
        ? h("span", { class: "welcome-match yours", text: `In your studio as ${folder.repo}` })
        : h(
            "span",
            { class: "welcome-match" },
            "Not in your studio yet",
            folder.remote_host === "github.com" ? h("span", { class: "welcome-github", text: " · on GitHub" }) : null,
          ),
    );
  }

  // ---------------------------------------------------- 4. lend a hand

  function runnerStep(): void {
    const mine = enter(3);
    const body = h("div", { class: "welcome-found" }, h("span", { class: "welcome-wait", text: "Looking for a runner…" }));
    const finish = () => {
      if (alive(mine)) done();
    };
    const end = h("button", { type: "button", class: "btn primary xl", text: "Finish", onclick: finish });

    show(
      "Lend a hand",
      body,
      quiet("This version of the app doesn't install or change the runner."),
      h("div", { class: "welcome-actions" }, end, skip(finish)),
    );

    void api.runnerProbe().then(
      (probe) => {
        if (alive(mine)) mount(body, drawRunner(probe));
      },
      (e) => {
        if (alive(mine)) mount(body, problem(asError(e)));
      },
    );
  }

  function drawRunner(probe: RunnerProbe): Child[] {
    if (probe.running) {
      return [
        lead(`A runner is already working on this ${machine}, so it can run your jobs and agents.`),
        h(
          "div",
          { class: "welcome-box welcome-runner" },
          h(
            "span",
            { class: "welcome-fact" },
            h("span", { class: ["dot sm tone-agents", !probe.taking_jobs && "off"], "aria-hidden": "true" }),
            h("span", { class: "welcome-box-title grow clip", text: probe.name ?? "Runner" }),
            h("span", { class: "soft", text: probe.taking_jobs ? "Taking jobs" : "Not taking jobs right now" }),
          ),
          probe.capabilities.length > 0
            ? h("span", { class: "welcome-caps" }, probe.capabilities.map((cap) => pill(cap, "agents", "mono")))
            : null,
        ),
      ];
    }
    if (probe.installed) {
      return [
        lead(`The runner is installed on this ${machine} but isn't running. Jobs will be picked up once it is started.`),
      ];
    }
    const how = h("button", {
      type: "button",
      class: "btn lg",
      text: "How to set up a runner ↗",
      onclick: () => void ctx.busy(how, () => api.openWeb(probe.how_web_path)),
    });
    return [
      lead(
        `Lending a hand means this ${machine} can run your jobs and agents, next to whatever else your studio uses. It is optional, and you can set it up whenever you like.`,
      ),
      h("div", null, how),
    ];
  }

  if (startAt === "folders") foldersStep(true);
  else connectStep();

  return {
    el,
    dispose() {
      disposed = true;
      clearTimeout(timer);
    },
  };
}
