// Top-bar bell: fills in the unread count. Progressive enhancement only;
// without JS the bell is a plain link to /notifications.
(function () {
  "use strict";
  function update() {
    var bell = document.querySelector("[data-nf-bell]");
    if (!bell || !window.fetch) return;
    fetch("/notifications/count", { credentials: "same-origin", headers: { "Accept": "application/json" } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (d) {
        if (!d) return;
        var badge = bell.querySelector("[data-nf-count]");
        if (badge) {
          badge.textContent = d.label || "";
          badge.hidden = !d.label;
        }
        bell.setAttribute("aria-label", d.unread > 0 ? "Notifications, " + d.unread + " unread" : "Notifications");
      })
      .catch(function () {});
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", update);
  } else {
    update();
  }
  // Refresh when the tab comes back into view.
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible") update();
  });
})();
