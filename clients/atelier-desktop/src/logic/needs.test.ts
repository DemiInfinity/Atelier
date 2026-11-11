import { describe, expect, it } from "vitest";
import { needStep, routeFor } from "./needs";

describe("needs list actions", () => {
  it("sends an agent approval to This device with the job lit", () => {
    expect(needStep({ type: "approve_agent", job_id: 31 })).toEqual({
      do: "go",
      route: { screen: "device", approve: 31 },
    });
  });

  it("shows a failed run's log in the app", () => {
    expect(
      needStep({ type: "open_log", run_id: 208, repo: "DemiInfinity/Atelier", web_path: "/DemiInfinity/Atelier/actions/runs/208" }),
    ).toEqual({ do: "log", repo: "DemiInfinity/Atelier", runId: 208, webPath: "/DemiInfinity/Atelier/actions/runs/208" });
  });

  it("opens a pull request on the pulls screen", () => {
    expect(needStep({ type: "open_pull", repo: "DemiInfinity/Tray", number: 9 })).toEqual({
      do: "go",
      route: { screen: "pulls", repo: "DemiInfinity/Tray", number: 9 },
    });
  });

  it("pushes a working copy", () => {
    expect(needStep({ type: "push", path: "/Users/x/Atelier/me/Marbl" })).toEqual({
      do: "push",
      path: "/Users/x/Atelier/me/Marbl",
    });
  });

  it("falls back to the studio page", () => {
    expect(needStep({ type: "open_web", web_path: "/x/y/security" })).toEqual({ do: "web", webPath: "/x/y/security" });
  });
});

describe("where pop-ups lead", () => {
  it("maps targets to screens", () => {
    expect(routeFor({ screen: "device" })).toEqual({ screen: "device" });
    expect(routeFor({ screen: "pull", repo: "a/b", number: 3 })).toEqual({ screen: "pulls", repo: "a/b", number: 3 });
    expect(routeFor({ screen: "repo", path: "/p" })).toEqual({ screen: "repo", path: "/p" });
    expect(routeFor({ screen: "today" })).toEqual({ screen: "today" });
    expect(routeFor({ screen: "pulls" })).toEqual({ screen: "pulls" });
  });

  it("leaves studio pages to the browser", () => {
    expect(routeFor({ screen: "web", web_path: "/a/b/actions/runs/1" })).toBeNull();
  });
});
