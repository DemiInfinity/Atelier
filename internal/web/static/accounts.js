// The top bar's account menu (<details data-acct-menu>): close it on Escape
// or a click outside. It opens and works without this file.
(function () {
  "use strict";
  document.addEventListener("click", function (e) {
    document.querySelectorAll("details[data-acct-menu][open]").forEach(function (d) {
      if (!d.contains(e.target)) d.removeAttribute("open");
    });
  });
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    document.querySelectorAll("details[data-acct-menu][open]").forEach(function (d) {
      d.removeAttribute("open");
      var s = d.querySelector("summary");
      if (s) s.focus();
    });
  });
})();
