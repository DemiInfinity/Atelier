import "./styles/fonts";
import "./styles/base.css";
import { api, asError } from "./api";
import { h, mount } from "./dom";
import { Shell } from "./shell";

const root = document.getElementById("app")!;
const shell = new Shell(root);

shell
  .start()
  .then(async () => {
    // A scripted walk through the app for checking a build without a person
    // at the keyboard. Only when the app was started with the variable set.
    const plan = await api.selftestPlan().catch(() => null);
    if (plan) {
      const { runSelfTest } = await import("./selftest");
      void runSelfTest(shell, plan);
    }
  })
  .catch((e) => {
    const err = asError(e);
    mount(
      root,
      h(
        "div",
        { class: "loading col", style: { gap: "8px" } },
        h("span", { class: "h-screen", text: "Atelier couldn’t start" }),
        h("span", { class: "selectable", text: err.message }),
        err.hint ? h("span", { class: "muted", text: err.hint }) : null,
      ),
    );
  });
