// Atelier: Settings → Remote access. Keeps the status card live while the
// page is open (the address appears a few seconds after turning it on).
(function () {
  "use strict";
  // Show only the chosen provider's fields.
  var providers = document.querySelectorAll("[data-tn-provider]");
  function showProvider() {
    var chosen = "cloudflare";
    providers.forEach(function (el) { if (el.checked) chosen = el.value; });
    document.querySelectorAll("[data-tn-fields]").forEach(function (el) {
      el.hidden = el.getAttribute("data-tn-fields") !== chosen;
    });
  }
  providers.forEach(function (el) { el.addEventListener("change", showProvider); });
  if (providers.length) showProvider();

  var card = document.querySelector("[data-tn-status]");
  if (!card || !window.fetch) return;
  var on = card.getAttribute("data-tn-on") === "1";
  var shownAddress = card.getAttribute("data-tn-address") || "";
  var labels = { off: "Off", starting: "Starting…", connected: "Connected", restarting: "Restarting" };

  function set(sel, value) {
    var el = card.querySelector(sel);
    if (el) el.textContent = String(value);
  }

  function tick() {
    if (document.hidden) return;
    fetch("/settings/remote/status", { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (s) {
        if (!s) return;
        // A new address (or the switch flipped elsewhere): redraw the page.
        if (s.on !== on || (s.address || "") !== shownAddress) { window.location.reload(); return; }
        var phase = card.querySelector("[data-tn-phase]");
        if (phase) {
          phase.textContent = labels[s.phase] || s.phase;
          phase.className = "pill tn-phase tn-phase-" + s.phase;
        }
        set("[data-tn-inflight]", s.in_flight);
        set("[data-tn-visitors]", s.visitors);
        set("[data-tn-sessions]", s.sessions);
        set("[data-tn-conns]", s.connections);
      })
      .catch(function () { /* offline: try again next tick */ });
  }
  setInterval(tick, 3000);
})();
