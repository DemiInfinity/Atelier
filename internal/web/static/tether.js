// Fills the "Tether this repo" card on a repository's code page from
// /{owner}/{repo}/tether/status. Text only: nothing from the server is
// inserted as HTML.
(function () {
  "use strict";
  var card = document.querySelector("[data-tether-card]");
  if (!card || !window.fetch) return;
  var src = card.getAttribute("data-src");
  if (!src) return;
  fetch(src, { credentials: "same-origin", headers: { Accept: "application/json" } })
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (s) {
      if (!s) return;
      var text = card.querySelector("[data-tether-text]");
      var link = card.querySelector("[data-tether-link]");
      var gh = card.querySelector("[data-tether-gh]");
      if (s.linked) {
        if (text) {
          text.textContent = "Linked to " + s.gh_repo + " on GitHub · Home: " + (s.home === "github" ? "GitHub" : "Atelier") +
            (s.diverged ? " · the default branches differ" : "") + (s.problem ? " · last sync had a problem" : "");
        }
        if (gh && typeof s.url === "string" && s.url.indexOf("https://") === 0) {
          gh.href = s.url;
          gh.hidden = false;
        }
        if (link) link.textContent = "Tether settings →";
      }
      if (link) {
        if (s.settings_url) link.href = s.settings_url;
        else link.hidden = true;
      }
    })
    .catch(function () {});
})();
