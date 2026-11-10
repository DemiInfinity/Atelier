// Pull requests: progressive enhancement only. Every action also works as a
// plain form or link without this script (CSP script-src 'self', no inline handlers).
(function () {
  "use strict";

  var seq = 0;

  // Compare page: re-run the comparison when a branch is picked.
  document.addEventListener("change", function (ev) {
    var sel = ev.target;
    if (sel.tagName !== "SELECT") return;
    var form = sel.closest("form[data-pr-autosubmit]");
    if (form && form.querySelector("#pr-head").value) form.submit();
  });

  function closeOpenForms() {
    var rows = document.querySelectorAll("tr.pr-js-note-row");
    for (var i = 0; i < rows.length; i++) {
      var ta = rows[i].querySelector("textarea");
      if (ta && ta.value.trim() !== "") continue; // keep drafts
      rows[i].parentNode.removeChild(rows[i]);
    }
  }

  document.addEventListener("click", function (ev) {
    // "+" beside a diff line: open a note form under it without reloading.
    var add = ev.target.closest && ev.target.closest("a.pr-note-add");
    var tpl = document.getElementById("pr-note-template");
    if (add && tpl && tpl.content) {
      ev.preventDefault();
      var row = add.closest("tr");
      var next = row.nextElementSibling;
      while (next && next.classList.contains("pr-notes-row") && !next.classList.contains("pr-js-note-row")) {
        next = next.nextElementSibling;
      }
      if (next && next.classList.contains("pr-js-note-row") && next.getAttribute("data-for") === row.id) {
        next.querySelector("textarea").focus();
        return;
      }
      closeOpenForms();
      var form = tpl.content.firstElementChild.cloneNode(true);
      form.querySelector('input[name="path"]').value = add.getAttribute("data-path");
      form.querySelector('input[name="side"]').value = add.getAttribute("data-side");
      form.querySelector('input[name="line"]').value = add.getAttribute("data-line");
      var lineLabel = form.querySelector(".pr-note-line");
      if (lineLabel) lineLabel.textContent = add.getAttribute("data-line");
      var id = "pr-note-js-" + (++seq);
      var ta = form.querySelector("textarea");
      var label = form.querySelector("label");
      ta.id = id;
      if (label) label.setAttribute("for", id);

      var tr = document.createElement("tr");
      tr.className = "pr-notes-row pr-js-note-row";
      tr.setAttribute("data-for", row.id);
      var td = document.createElement("td");
      td.colSpan = 3;
      td.appendChild(form);
      tr.appendChild(td);
      // After the line and any notes already on it.
      var after = row;
      while (after.nextElementSibling && after.nextElementSibling.classList.contains("pr-notes-row")) {
        after = after.nextElementSibling;
      }
      after.parentNode.insertBefore(tr, after.nextSibling);
      ta.focus();
      return;
    }

    // Cancel in a script-made form just removes it.
    var cancel = ev.target.closest && ev.target.closest(".pr-js-note-row .pr-note-cancel");
    if (cancel) {
      ev.preventDefault();
      var r = cancel.closest("tr");
      var line = document.getElementById(r.getAttribute("data-for"));
      r.parentNode.removeChild(r);
      var btn = line && line.querySelector(".pr-note-add");
      if (btn) btn.focus();
    }
  });

  // Ctrl/Cmd+Enter submits a note or comment; Escape cancels a new note.
  document.addEventListener("keydown", function (ev) {
    var ta = ev.target;
    if (ta.tagName !== "TEXTAREA") return;
    if (ev.key === "Enter" && (ev.metaKey || ev.ctrlKey) && ta.form) {
      ev.preventDefault();
      if (ta.form.requestSubmit) ta.form.requestSubmit(); else ta.form.submit();
    } else if (ev.key === "Escape" && ta.closest(".pr-js-note-row") && ta.value.trim() === "") {
      var c = ta.closest(".pr-js-note-row").querySelector(".pr-note-cancel");
      if (c) c.click();
    }
  });
})();
