// Atelier ⌘K command palette. No dependencies, no inline handlers (CSP).
// Opens on ⌘K / Ctrl+K or any [data-pal-open] button; results come from
// GET /-/palette?q=&repo=. The dialog is built on first open.
(function () {
  "use strict";

  var GLYPHS = { repo: "A", file: "/", issue: "#", pull: "⇄", snippet: "{}", command: "›", account: "@", github: "↗" };
  // First path segments that are pages, not accounts (keep in step with the
  // server's reserved names; the server ignores anything it can't read anyway).
  var NOT_REPO = { "-": 1, snippets: 1, settings: 1, search: 1, static: 1, new: 1, accounts: 1, notifications: 1,
    packages: 1, api: 1, npm: 1, explore: 1, signin: 1, setup: 1, auth: 1, mcp: 1, runners: 1, actions: 1, lfs: 1 };

  var root, dialog, input, list, status, lastFocus, items = [], active = -1, timer, ctl, seq = 0, open = false;

  function currentRepo() {
    var parts = location.pathname.split("/").filter(Boolean);
    if (parts.length < 2 || NOT_REPO[parts[0]]) return "";
    return decodeURIComponent(parts[0]) + "/" + decodeURIComponent(parts[1].replace(/\.git$/, ""));
  }

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  function build() {
    root = el("div", "pal-root");
    root.hidden = true;
    var backdrop = el("div", "pal-backdrop");
    backdrop.addEventListener("mousedown", function (ev) { ev.preventDefault(); close(); });
    dialog = el("div", "pal-dialog");
    dialog.setAttribute("role", "dialog");
    dialog.setAttribute("aria-modal", "true");
    dialog.setAttribute("aria-label", "Command palette");

    var bar = el("div", "pal-bar");
    var icon = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    icon.setAttribute("width", "18"); icon.setAttribute("height", "18"); icon.setAttribute("viewBox", "0 0 24 24");
    icon.setAttribute("fill", "none"); icon.setAttribute("stroke", "currentColor"); icon.setAttribute("stroke-width", "2");
    icon.setAttribute("aria-hidden", "true"); icon.setAttribute("class", "pal-icon");
    icon.innerHTML = '<circle cx="11" cy="11" r="7"></circle><path d="M20 20 L16 16"></path>';
    var label = el("label", "sr-only", "Search or run a command");
    label.htmlFor = "pal-input";
    input = el("input", "pal-input");
    input.id = "pal-input";
    input.type = "text";
    input.autocomplete = "off";
    input.spellcheck = false;
    input.setAttribute("placeholder", "Type a command or search…");
    input.setAttribute("role", "combobox");
    input.setAttribute("aria-expanded", "false");
    input.setAttribute("aria-controls", "pal-list");
    input.setAttribute("aria-autocomplete", "list");
    input.setAttribute("aria-describedby", "pal-help");
    var esc = el("button", "pal-esc", "esc");
    esc.type = "button";
    esc.setAttribute("aria-label", "Close command palette");
    esc.addEventListener("click", close);
    bar.appendChild(icon); bar.appendChild(label); bar.appendChild(input); bar.appendChild(esc);

    list = el("div", "pal-list");
    list.id = "pal-list";
    list.setAttribute("role", "listbox");
    list.setAttribute("aria-label", "Results");
    status = el("p", "pal-status");
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");

    var foot = el("div", "pal-foot");
    foot.id = "pal-help";
    foot.innerHTML = "<span><kbd>↑</kbd><kbd>↓</kbd> move</span><span><kbd>↵</kbd> open</span><span><kbd>esc</kbd> close</span>" +
      '<span class="pal-modes"><kbd>&gt;</kbd> commands · <kbd>#</kbd> issues · <kbd>/</kbd> files · <kbd>@</kbd> accounts</span>';

    dialog.appendChild(bar); dialog.appendChild(list); dialog.appendChild(status); dialog.appendChild(foot);
    root.appendChild(backdrop); root.appendChild(dialog);
    document.body.appendChild(root);

    input.addEventListener("input", schedule);
    input.addEventListener("keydown", onKey);
    dialog.addEventListener("keydown", trap);
    list.addEventListener("mousedown", function (ev) { ev.preventDefault(); }); // keep focus in the input
    list.addEventListener("click", function (ev) {
      var opt = ev.target.closest("[role=option]");
      if (opt) go(+opt.getAttribute("data-i"), ev.metaKey || ev.ctrlKey);
    });
    list.addEventListener("mousemove", function (ev) {
      var opt = ev.target.closest("[role=option]");
      if (opt && +opt.getAttribute("data-i") !== active) setActive(+opt.getAttribute("data-i"), false);
    });
  }

  function show() {
    if (!root) build();
    if (open) { input.select(); return; }
    open = true;
    lastFocus = document.activeElement;
    root.hidden = false;
    document.documentElement.classList.add("pal-open");
    input.value = "";
    input.focus();
    fetchResults();
  }

  function close() {
    if (!open) return;
    open = false;
    root.hidden = true;
    document.documentElement.classList.remove("pal-open");
    input.setAttribute("aria-expanded", "false");
    input.removeAttribute("aria-activedescendant");
    if (ctl) ctl.abort();
    if (lastFocus && lastFocus !== document.body && lastFocus.focus) lastFocus.focus();
    else input.blur();
  }

  // Focus trap: Tab cycles between the input and the close button.
  function trap(ev) {
    if (ev.key !== "Tab") return;
    var f = [input, dialog.querySelector(".pal-esc")];
    var i = f.indexOf(document.activeElement);
    ev.preventDefault();
    f[(i + (ev.shiftKey ? f.length - 1 : 1)) % f.length].focus();
  }

  function schedule() {
    clearTimeout(timer);
    timer = setTimeout(fetchResults, 70);
  }

  function fetchResults() {
    var my = ++seq;
    if (ctl) ctl.abort();
    ctl = window.AbortController ? new AbortController() : null;
    var url = "/-/palette?q=" + encodeURIComponent(input.value);
    var repo = currentRepo();
    if (repo) url += "&repo=" + encodeURIComponent(repo);
    fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" }, signal: ctl ? ctl.signal : undefined })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (data) { if (my === seq) render(data); })
      .catch(function (e) {
        if (my !== seq || (e && e.name === "AbortError")) return;
        items = []; list.textContent = ""; status.textContent = "Couldn't load results.";
      });
  }

  function render(data) {
    items = data.items || [];
    list.textContent = "";
    var group = null, box = null;
    items.forEach(function (it, i) {
      if (it.group !== group) {
        group = it.group;
        box = el("div", "pal-group");
        box.setAttribute("role", "group");
        var head = el("div", "pal-group-head", group);
        head.id = "pal-g-" + i;
        head.setAttribute("aria-hidden", "true");
        box.setAttribute("aria-labelledby", head.id);
        box.appendChild(head);
        list.appendChild(box);
      }
      var opt = el("div", "pal-item pal-" + it.kind);
      opt.id = "pal-opt-" + i;
      opt.setAttribute("role", "option");
      opt.setAttribute("aria-selected", "false");
      opt.setAttribute("data-i", i);
      var glyph = opt.appendChild(el("span", "pal-glyph", it.avatar ? "" : (GLYPHS[it.kind] || "•")));
      glyph.setAttribute("aria-hidden", "true");
      if (it.avatar && it.avatar.indexOf("/-/avatar/") === 0) {
        var pic = document.createElement("img");
        pic.src = it.avatar; pic.alt = ""; pic.width = 18; pic.height = 18; pic.className = "pal-pic";
        glyph.appendChild(pic);
      }
      opt.appendChild(el("span", "pal-title", it.title));
      if (it.hint) opt.appendChild(el("span", "pal-hint", it.hint));
      box.appendChild(opt);
    });
    input.setAttribute("aria-expanded", items.length ? "true" : "false");
    status.textContent = items.length ? items.length + (items.length === 1 ? " result" : " results") : (data.empty || "No results.");
    status.classList.toggle("pal-status-empty", !items.length);
    setActive(items.length ? 0 : -1, true);
  }

  function setActive(i, scroll) {
    var prev = list.querySelector("[aria-selected=true]");
    if (prev) prev.setAttribute("aria-selected", "false");
    active = i;
    if (i < 0) { input.removeAttribute("aria-activedescendant"); return; }
    var opt = document.getElementById("pal-opt-" + i);
    opt.setAttribute("aria-selected", "true");
    input.setAttribute("aria-activedescendant", opt.id);
    if (scroll) opt.scrollIntoView({ block: "nearest" });
  }

  function go(i, newTab) {
    var it = items[i];
    if (!it) return;
    if (newTab) { window.open(it.url, "_blank", "noopener"); return; }
    close();
    location.href = it.url;
  }

  function onKey(ev) {
    switch (ev.key) {
      case "ArrowDown":
        ev.preventDefault();
        if (items.length) setActive((active + 1) % items.length, true);
        break;
      case "ArrowUp":
        ev.preventDefault();
        if (items.length) setActive((active - 1 + items.length) % items.length, true);
        break;
      case "Home":
        if (items.length && ev.ctrlKey) { ev.preventDefault(); setActive(0, true); }
        break;
      case "End":
        if (items.length && ev.ctrlKey) { ev.preventDefault(); setActive(items.length - 1, true); }
        break;
      case "Enter":
        ev.preventDefault();
        if (active >= 0) go(active, ev.metaKey || ev.ctrlKey);
        break;
      case "Escape":
        ev.preventDefault();
        ev.stopPropagation();
        close();
        break;
    }
  }

  document.addEventListener("keydown", function (ev) {
    if ((ev.metaKey || ev.ctrlKey) && !ev.altKey && !ev.shiftKey && (ev.key === "k" || ev.key === "K")) {
      ev.preventDefault();
      if (open) close(); else show();
    } else if (open && ev.key === "Escape") {
      close();
    }
  });
  document.addEventListener("click", function (ev) {
    var b = ev.target.closest && ev.target.closest("[data-pal-open]");
    if (b) { ev.preventDefault(); show(); }
  });
})();
