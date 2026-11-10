// Actions run page: live logs and statuses over Server-Sent Events.
// Without JavaScript (or if the stream fails) the page still shows every log
// line on reload.
(function () {
  "use strict";
  var WORDS = { success: "Succeeded", failure: "Failed", cancelled: "Cancelled", skipped: "Skipped",
    running: "Running", waiting: "Waiting for approval", pending: "Waiting", queued: "Queued" };

  function setStatus(holder, status) {
    if (!holder) return;
    var mark = holder.querySelector("[data-st]");
    var text = holder.querySelector("[data-st-text]");
    if (mark) mark.className = "act-st is-" + status;
    if (text) text.textContent = WORDS[status] || status;
  }

  function each(sel, fn) {
    var els = document.querySelectorAll(sel);
    for (var i = 0; i < els.length; i++) fn(els[i]);
  }

  function nearBottom(el) { return el.scrollHeight - el.scrollTop - el.clientHeight < 40; }

  function appendLines(msg) {
    var byStep = {};
    msg.lines.forEach(function (l) {
      var key = msg.job + ":" + (l.step < 0 ? -1 : l.step);
      (byStep[key] = byStep[key] || []).push(l);
    });
    Object.keys(byStep).forEach(function (key) {
      var list = document.querySelector('[data-act-log="' + key + '"]');
      if (!list) return;
      var stick = nearBottom(list);
      var frag = document.createDocumentFragment();
      byStep[key].forEach(function (l) {
        var li = document.createElement("li");
        li.className = "is-" + (l.stream === "stderr" || l.stream === "system" ? l.stream : "stdout");
        li.textContent = l.text;
        frag.appendChild(li);
      });
      list.appendChild(frag);
      if (stick) list.scrollTop = list.scrollHeight;
    });
  }

  function applyStatus(s) {
    setStatus(document.querySelector("[data-act-run-status]"), s.run);
    (s.jobs || []).forEach(function (j) {
      setStatus(document.querySelector('[data-act-job-status="' + j.id + '"]'), j.status);
      setStatus(document.querySelector('[data-act-node="' + j.id + '"]'), j.status);
      each('[data-act-job-duration="' + j.id + '"]', function (el) { el.textContent = j.duration || ""; });
      var job = document.querySelector('[data-act-job="' + j.id + '"]');
      if (job && (j.status === "running" || j.status === "failure") && !job.open && !job.dataset.touched) job.open = true;
      (j.steps || []).forEach(function (st) {
        var key = j.id + ":" + st.index;
        setStatus(document.querySelector('[data-act-step-status="' + key + '"]'), st.status);
        var d = document.querySelector('[data-act-step-duration="' + key + '"]');
        if (d) d.textContent = st.duration || "";
        var step = document.querySelector('[data-act-step="' + key + '"]');
        if (step && st.status === "running" && !step.open && !step.dataset.touched) step.open = true;
      });
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    var root = document.querySelector("[data-act-stream]");
    // Remember when someone opens/closes a section so live updates don't fight them.
    document.addEventListener("toggle", function (ev) {
      if (ev.target && ev.target.dataset && ev.isTrusted !== false) ev.target.dataset.touched = "1";
    }, true);
    if (!root || !window.EventSource) return;
    var url = root.getAttribute("data-act-stream") + "?after=" + encodeURIComponent(root.getAttribute("data-act-after") || "0");
    var es = new EventSource(url);
    var reloadOnDone = false;
    es.addEventListener("log", function (ev) {
      try { appendLines(JSON.parse(ev.data)); } catch (e) { /* ignore a bad message */ }
    });
    es.addEventListener("status", function (ev) {
      try {
        var s = JSON.parse(ev.data);
        applyStatus(s);
        // New jobs (a matrix expanded) or approvals need the full page.
        var known = document.querySelectorAll("[data-act-job]").length;
        if ((s.jobs || []).length !== known) reloadOnDone = true;
      } catch (e) { /* ignore */ }
    });
    es.addEventListener("done", function () {
      es.close();
      var live = document.querySelector("[data-act-live]");
      if (live) live.textContent = "This run has finished.";
      if (reloadOnDone) window.location.reload();
    });
  });
})();
