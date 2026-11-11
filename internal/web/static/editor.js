// Web editor: line numbers, tab handling and Markdown preview for the file
// editor; drag-and-drop for uploads. No dependencies; works without it too.
(function () {
  "use strict";

  // ---------- text editor ----------
  var text = document.querySelector("[data-edit-text]");
  var gutter = document.querySelector("[data-edit-gutter]");
  if (text && gutter) {
    var lastCount = 0;
    var renumber = function () {
      var n = text.value.split("\n").length;
      if (n === lastCount) return;
      lastCount = n;
      var out = [];
      for (var i = 1; i <= n; i++) out.push(i);
      gutter.textContent = out.join("\n") + "\n";
    };
    renumber();
    text.addEventListener("input", renumber);
    text.addEventListener("scroll", function () { gutter.scrollTop = text.scrollTop; });

    // Indent with what the file already uses: tabs, or its smallest space indent.
    var unit = "\t";
    var m = text.value.match(/^( +)\S/m);
    if (m && !/^\t/m.test(text.value)) unit = m[1].length >= 4 ? "    " : m[1].length === 2 ? "  " : m[1];

    var insert = function (s) {
      // execCommand keeps the browser's undo history; setRangeText is the fallback.
      if (!document.execCommand || !document.execCommand("insertText", false, s)) {
        text.setRangeText(s, text.selectionStart, text.selectionEnd, "end");
        text.dispatchEvent(new Event("input"));
      }
    };
    var trapTab = true;
    text.addEventListener("keydown", function (e) {
      if (e.key === "Escape") { trapTab = false; return; }
      if (e.key !== "Tab" || !trapTab || e.ctrlKey || e.metaKey || e.altKey) return;
      e.preventDefault();
      var v = text.value, s = text.selectionStart, end = text.selectionEnd;
      var lineStart = v.lastIndexOf("\n", s - 1) + 1;
      if (!e.shiftKey && s === end) { insert(unit); return; }
      // Several lines (or Shift+Tab): indent or outdent every selected line.
      var blockEnd = v.indexOf("\n", end > s && v[end - 1] === "\n" ? end - 1 : end);
      if (blockEnd < 0) blockEnd = v.length;
      var lines = v.slice(lineStart, blockEnd).split("\n");
      var changed = lines.map(function (l) {
        if (!e.shiftKey) return unit + l;
        if (l.indexOf("\t") === 0) return l.slice(1);
        var k = 0;
        while (k < unit.length && l[k] === " ") k++;
        return l.slice(k);
      }).join("\n");
      text.setSelectionRange(lineStart, blockEnd);
      insert(changed);
      text.setSelectionRange(lineStart, lineStart + changed.length);
    });
    text.addEventListener("focus", function () { trapTab = true; });
  }

  // ---------- Markdown preview ----------
  var tabs = document.querySelector("[data-edit-tabs]");
  var surface = document.querySelector("[data-edit-surface]");
  var preview = document.querySelector("[data-edit-preview]");
  var pathInput = document.querySelector("[data-edit-path]");
  if (tabs && surface && preview && text) {
    var isMd = function () { return /\.(md|markdown)$/i.test(pathInput ? pathInput.value : ""); };
    var showTabs = function () { tabs.hidden = !isMd(); if (tabs.hidden) select("code"); };
    var select = function (which) {
      tabs.querySelectorAll("[data-edit-tab]").forEach(function (a) {
        var on = a.getAttribute("data-edit-tab") === which;
        a.setAttribute("aria-selected", on ? "true" : "false");
        if (on) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
      });
      surface.hidden = which !== "code";
      preview.hidden = which !== "preview";
    };
    tabs.addEventListener("click", function (e) {
      var a = e.target.closest("[data-edit-tab]");
      if (!a) return;
      e.preventDefault();
      var which = a.getAttribute("data-edit-tab");
      if (which === "preview") {
        preview.textContent = "Loading preview…";
        var body = new URLSearchParams();
        body.set("text", text.value);
        fetch(a.getAttribute("data-preview-url"), { method: "POST", body: body, credentials: "same-origin" })
          .then(function (r) { if (!r.ok) throw new Error(r.status); return r.text(); })
          // Server-rendered with goldmark, which escapes raw HTML and unsafe links.
          .then(function (html) { preview.innerHTML = html || "<p class=\"muted\">Nothing to preview.</p>"; })
          .catch(function () { preview.textContent = "The preview couldn't load."; });
      }
      select(which);
    });
    if (pathInput) pathInput.addEventListener("input", showTabs);
    showTabs();
  }

  // ---------- uploads ----------
  var upload = document.querySelector("[data-edit-upload]");
  if (upload) {
    var drop = upload.querySelector("[data-edit-drop]");
    var input = upload.querySelector("[data-edit-files]");
    var list = upload.querySelector("[data-edit-list]");
    var errBox = upload.querySelector("[data-edit-upload-error]");
    var maxFile = Number(upload.getAttribute("data-max-file")) * 1048576;
    var maxTotal = Number(upload.getAttribute("data-max-total")) * 1048576;
    var maxFiles = Number(upload.getAttribute("data-max-files"));
    var human = function (n) {
      if (n < 1024) return n + " B";
      if (n < 1048576) return (n / 1024).toFixed(1) + " KB";
      return (n / 1048576).toFixed(1) + " MB";
    };
    var problem = "";
    var show = function () {
      list.textContent = "";
      var total = 0;
      problem = "";
      Array.prototype.forEach.call(input.files, function (f) {
        total += f.size;
        var li = document.createElement("li");
        var name = document.createElement("span");
        name.className = "mono";
        name.textContent = f.name;
        var size = document.createElement("span");
        size.className = "small " + (f.size > maxFile ? "is-too-big" : "muted");
        size.textContent = human(f.size);
        li.append(name, size);
        list.append(li);
        if (f.size > maxFile) problem = f.name + " is larger than " + human(maxFile) + ".";
      });
      if (input.files.length > maxFiles) problem = "Upload at most " + maxFiles + " files at a time.";
      else if (total > maxTotal) problem = "That's more than " + human(maxTotal) + " in one upload.";
      list.hidden = input.files.length === 0;
      errBox.textContent = problem;
      errBox.hidden = !problem;
    };
    input.addEventListener("change", show);
    ["dragenter", "dragover"].forEach(function (t) {
      drop.addEventListener(t, function (e) { e.preventDefault(); drop.classList.add("is-over"); });
    });
    ["dragleave", "drop"].forEach(function (t) {
      drop.addEventListener(t, function () { drop.classList.remove("is-over"); });
    });
    drop.addEventListener("drop", function (e) {
      e.preventDefault();
      if (e.dataTransfer && e.dataTransfer.files.length) {
        input.files = e.dataTransfer.files;
        show();
      }
    });
    upload.addEventListener("submit", function (e) {
      if (problem) { e.preventDefault(); errBox.focus && errBox.focus(); }
    });
  }
})();
