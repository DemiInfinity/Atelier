/* Atelier as an app: service worker registration, the compact top-bar menu on
   phones and the bottom navigation when installed (display-mode: standalone).
   See internal/web/pwa.go. Everything here is optional: without JavaScript,
   or on plain HTTP where service workers aren't allowed, the site works as is. */
(function () {
  'use strict';

  // ---------- service worker ----------
  // Service workers need a secure context (HTTPS or localhost). On a plain
  // HTTP LAN address skip it silently.
  if ('serviceWorker' in navigator && window.isSecureContext) {
    window.addEventListener('load', function () {
      navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(function () {});
    });
  }

  function ready(fn) {
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', fn);
    else fn();
  }

  ready(function () {
    // ---------- offline page: try again ----------
    document.querySelectorAll('[data-m-reload]').forEach(function (el) {
      el.addEventListener('click', function (e) {
        e.preventDefault();
        window.location.reload();
      });
    });

    var bar = document.querySelector('.topbar');
    if (!bar) return;

    // ---------- compact top-bar menu (phones and tablets, up to 900px) ----------
    var nav = bar.querySelector('.topnav');
    if (nav && !bar.querySelector('.m-menu-btn')) {
      if (!nav.id) nav.id = 'm-topnav';
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'icon-btn m-menu-btn';
      btn.setAttribute('aria-label', 'Menu');
      btn.setAttribute('aria-expanded', 'false');
      btn.setAttribute('aria-controls', nav.id);
      btn.innerHTML = '<svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path class="m-menu-open" d="M4 7 H20 M4 12 H20 M4 17 H20"/><path class="m-menu-close" d="M6 6 L18 18 M18 6 L6 18"/></svg>';
      var end = bar.querySelector('.topbar-end');
      if (end) end.appendChild(btn);
      else bar.appendChild(btn);

      var setOpen = function (open) {
        bar.classList.toggle('m-open', open);
        btn.setAttribute('aria-expanded', open ? 'true' : 'false');
        btn.setAttribute('aria-label', open ? 'Close menu' : 'Menu');
      };
      btn.addEventListener('click', function () { setOpen(!bar.classList.contains('m-open')); });
      document.addEventListener('keydown', function (e) {
        if (e.key === 'Escape' && bar.classList.contains('m-open')) {
          setOpen(false);
          btn.focus();
        }
      });
      document.addEventListener('click', function (e) {
        if (bar.classList.contains('m-open') && !bar.contains(e.target)) setOpen(false);
      });
      var wide = window.matchMedia('(min-width: 901px)');
      var onWide = function () { if (wide.matches) setOpen(false); };
      if (wide.addEventListener) wide.addEventListener('change', onWide);
    }

    // ---------- bottom navigation (installed app only) ----------
    // CSS shows it only under @media (display-mode: standalone).
    if (!document.querySelector('.m-tabbar')) {
      var profile = bar.querySelector('a.avatar');
      var here = window.location.pathname;
      var icons = {
        home: 'M3 11 L12 4 L21 11 V20 H15 V14 H9 V20 H3 Z',
        inbox: 'M6 16 V11 a6 6 0 0 1 12 0 v5 l2 2 H4 Z M10 21 h4',
        search: 'M11 4 a7 7 0 1 0 0 14 a7 7 0 0 0 0-14 Z M20 20 L16 16',
        insights: 'M4 20 V10 M10 20 V4 M16 20 V13 M22 20 H2',
        me: 'M12 12 a4 4 0 1 0 0-8 a4 4 0 0 0 0 8 Z M4 21 a8 8 0 0 1 16 0'
      };
      var tabs = [
        ['Home', '/', icons.home, here === '/'],
        ['Inbox', '/notifications', icons.inbox, here.indexOf('/notifications') === 0],
        ['Search', '/search', icons.search, here.indexOf('/search') === 0],
        ['Insights', '/insights', icons.insights, here === '/insights']
      ];
      if (profile) {
        var p = profile.getAttribute('href') || '/';
        tabs.push(['Profile', p, icons.me, here === p]);
      }
      var tb = document.createElement('nav');
      tb.className = 'm-tabbar';
      tb.setAttribute('aria-label', 'App');
      tabs.forEach(function (t) {
        var a = document.createElement('a');
        a.href = t[1];
        if (t[3]) a.setAttribute('aria-current', 'page');
        a.innerHTML = '<svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="' + t[2] + '"/></svg>';
        var label = document.createElement('span');
        label.textContent = t[0];
        a.appendChild(label);
        tb.appendChild(a);
      });
      document.body.appendChild(tb);
      document.body.classList.add('m-has-tabbar');
    }
  });
})();
