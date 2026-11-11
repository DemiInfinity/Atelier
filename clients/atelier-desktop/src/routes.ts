// The screens of the main window.

/** A repository's tabs, in the order they are shown. History comes first and is where a repository opens. */
export type RepoTab = "history" | "changes" | "pulls" | "issues" | "branches" | "actions";
export const DEFAULT_REPO_TAB: RepoTab = "history";
export type AddTab = "publish" | "clone" | "new" | "open";

export type Route =
  | { screen: "today" }
  | { screen: "pulls"; repo?: string; number?: number }
  | { screen: "repo"; path: string; tab?: RepoTab }
  /** `approve`: scroll to and highlight this waiting agent. */
  | { screen: "device"; approve?: number }
  /** `path`: a folder already known to the app, to publish (skips the chooser). */
  | { screen: "add"; tab?: AddTab; path?: string }
  | { screen: "settings" }
  | { screen: "messages" };

/** Which sidebar entry is lit for a route. */
export function sidebarKey(route: Route): string {
  switch (route.screen) {
    case "repo":
      return `repo:${route.path}`;
    case "messages":
      return "settings";
    case "add":
      return "none";
    default:
      return route.screen;
  }
}
