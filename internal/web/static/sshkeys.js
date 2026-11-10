// Atelier: HTTPS/SSH switch for clone boxes. Progressive enhancement: without
// this script (or with SSH off) the box shows the HTTPS URL as before.
(function () {
  "use strict";
  if (window.__atelierCloneProto) return; // the partial may include this script more than once
  window.__atelierCloneProto = true;

  var PREF = "atelier.clone-protocol";

  function pref(v) {
    try {
      if (v) localStorage.setItem(PREF, v);
      return localStorage.getItem(PREF) || "https";
    } catch (e) { return v || "https"; }
  }

  function boxes() { return document.querySelectorAll(".clone[data-clone-https]"); }

  function apply(proto) {
    var list = boxes();
    for (var i = 0; i < list.length; i++) {
      var box = list[i];
      var ssh = box.getAttribute("data-clone-ssh");
      var use = proto === "ssh" && ssh ? "ssh" : "https";
      var input = box.querySelector(".clone-url");
      if (input) input.value = use === "ssh" ? ssh : box.getAttribute("data-clone-https");
      var btns = box.querySelectorAll("[data-clone-proto]");
      for (var j = 0; j < btns.length; j++) {
        btns[j].setAttribute("aria-pressed", btns[j].getAttribute("data-clone-proto") === use ? "true" : "false");
      }
    }
  }

  function ensureStyles() {
    if (document.querySelector('link[href="/static/sshkeys.css"]')) return;
    var l = document.createElement("link");
    l.rel = "stylesheet";
    l.href = "/static/sshkeys.css";
    document.head.appendChild(l);
  }

  function init() {
    var list = boxes();
    if (!list.length || !window.fetch) return;
    fetch("/settings/keys/clone.json", { credentials: "same-origin", headers: { Accept: "application/json" } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (info) {
        if (!info || !info.enabled || !info.base) return;
        ensureStyles();
        for (var i = 0; i < list.length; i++) {
          var path;
          try { path = new URL(list[i].getAttribute("data-clone-https"), location.href).pathname; } catch (e) { continue; }
          list[i].setAttribute("data-clone-ssh", info.base + path);
          var group = list[i].querySelector(".clone-proto");
          if (group) group.hidden = false;
        }
        apply(pref());
      })
      .catch(function () { /* keep HTTPS only */ });

    document.addEventListener("click", function (ev) {
      var btn = ev.target.closest && ev.target.closest("[data-clone-proto]");
      if (!btn) return;
      apply(pref(btn.getAttribute("data-clone-proto")));
    });
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
