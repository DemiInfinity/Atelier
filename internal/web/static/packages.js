// Packages: .npmrc scope toggles and delete confirmations. Progressive
// enhancement only; the pages work without it.
(function () {
  "use strict";

  function buildNpmrc() {
    var pre = document.getElementById("pk-npmrc");
    if (!pre) return;
    var registry = pre.getAttribute("data-pk-registry");
    var host = pre.getAttribute("data-pk-host");
    var lines = [];
    var all = document.querySelector("[data-pk-all]");
    if (all && all.checked) lines.push("registry=" + registry);
    var boxes = document.querySelectorAll("[data-pk-scope]");
    for (var i = 0; i < boxes.length; i++) {
      if (boxes[i].checked) lines.push("@" + boxes[i].getAttribute("data-pk-scope") + ":registry=" + registry);
    }
    lines.push("//" + host + "/npm/:_authToken=${ATELIER_TOKEN}");
    pre.textContent = lines.join("\n");
  }

  document.addEventListener("change", function (ev) {
    var t = ev.target;
    if (t && (t.hasAttribute("data-pk-scope") || t.hasAttribute("data-pk-all"))) buildNpmrc();
  });

  document.addEventListener("click", function (ev) {
    var btn = ev.target.closest && ev.target.closest("[data-pk-confirm]");
    if (btn && !window.confirm(btn.getAttribute("data-pk-confirm"))) ev.preventDefault();
  });

  document.addEventListener("DOMContentLoaded", buildNpmrc);
})();
