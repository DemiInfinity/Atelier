// Issues: progressive enhancement only (CSP script-src 'self', no inline handlers).
(function () {
  "use strict";

  // <form data-confirm="Question?"> asks before submitting (deletes).
  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    var msg = form && form.getAttribute && form.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) ev.preventDefault();
  });

  // Ctrl/Cmd+Enter in a comment box submits it as a plain comment.
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Enter" || !(ev.metaKey || ev.ctrlKey)) return;
    var ta = ev.target;
    if (!ta || ta.tagName !== "TEXTAREA" || !ta.form) return;
    var btn = ta.form.querySelector('button[value="comment"]') || ta.form.querySelector('button[type="submit"]');
    if (btn) { ev.preventDefault(); btn.click(); }
  });

  // Opening an inline editor focuses its first field.
  document.addEventListener("toggle", function (ev) {
    var d = ev.target;
    if (!d || !d.classList || !d.classList.contains("inline-edit") || !d.open) return;
    var f = d.querySelector("input:not([type=hidden]), textarea");
    if (f) f.focus();
  }, true);
})();
