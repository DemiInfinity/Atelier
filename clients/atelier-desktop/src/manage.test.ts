import { afterEach, describe, expect, it } from "vitest";
import type { Ctx, MenuItem } from "./ctx";
import { typedConfirmDialog } from "./dialogs";
import { deleteWords, manageMenu, refusal, renamedLine, settingsPath } from "./manage";

const labels = (items: MenuItem[]) => items.map((i) => ("label" in i ? i.label : "separator" in i ? "—" : i.note));
const ctx = {} as Ctx;

describe("the … menu of a repository", () => {
  it("offers pin, rename and delete for one in the studio", () => {
    const repo = { path: "/code/Atelier", name: "Atelier", full_name: "DemiInfinity/Atelier", pinned: false };
    expect(labels(manageMenu(ctx, repo))).toEqual(["Pin to the top", "—", "Rename…", "Delete from studio…"]);
    expect(labels(manageMenu(ctx, { ...repo, pinned: true }))[0]).toBe("Unpin");
  });

  it("only offers the pin for a folder the studio doesn't know", () => {
    expect(labels(manageMenu(ctx, { path: "/code/x", name: "x", full_name: null, pinned: false }))).toEqual(["Pin to the top"]);
  });
});

describe("deleting", () => {
  const words = deleteWords({ name: "Atelier", full_name: "DemiInfinity/Atelier" }, "Mac");

  it("says it is the studio's copy that goes and the folder that stays", () => {
    expect(words.title).toBe("Delete Atelier from your studio?");
    expect(words.body[0]).toContain("DemiInfinity/Atelier is deleted from your studio for good");
    expect(words.body[1]).toBe("The folder on this Mac is kept exactly as it is. Afterwards it shows under “Other folders”.");
    expect(words.expect).toBe("Atelier");
    expect(words.confirm).toBe("Delete from studio");
  });

  afterEach(() => {
    document.body.replaceChildren();
  });

  it("only lets the button work once the name is typed exactly", async () => {
    const answer = typedConfirmDialog(words);
    const dialog = document.querySelector<HTMLFormElement>("form.dialog");
    expect(dialog?.getAttribute("role")).toBe("alertdialog");
    expect(dialog?.textContent).toContain("The folder on this Mac is kept exactly as it is.");
    expect(dialog?.querySelector("label")?.textContent).toBe("Type Atelier to confirm");
    const input = dialog?.querySelector("input") as HTMLInputElement;
    const button = dialog?.querySelector<HTMLButtonElement>("button[type=submit]") as HTMLButtonElement;
    expect(button.textContent).toBe("Delete from studio");
    expect(button.classList.contains("danger")).toBe(true);
    expect(button.disabled).toBe(true);
    for (const wrong of ["atelier", "Atelie", "Atelier2"]) {
      input.value = wrong;
      input.dispatchEvent(new Event("input"));
      expect(button.disabled).toBe(true);
    }
    // Submitting with the wrong name does nothing.
    dialog?.dispatchEvent(new Event("submit", { cancelable: true }));
    expect(document.querySelector("form.dialog")).not.toBeNull();
    input.value = "Atelier";
    input.dispatchEvent(new Event("input"));
    expect(button.disabled).toBe(false);
    dialog?.dispatchEvent(new Event("submit", { cancelable: true }));
    expect(await answer).toBe("Atelier");
    expect(document.querySelector("form.dialog")).toBeNull();
  });

  it("answers null when cancelled", async () => {
    const answer = typedConfirmDialog(words);
    const cancel = [...document.querySelectorAll<HTMLButtonElement>("form.dialog button")].find((b) => b.textContent === "Cancel");
    cancel?.click();
    expect(await answer).toBeNull();
  });
});

describe("renaming", () => {
  it("says what happened to the folder's remotes", () => {
    expect(renamedLine(["origin"], null)).toBe("This folder's remote “origin” points at the new address.");
    expect(renamedLine(["origin", "ssh"], null)).toBe("This folder's remotes origin, ssh point at the new address.");
    expect(renamedLine([], null)).toBe("The folder here needed no change.");
    expect(renamedLine([], "git said no.")).toContain("couldn't be updated: git said no.");
  });

  it("knows the page in the studio where it can be done instead", () => {
    expect(settingsPath("DemiInfinity/Atelier")).toBe("/DemiInfinity/Atelier/settings");
  });
});

describe("a refusal from the studio", () => {
  it("leads to the Devices settings when this device isn't allowed yet", () => {
    const words = refusal(
      { kind: "device_not_allowed", message: "This Mac isn’t allowed to rename or delete repositories yet.", hint: "You can allow it in your studio, under Settings → Devices, or do it in the browser." },
      "DemiInfinity/Atelier",
    );
    expect(words).toEqual({
      title: "This Mac isn’t allowed to rename or delete repositories yet.",
      body: "You can allow it in your studio, under Settings → Devices, or do it in the browser.",
      confirm: "Open Devices settings",
      webPath: "/settings/devices",
    });
  });

  it("leads to the repository's settings for any other no", () => {
    const words = refusal({ kind: "not_allowed", message: "You can’t rename or delete Atelier from here.", hint: null }, "DemiInfinity/Atelier");
    expect(words?.confirm).toBe("Open in browser");
    expect(words?.webPath).toBe("/DemiInfinity/Atelier/settings");
  });

  it("leaves everything else to the usual pop-up", () => {
    expect(refusal({ kind: "name_taken", message: "That name is already taken.", hint: null }, "a/b")).toBeNull();
  });
});

