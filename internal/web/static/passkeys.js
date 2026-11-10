// Atelier: passkeys (WebAuthn). Registration on Settings → Passkeys and
// "Sign in with a passkey" on the sign-in page. No dependencies; CSP-safe.
(function () {
  "use strict";

  var supported = !!(window.PublicKeyCredential && navigator.credentials && window.isSecureContext && window.fetch);

  // ---------- base64url <-> ArrayBuffer ----------

  function fromB64u(s) {
    s = String(s).replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    var bin = atob(s);
    var out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  }

  function toB64u(buf) {
    if (!buf) return null;
    var bytes = new Uint8Array(buf);
    var s = "";
    for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function decodeCreation(pk) {
    pk.challenge = fromB64u(pk.challenge);
    pk.user.id = fromB64u(pk.user.id);
    (pk.excludeCredentials || []).forEach(function (c) { c.id = fromB64u(c.id); });
    return pk;
  }

  function decodeRequest(pk) {
    pk.challenge = fromB64u(pk.challenge);
    (pk.allowCredentials || []).forEach(function (c) { c.id = fromB64u(c.id); });
    return pk;
  }

  function encodeCredential(cred) {
    var r = cred.response;
    var resp = { clientDataJSON: toB64u(r.clientDataJSON) };
    if (r.attestationObject) {
      resp.attestationObject = toB64u(r.attestationObject);
      if (r.getTransports) { try { resp.transports = r.getTransports(); } catch (e) { /* optional */ } }
    } else {
      resp.authenticatorData = toB64u(r.authenticatorData);
      resp.signature = toB64u(r.signature);
      if (r.userHandle) resp.userHandle = toB64u(r.userHandle);
    }
    var out = {
      id: cred.id,
      rawId: toB64u(cred.rawId),
      type: cred.type,
      response: resp,
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
    };
    if (cred.authenticatorAttachment) out.authenticatorAttachment = cred.authenticatorAttachment;
    return out;
  }

  // ---------- requests ----------

  function post(url, body) {
    return fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: body ? JSON.stringify(body) : "{}"
    }).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) throw new Error(data.error || "Something went wrong. Please try again.");
        return data;
      });
    });
  }

  function showError(msg) {
    var el = document.querySelector("[data-passkey-error]");
    if (!el) return;
    el.textContent = msg || "";
    el.hidden = !msg;
  }

  function friendly(err) {
    if (err && (err.name === "NotAllowedError" || err.name === "AbortError")) {
      return "The passkey request was cancelled or timed out.";
    }
    if (err && err.name === "InvalidStateError") {
      return "This device already has a passkey for Atelier.";
    }
    if (err && err.name === "SecurityError") {
      return "This address can't use passkeys. Open Atelier at its https:// address.";
    }
    return (err && err.message) || "Something went wrong. Please try again.";
  }

  // ---------- registration (settings) ----------

  function initRegister(form) {
    var btn = form.querySelector("[data-passkey-submit]");
    if (!supported) {
      var hint = form.querySelector("[data-passkey-hint]");
      if (hint) hint.textContent = "This browser doesn't support passkeys here. Try a current browser on an https:// address.";
      return;
    }
    if (btn) btn.disabled = false;
    form.addEventListener("submit", function (ev) {
      ev.preventDefault();
      showError("");
      var name = (form.querySelector("[name=name]") || {}).value || "";
      if (btn) { btn.disabled = true; btn.setAttribute("aria-busy", "true"); }
      post("/settings/passkeys/register/begin")
        .then(function (opts) { return navigator.credentials.create({ publicKey: decodeCreation(opts.publicKey) }); })
        .then(function (cred) { return post("/settings/passkeys/register/finish", { name: name, credential: encodeCredential(cred) }); })
        .then(function (res) { window.location.assign(res.redirect || "/settings/passkeys?saved=1"); })
        .catch(function (err) {
          showError(friendly(err));
          if (btn) { btn.disabled = false; btn.removeAttribute("aria-busy"); }
        });
    });
  }

  // ---------- sign in ----------

  function initLogin(btn) {
    var wrap = document.querySelector("[data-passkey-signin]");
    var or = document.querySelector("[data-passkey-or]");
    if (!supported) {
      var note = document.querySelector("[data-passkey-note]");
      if (note) note.hidden = false;
      return;
    }
    if (wrap) wrap.hidden = false;
    if (or) or.hidden = false;
    btn.addEventListener("click", function () {
      showError("");
      btn.disabled = true;
      btn.setAttribute("aria-busy", "true");
      post("/auth/passkey/begin")
        .then(function (opts) { return navigator.credentials.get({ publicKey: decodeRequest(opts.publicKey) }); })
        .then(function (cred) {
          return post("/auth/passkey/finish", { next: btn.getAttribute("data-next") || "/", credential: encodeCredential(cred) });
        })
        .then(function (res) { window.location.assign(res.redirect || "/"); })
        .catch(function (err) {
          showError(friendly(err));
          btn.disabled = false;
          btn.removeAttribute("aria-busy");
        });
    });
  }

  function init() {
    var form = document.querySelector("[data-passkey-register]");
    if (form) initRegister(form);
    var btn = document.querySelector("[data-passkey-login]");
    if (btn) initLogin(btn);
  }

  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();
})();
