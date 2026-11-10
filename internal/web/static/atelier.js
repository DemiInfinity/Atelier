// Atelier: progressive enhancement only. No dependencies, no inline handlers (CSP script-src 'self').
(function () {
  "use strict";

  // Live region for screen-reader feedback.
  var live = document.createElement("div");
  live.className = "sr-only";
  live.setAttribute("aria-live", "polite");
  live.setAttribute("role", "status");
  document.addEventListener("DOMContentLoaded", function () { document.body.appendChild(live); });

  function announce(msg) {
    live.textContent = "";
    setTimeout(function () { live.textContent = msg; }, 30);
  }

  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text);
    }
    return new Promise(function (resolve, reject) {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.className = "sr-only";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy") ? resolve() : reject(); } catch (e) { reject(e); }
      document.body.removeChild(ta);
    });
  }

  document.addEventListener("click", function (ev) {
    // Copy buttons: <button data-copy="#input-id">
    var btn = ev.target.closest && ev.target.closest("[data-copy]");
    if (btn) {
      var target = document.querySelector(btn.getAttribute("data-copy"));
      if (target) {
        var value = "value" in target ? target.value : target.textContent;
        copyText(value).then(function () {
          btn.classList.add("is-copied");
          announce("Copied to clipboard");
          clearTimeout(btn._copyTimer);
          btn._copyTimer = setTimeout(function () { btn.classList.remove("is-copied"); }, 1600);
        }, function () {
          if (target.select) { target.focus(); target.select(); }
          announce("Press Control or Command C to copy");
        });
      }
    }

    // Close open dropdowns when clicking outside them.
    var openMenus = document.querySelectorAll("details.dropdown[open]");
    for (var i = 0; i < openMenus.length; i++) {
      if (!openMenus[i].contains(ev.target)) openMenus[i].removeAttribute("open");
    }
  });

  // Escape closes an open dropdown and returns focus to its summary.
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape") return;
    var d = document.querySelector("details.dropdown[open]");
    if (d) {
      d.removeAttribute("open");
      var s = d.querySelector("summary");
      if (s) s.focus();
    }
  });
})();
