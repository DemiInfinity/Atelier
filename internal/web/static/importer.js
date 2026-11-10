// Import page: live job status and the repository picker's filter/select-all.
// Progressive enhancement only; everything works without it.
(function () {
  "use strict";

  function onReady(fn) {
    if (document.readyState !== "loading") fn();
    else document.addEventListener("DOMContentLoaded", fn);
  }

  // ---------- job status polling ----------
  function pending(list) {
    return list.querySelector('[data-status="queued"], [data-status="running"]') !== null;
  }

  var mirrorAdded = false;

  function update(list, jobs) {
    var live = document.getElementById("im-live");
    jobs.forEach(function (j) {
      var row = list.querySelector('[data-job-id="' + j.id + '"]');
      if (!row || row.getAttribute("data-status") === j.status) {
        if (row) row.querySelector(".im-msg").textContent = j.message;
        return;
      }
      row.setAttribute("data-status", j.status);
      var pill = row.querySelector(".im-status");
      pill.className = "pill im-status im-" + j.status;
      pill.textContent = j.status;
      var msg = row.querySelector(".im-msg");
      msg.textContent = j.message;
      msg.className = "small im-msg " + (j.status === "failed" ? "danger-ink" : "muted");
      if (j.url) {
        var name = row.querySelector(".im-repo");
        var a = document.createElement("a");
        a.className = "im-repo";
        a.href = j.url;
        a.textContent = j.repo;
        name.replaceWith(a);
      }
      if (j.status === "done" && row.querySelector(".im-mirror-pill")) mirrorAdded = true;
      if (live && (j.status === "done" || j.status === "failed")) {
        live.textContent = j.repo + (j.status === "done" ? " imported" : " failed: " + j.message);
      }
    });
  }

  function poll(list) {
    if (!pending(list)) return;
    fetch("/settings/import/jobs", { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then(function (r) { return r.ok ? r.json() : Promise.reject(r.status); })
      .then(function (jobs) { update(list, jobs); })
      .catch(function () {})
      .then(function () {
        if (pending(list)) setTimeout(function () { poll(list); }, 2000);
        // A new mirror belongs in the Mirrors list below; reload once to show it.
        else if (mirrorAdded) window.location.replace("/settings/import");
      });
  }

  // ---------- picker ----------
  function picker(form) {
    var filter = form.querySelector("[data-pick-filter]");
    var all = form.querySelector("[data-pick-all]");
    var rows = Array.prototype.slice.call(form.querySelectorAll(".ip-row"));
    function visible() { return rows.filter(function (r) { return !r.hidden; }); }
    if (filter) {
      filter.addEventListener("input", function () {
        var q = filter.value.trim().toLowerCase();
        rows.forEach(function (r) { r.hidden = q !== "" && r.getAttribute("data-name").toLowerCase().indexOf(q) < 0; });
        if (all) all.checked = false;
      });
    }
    if (all) {
      all.addEventListener("change", function () {
        visible().forEach(function (r) { r.querySelector('input[type="checkbox"]').checked = all.checked; });
      });
    }
  }

  onReady(function () {
    var list = document.querySelector("[data-import-jobs]");
    if (list) setTimeout(function () { poll(list); }, 1500);
    var form = document.querySelector("[data-import-pick]");
    if (form) picker(form);
  });
})();
