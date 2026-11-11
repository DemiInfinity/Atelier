// Atelier: Settings → Notifications. Turns Web Push on for this device
// (permission prompt → pushManager.subscribe → POST), sends a test and turns
// it off again. No dependencies; CSP-safe. The server never puts endpoints in
// the page: rows carry a hash of theirs, and this script hashes its own to
// find "this device".
(function () {
  "use strict";

  var box = document.querySelector("[data-push-device]");
  if (!box) return;
  var statusEl = box.querySelector("[data-push-status]");
  var enableBtn = box.querySelector("[data-push-enable]");
  var testBtn = box.querySelector("[data-push-test]");
  var disableBtn = box.querySelector("[data-push-disable]");
  var errEl = document.querySelector("[data-push-error]");
  var okEl = document.querySelector("[data-push-ok]");
  if (!statusEl) return; // not a secure page; the template explains

  var ua = navigator.userAgent || "";
  var isIOS = /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1);
  var standalone = (window.matchMedia && window.matchMedia("(display-mode: standalone)").matches) || navigator.standalone === true;
  var supported = window.isSecureContext && "serviceWorker" in navigator && "PushManager" in window &&
    "Notification" in window && !!window.fetch && !!(window.crypto && window.crypto.subtle);

  var reg = null;
  var current = null; // this browser's PushSubscription
  var currentRow = null;

  function say(text) { statusEl.textContent = text; }
  function show(el, on) { if (el) el.hidden = !on; }
  function flash(el, text) {
    show(errEl, false);
    show(okEl, false);
    if (el && text) { el.textContent = text; el.hidden = false; }
  }

  function fromB64u(s) {
    s = String(s).replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    var bin = atob(s);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  }

  function hex(buf) {
    var b = new Uint8Array(buf), s = "";
    for (var i = 0; i < b.length; i++) s += (b[i] < 16 ? "0" : "") + b[i].toString(16);
    return s;
  }

  // Same as pushEndpointHash in push.go: the first 16 bytes of SHA-256, hex.
  function endpointHash(endpoint) {
    return crypto.subtle.digest("SHA-256", new TextEncoder().encode(endpoint)).then(function (d) {
      return hex(d).slice(0, 32);
    });
  }

  function postForm(url, data) {
    var body = new URLSearchParams();
    Object.keys(data).forEach(function (k) { body.set(k, data[k]); });
    return fetch(url, { method: "POST", credentials: "same-origin", body: body, headers: { "Accept": "application/json" } })
      .then(function (r) { return r.json().catch(function () { return {}; }).then(function (d) { d._ok = r.ok; return d; }); });
  }

  function render() {
    show(enableBtn, false);
    show(testBtn, false);
    show(disableBtn, false);
    document.querySelectorAll("[data-push-this]").forEach(function (el) { el.hidden = true; });
    if (!supported) {
      if (isIOS && !standalone) {
        say("To get notifications on this " + (/iPad/.test(ua) ? "iPad" : "iPhone") + ", add Atelier to your Home Screen first (see below), then open it from there.");
      } else if (isIOS) {
        say("This version of iOS can't receive web notifications. Update to iOS 16.4 or later.");
      } else {
        say("This browser can't receive web notifications.");
      }
      return;
    }
    if (Notification.permission === "denied") {
      say("Notifications are blocked for Atelier in this browser's settings. Allow them there, then reload this page.");
      return;
    }
    if (current && currentRow) {
      var label = currentRow.querySelector("strong");
      say("On. This device gets notifications" + (label ? " as “" + label.textContent + "”" : "") + ".");
      var pill = currentRow.querySelector("[data-push-this]");
      if (pill) pill.hidden = false;
      show(testBtn, true);
      show(disableBtn, true);
      return;
    }
    say(current ? "This browser is subscribed, but Atelier doesn't have it on file. Enable notifications to fix that."
      : "Off. Enable notifications to get them on this device.");
    show(enableBtn, true);
  }

  function findRow() {
    currentRow = null;
    if (!current) return Promise.resolve();
    return endpointHash(current.endpoint).then(function (h) {
      currentRow = document.querySelector('[data-push-row][data-push-hash="' + h + '"]');
    });
  }

  function refresh() {
    return reg.pushManager.getSubscription().then(function (s) {
      current = s;
      return findRow();
    }).then(render);
  }

  function subscribe() {
    var key = fromB64u(box.getAttribute("data-push-key"));
    var opts = { userVisibleOnly: true, applicationServerKey: key };
    return reg.pushManager.subscribe(opts).catch(function (err) {
      // A subscription made with another server key has to go first.
      if (err && err.name === "InvalidStateError") {
        return reg.pushManager.getSubscription().then(function (old) {
          return (old ? old.unsubscribe() : Promise.resolve()).then(function () { return reg.pushManager.subscribe(opts); });
        });
      }
      throw err;
    });
  }

  function enable() {
    flash(null);
    enableBtn.disabled = true;
    say("Asking for permission…");
    // iOS only shows the prompt when it is asked for straight from a tap.
    var asked = Notification.requestPermission();
    Promise.resolve(asked).then(function (perm) {
      if (perm !== "granted") throw new Error(perm === "denied" ? "Notifications were blocked. Allow them in this browser's settings to turn them on." : "Notifications weren't allowed.");
      say("Subscribing…");
      return subscribe();
    }).then(function (sub) {
      var j = sub.toJSON();
      return fetch("/settings/notifications/subscribe", {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/json", "Accept": "application/json" },
        body: JSON.stringify({ endpoint: j.endpoint, keys: j.keys || {}, standalone: standalone })
      }).then(function (r) {
        return r.json().catch(function () { return {}; }).then(function (d) {
          if (!r.ok) throw new Error(d.error || "Atelier couldn't save this device.");
          window.location.assign(d.redirect || "/settings/notifications");
        });
      });
    }).catch(function (err) {
      enableBtn.disabled = false;
      flash(errEl, (err && err.message) || "Couldn't turn on notifications.");
      render();
    });
  }

  function test() {
    if (!currentRow) return;
    flash(null);
    testBtn.disabled = true;
    postForm("/settings/notifications/test", { id: currentRow.getAttribute("data-push-id") }).then(function (d) {
      testBtn.disabled = false;
      if (d._ok) flash(okEl, d.message || "Test notification sent.");
      else flash(errEl, d.error || "The test didn't go through.");
    }).catch(function () {
      testBtn.disabled = false;
      flash(errEl, "The test didn't go through.");
    });
  }

  function disable() {
    flash(null);
    disableBtn.disabled = true;
    var id = currentRow && currentRow.getAttribute("data-push-id");
    var drop = current ? current.unsubscribe().catch(function () {}) : Promise.resolve();
    drop.then(function () {
      return id ? postForm("/settings/notifications/remove", { id: id }) : {};
    }).then(function (d) {
      window.location.assign((d && d.redirect) || "/settings/notifications");
    }).catch(function () {
      disableBtn.disabled = false;
      flash(errEl, "Couldn't turn notifications off.");
    });
  }

  if (enableBtn) enableBtn.addEventListener("click", enable);
  if (testBtn) testBtn.addEventListener("click", test);
  if (disableBtn) disableBtn.addEventListener("click", disable);

  if (!supported) { render(); return; }
  // pwa.js registers the worker too; registering again is harmless and makes
  // this page work on its own.
  navigator.serviceWorker.register("/sw.js", { scope: "/" })
    .then(function () { return navigator.serviceWorker.ready; })
    .then(function (r) { reg = r; return refresh(); })
    .catch(function () { say("The service worker couldn't start, so notifications can't be turned on here."); });
})();
