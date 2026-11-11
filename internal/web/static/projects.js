// Projects: progressive enhancement for boards, tables and the issue / PR
// sidebar panel. Everything works without it: forms post and redirect back.
(function () {
  "use strict";
  if (window.__projInit) return;
  window.__projInit = true;
  document.documentElement.classList.add("proj-js");

  function say(msg) {
    var live = document.querySelector("[data-proj-live]");
    if (!live) return;
    live.textContent = "";
    setTimeout(function () { live.textContent = msg; }, 30);
  }

  // Selects and inputs that save themselves.
  document.addEventListener("change", function (ev) {
    var el = ev.target;
    if (!el.matches || !el.matches("[data-proj-autosubmit]")) return;
    var form = el.form;
    if (!form) return;
    if (form.requestSubmit) form.requestSubmit(); else form.submit();
  });

  // Confirmation for destructive forms.
  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    var msg = form.getAttribute && form.getAttribute("data-proj-confirm");
    if (msg && !window.confirm(msg)) ev.preventDefault();
  });

  // The Projects panel in issue and pull request sidebars.
  function loadPanels() {
    var panels = document.querySelectorAll("[data-proj-panel]");
    Array.prototype.forEach.call(panels, function (panel) {
      var url = panel.getAttribute("data-proj-panel");
      var body = panel.querySelector("[data-proj-panel-body]");
      if (!url || !body || panel.__projLoaded) return;
      panel.__projLoaded = true;
      fetch(url + "&fragment=1", { credentials: "same-origin", headers: { "Accept": "text/html" } })
        .then(function (res) { return res.ok ? res.text() : Promise.reject(res.status); })
        .then(function (html) { body.innerHTML = html; })
        .catch(function () { /* keep the link */ });
    });
  }

  // ---------- board drag and drop ----------

  function cardsIn(list) {
    return Array.prototype.filter.call(list.children, function (el) { return el.hasAttribute("data-proj-item"); });
  }

  // Keep the move-up / move-down forms and column counts right after a drop.
  function refreshColumn(col) {
    if (!col) return;
    var list = col.querySelector("[data-proj-drop]");
    var cards = cardsIn(list);
    var count = col.querySelector("[data-proj-count]");
    if (count) count.textContent = String(cards.length);
    cards.forEach(function (card, i) {
      var prev = cards[i - 1], next = cards[i + 1];
      var before = card.querySelector('input[name="before"]');
      var after = card.querySelector('input[name="after"]');
      if (before) {
        before.value = prev ? prev.getAttribute("data-proj-item") : "0";
        before.form.querySelector("button").disabled = !prev;
      }
      if (after) {
        after.value = next ? next.getAttribute("data-proj-item") : "0";
        after.form.querySelector("button").disabled = !next;
      }
      var sel = card.querySelector('select[name="status"]');
      if (sel) sel.value = col.getAttribute("data-proj-col") || "";
    });
  }

  function setupBoard(board) {
    if (!board.hasAttribute("data-proj-can-write")) return;
    var dragged = null, origin = null, originNext = null, dropped = false;

    board.addEventListener("dragstart", function (ev) {
      var card = ev.target.closest && ev.target.closest(".proj-card[draggable='true']");
      if (!card) return;
      dragged = card;
      origin = card.parentNode;
      originNext = card.nextSibling;
      dropped = false;
      card.classList.add("is-dragging");
      ev.dataTransfer.effectAllowed = "move";
      try { ev.dataTransfer.setData("text/plain", card.getAttribute("data-proj-item")); } catch (e) { /* old browsers */ }
    });

    board.addEventListener("dragover", function (ev) {
      if (!dragged) return;
      var list = ev.target.closest && ev.target.closest("[data-proj-drop]");
      if (!list) return;
      ev.preventDefault();
      ev.dataTransfer.dropEffect = "move";
      Array.prototype.forEach.call(board.querySelectorAll("[data-proj-drop].is-over"), function (l) {
        if (l !== list) l.classList.remove("is-over");
      });
      list.classList.add("is-over");
      var after = null;
      cardsIn(list).some(function (c) {
        if (c === dragged) return false;
        var box = c.getBoundingClientRect();
        if (ev.clientY < box.top + box.height / 2) { after = c; return true; }
        return false;
      });
      if (after) {
        if (dragged.nextSibling !== after) list.insertBefore(dragged, after);
      } else if (list.lastElementChild !== dragged) {
        list.appendChild(dragged);
      }
    });

    board.addEventListener("drop", function (ev) {
      if (!dragged) return;
      var list = ev.target.closest && ev.target.closest("[data-proj-drop]");
      if (!list) return;
      ev.preventDefault();
      dropped = true;
      var card = dragged;
      var col = list.closest("[data-proj-col]");
      var next = card.nextElementSibling;
      while (next && !next.hasAttribute("data-proj-item")) next = next.nextElementSibling;
      var body = new URLSearchParams();
      body.set("status", col.getAttribute("data-proj-col") || "");
      body.set("before", next ? next.getAttribute("data-proj-item") : "0");
      var fromCol = origin && origin.closest("[data-proj-col]");
      fetch(card.getAttribute("data-proj-url") + "/move", {
        method: "POST", credentials: "same-origin", body: body,
        headers: { "Accept": "application/json", "Content-Type": "application/x-www-form-urlencoded" }
      }).then(function (res) {
        if (!res.ok) throw new Error(String(res.status));
        refreshColumn(col);
        if (fromCol && fromCol !== col) refreshColumn(fromCol);
        card.classList.add("is-moved");
        setTimeout(function () { card.classList.remove("is-moved"); }, 1200);
        var name = col.querySelector(".proj-col-head h2");
        say("Moved to " + (name ? name.textContent : "column"));
      }).catch(function () {
        say("Couldn’t move the card. Reloading.");
        window.location.reload();
      });
    });

    board.addEventListener("dragend", function () {
      if (!dragged) return;
      dragged.classList.remove("is-dragging");
      Array.prototype.forEach.call(board.querySelectorAll("[data-proj-drop].is-over"), function (l) { l.classList.remove("is-over"); });
      if (!dropped && origin) origin.insertBefore(dragged, originNext); // cancelled: put it back
      dragged = null;
    });
  }

  function init() {
    loadPanels();
    Array.prototype.forEach.call(document.querySelectorAll("[data-proj-board]"), setupBoard);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init); else init();
})();
