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

  // Live lines arrive rendered (actions_log.go); this only places them:
  // ::group:: opens a fold, ::endgroup:: closes it, and runs of download
  // progress fold together.
  var showTimes = false;

  function localTime(t) {
    var d = new Date(t.getAttribute("datetime"));
    if (!isNaN(d)) t.textContent = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
  }

  function foldCount(fold) {
    var n = fold.querySelectorAll(":scope > .act-ln").length;
    var c = fold.querySelector(".act-fold-n");
    if (c) c.textContent = n === 1 ? "1 line" : n + " lines";
  }

  function placeRow(list, row) {
    var st = list.actState;
    if (!st) {
      st = list.actState = { n: parseInt(list.getAttribute("data-n") || "0", 10) || 0,
        group: list.querySelector(":scope > details.is-group[data-act-open]") };
    }
    st.n++;
    if (row.k === "group") {
      if (st.group) st.group.removeAttribute("data-act-open");
      list.insertAdjacentHTML("beforeend", row.f);
      st.group = list.lastElementChild;
      st.group.setAttribute("data-act-open", "");
      st.group.querySelector("summary").setAttribute("data-ln", st.n);
      return;
    }
    if (row.k === "endgroup") {
      if (st.group) st.group.removeAttribute("data-act-open");
      st.group = null;
      return;
    }
    var target = st.group || list;
    if (row.k === "pull" && !st.group) {
      var last = list.lastElementChild;
      if (last && last.matches("details.is-pull")) {
        target = last;
      } else if (last && last.matches(".act-ln.is-pull")) {
        list.insertAdjacentHTML("beforeend", row.f);
        target = list.lastElementChild;
        target.appendChild(last);
      }
    }
    target.insertAdjacentHTML("beforeend", row.h);
    var el = target.lastElementChild;
    el.setAttribute("data-ln", st.n);
    var ts = showTimes && el.querySelector("time.act-ts");
    if (ts) localTime(ts);
    if (target !== list) {
      foldCount(target);
      if (row.k === "error") target.open = true;
      var title = el.getAttribute("data-title");
      var t = target.querySelector(".act-fold-t");
      if (title && t && target.matches(".is-pull")) t.textContent = "Pulled image " + title;
    }
  }

  function appendLines(msg) {
    var rows = msg.rows || [];
    var byList = {};
    rows.forEach(function (r) {
      var key = msg.job + ":" + (r.step < 0 ? -1 : r.step);
      (byList[key] = byList[key] || []).push(r);
    });
    Object.keys(byList).forEach(function (key) {
      var list = document.querySelector('[data-act-log="' + key + '"]');
      if (!list) return;
      var stick = nearBottom(list);
      byList[key].forEach(function (r) { placeRow(list, r); });
      if (stick) list.scrollTop = list.scrollHeight;
    });
  }

  // "Jump to the error": open every fold around the line, then show it.
  function reveal(id) {
    var el = id && document.getElementById(id);
    if (!el) return false;
    for (var p = el; p; p = p.parentElement) if (p.tagName === "DETAILS") p.open = true;
    el.scrollIntoView({ block: "center" });
    each(".act-ln.is-target", function (x) { x.classList.remove("is-target"); });
    if (el.classList.contains("act-ln")) el.classList.add("is-target");
    return true;
  }

  function setTimes(on) {
    showTimes = on;
    document.documentElement.classList.toggle("act-show-ts", on);
    if (on) each("time.act-ts", localTime);
    each("[data-act-ts]", function (b) {
      b.setAttribute("aria-pressed", on ? "true" : "false");
      b.textContent = on ? "Hide times" : "Show times";
    });
    try { localStorage.setItem("atelier.actTimes", on ? "1" : ""); } catch (e) { /* private mode */ }
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
    each("[data-act-ts]", function (b) {
      b.hidden = false;
      b.addEventListener("click", function () { setTimes(!showTimes); });
    });
    var saved = false;
    try { saved = localStorage.getItem("atelier.actTimes") === "1"; } catch (e) { /* private mode */ }
    if (saved) setTimes(true);
    document.addEventListener("click", function (ev) {
      var a = ev.target.closest && ev.target.closest("[data-act-jump]");
      if (!a) return;
      var id = a.getAttribute("href").slice(1);
      if (reveal(id)) {
        ev.preventDefault();
        if (history.replaceState) history.replaceState(null, "", "#" + id);
      }
    });
    if (location.hash) reveal(decodeURIComponent(location.hash.slice(1)));
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
