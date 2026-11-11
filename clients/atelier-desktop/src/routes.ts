// The screens of the main window.

export type RepoTab = "changes" | "history" | "branches";
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
