// What a "needs you" button does. Kept apart from the screens so the
// mapping can be tested without a window.

import type { NeedAction, Target } from "../api";
import type { Route } from "../routes";

/** One step the window should take for a need's button. */
export type NeedStep =
  /** Go to a screen of the app. */
  | { do: "go"; route: Route }
  /** Push a working copy, then refresh. */
  | { do: "push"; path: string }
  /** Show a failed run's log in the app. */
  | { do: "log"; repo: string; runId: number; webPath: string }
  /** Open a page of the studio in the browser. */
  | { do: "web"; webPath: string };

export function needStep(action: NeedAction): NeedStep {
  switch (action.type) {
    case "approve_agent":
      // Approving shows the tools and limits first, on "This device".
      return { do: "go", route: { screen: "device", approve: action.job_id } };
    case "open_log":
      return { do: "log", repo: action.repo, runId: action.run_id, webPath: action.web_path };
    case "open_pull":
      return { do: "go", route: { screen: "pulls", repo: action.repo, number: action.number } };
    case "push":
      return { do: "push", path: action.path };
    case "open_web":
      return { do: "web", webPath: action.web_path };
  }
}

/** The screen a pop-up or feed item leads to; `null` means "open in the browser". */
export function routeFor(target: Target): Route | null {
  switch (target.screen) {
    case "today":
      return { screen: "today" };
    case "device":
      return { screen: "device" };
    case "pulls":
      return { screen: "pulls" };
    case "pull":
      return { screen: "pulls", repo: target.repo, number: target.number };
    case "repo":
      return { screen: "repo", path: target.path };
    case "add":
      return { screen: "add", tab: target.tab };
    case "settings":
      return { screen: "settings" };
    case "web":
      return null;
  }
}
