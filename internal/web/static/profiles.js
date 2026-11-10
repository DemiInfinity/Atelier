// Atelier profiles: progressive enhancement only. Without this script the
// repository search and sort are plain GET forms and the pin picker uses
// submit buttons.
(function () {
  "use strict";

  function init() {
    // ---------- profile page: live repository filter ----------
    var form = document.querySelector("[data-prof-filter]");
    var list = document.querySelector("[data-prof-list]");
    if (form) {
      var sortSel = form.querySelector("[data-prof-sort]");
      if (sortSel) sortSel.addEventListener("change", function () { form.submit(); });
    }
    if (form && list) {
      var q = form.querySelector("[data-prof-q]");
      var count = document.querySelector("[data-prof-count]");
      var none = document.querySelector("[data-prof-none]");
      var rows = Array.prototype.slice.call(list.querySelectorAll("[data-name]"));
      var initial = q.value;
      q.addEventListener("input", function () {
        var term = q.value.trim().toLowerCase();
        var shown = 0;
        rows.forEach(function (row) {
          var hay = (row.getAttribute("data-name") + " " + row.getAttribute("data-desc")).toLowerCase();
          var hit = !term || hay.indexOf(term) !== -1;
          row.hidden = !hit;
          if (hit) shown++;
        });
        if (none) none.hidden = shown !== 0;
        if (count && q.value !== initial) {
          count.textContent = term ? shown + (shown === 1 ? " result" : " results") : "";
        }
      });
    }

    // ---------- edit page: pin limit ----------
    var pins = document.querySelector("[data-prof-pins]");
    if (pins) {
      var max = parseInt(pins.getAttribute("data-max"), 10) || 6;
      var label = pins.querySelector("[data-prof-pin-count]");
      var boxes = Array.prototype.slice.call(pins.querySelectorAll('input[type="checkbox"][name="pin"]'));
      var update = function () {
        var n = boxes.filter(function (b) { return b.checked; }).length;
        if (label) label.textContent = n + " of " + max + " pinned";
        boxes.forEach(function (b) {
          var block = !b.checked && n >= max;
          b.disabled = block;
          var row = b.closest(".prof-pin-row");
          if (row) row.classList.toggle("is-over", block);
        });
      };
      boxes.forEach(function (b) { b.addEventListener("change", update); });
      update();
    }
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
