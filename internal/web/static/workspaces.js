// Device workspaces: fills the "Open on…" menu with the person's devices and
// follows a request's progress. Without JavaScript the menu links to the
// full page and the status shows when the page is reloaded.
(function () {
  "use strict";

  var menu = document.querySelector("[data-ws-menu]");
  if (menu) {
    var loaded = false;
    menu.addEventListener("toggle", function () {
      if (!menu.open || loaded) return;
      loaded = true;
      fetch(menu.getAttribute("data-src"), { credentials: "same-origin" })
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function (data) {
          var box = menu.querySelector("[data-ws-items]");
          var fallback = box.innerHTML;
          box.textContent = "";
          var online = (data.devices || []).filter(function (d) { return d.online; });
          if (!online.length) {
            var p = document.createElement("p");
            p.className = "dropdown-item muted";
            p.textContent = (data.devices || []).length ? "Your workspace devices are offline." : "No device has workspaces switched on.";
            box.append(p);
            box.insertAdjacentHTML("beforeend", fallback);
            return;
          }
          online.forEach(function (d) {
            var choices = [{ preset: "", label: "Clone or update" }].concat((d.presets || []).map(function (p) {
              return { preset: p, label: "Open with " + p };
            }));
            choices.forEach(function (c) {
              var form = document.createElement("form");
              form.method = "post";
              form.action = data.action;
              [["runner", d.id], ["branch", menu.getAttribute("data-branch")], ["preset", c.preset]].forEach(function (kv) {
                var i = document.createElement("input");
                i.type = "hidden";
                i.name = kv[0];
                i.value = kv[1];
                form.append(i);
              });
              var b = document.createElement("button");
              b.type = "submit";
              b.className = "dropdown-item";
              b.textContent = d.name;
              var sub = document.createElement("span");
              sub.className = "ws-item-sub";
              sub.textContent = c.label;
              b.append(sub);
              form.append(b);
              box.append(form);
            });
          });
        })
        .catch(function () { loaded = false; });
    });
  }

  var status = document.querySelector("[data-ws-status]");
  if (status && !status.hasAttribute("data-done")) {
    var tries = 0;
    var tick = function () {
      fetch(status.getAttribute("data-ws-status"), { credentials: "same-origin" })
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function (s) {
          status.querySelector("[data-ws-label]").textContent = s.label;
          status.querySelector("[data-ws-message]").textContent = s.message || "";
          var path = status.querySelector("[data-ws-path]");
          path.textContent = s.path || "";
          path.hidden = !s.path;
          status.querySelector("[data-ws-dirty]").hidden = !s.dirty;
          if (s.done) {
            status.classList.add(s.failed ? "card-danger" : "card-ok");
            return;
          }
          if (++tries < 200) setTimeout(tick, 1500);
        })
        .catch(function () { if (++tries < 200) setTimeout(tick, 3000); });
    };
    setTimeout(tick, 1000);
  }
})();
