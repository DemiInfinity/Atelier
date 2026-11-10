// Releases: "Generate notes" and delete confirmations. Progressive enhancement only (CSP script-src 'self').
(function () {
  "use strict";

  function init() {
    var form = document.querySelector("[data-rel-form]");
    if (form) {
      var btn = form.querySelector("[data-rel-notes]");
      var status = form.querySelector("[data-rel-notes-status]");
      var body = form.querySelector("#rel-body");
      if (btn && body && window.fetch) {
        btn.hidden = false;
        btn.addEventListener("click", function () {
          if (body.value.trim() !== "" && !window.confirm("Replace the notes you've written with generated ones?")) return;
          var data = new URLSearchParams();
          data.set("tag", form.querySelector("#rel-tag").value);
          data.set("target", form.querySelector("#rel-target").value);
          btn.disabled = true;
          status.textContent = "Drafting notes…";
          fetch(form.getAttribute("data-notes-url"), {
            method: "POST",
            credentials: "same-origin",
            headers: { "Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json" },
            body: data.toString()
          }).then(function (res) {
            return res.json().then(function (j) { return { ok: res.ok, j: j }; });
          }).then(function (r) {
            if (!r.ok) throw new Error(r.j.message || "Couldn't generate notes.");
            body.value = r.j.body;
            status.textContent = r.j.previous_tag ? "Notes drafted from changes since " + r.j.previous_tag + ". Edit them as you like." : "Notes drafted from the history so far. Edit them as you like.";
            body.focus();
          }).catch(function (e) {
            status.textContent = e.message || "Couldn't generate notes.";
          }).then(function () { btn.disabled = false; });
        });
      }
    }
    document.addEventListener("click", function (ev) {
      var b = ev.target.closest && ev.target.closest("[data-rel-confirm]");
      if (b && !window.confirm(b.getAttribute("data-rel-confirm"))) ev.preventDefault();
    });
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
