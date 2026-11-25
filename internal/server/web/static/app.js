// SeaTime web: live updates over SSE, and a few small comforts.
// Everything works without JS (just not live).
(function () {
  'use strict';
  var root = document.documentElement;

  // Day / night. theme.js has already set data-theme.
  var tb = document.getElementById('theme');
  if (tb) {
    var lab = tb.querySelector('span') || tb;
    var dark = function () { return root.getAttribute('data-theme') === 'dark'; };
    var paint = function () { lab.textContent = dark() ? 'Day' : 'Night'; };
    tb.hidden = false;
    paint();
    tb.addEventListener('click', function () {
      var next = dark() ? 'light' : 'dark';
      root.setAttribute('data-theme', next);
      try { localStorage.setItem('seatime-theme', next); } catch (_) {}
      paint();
    });
  }

  // Settings: mark the look in use. The links themselves carry ?look=, which theme.js stores.
  var cur = root.getAttribute('data-skin') || 'channel';
  document.querySelectorAll('.look[data-look]').forEach(function (a) {
    if (a.getAttribute('data-look') === cur) a.setAttribute('aria-current', 'true');
  });

  // The clock in the top bar, like a system menu.
  var ck = document.getElementById('clock');
  if (ck) {
    var tick = function () {
      var d = new Date(), m = d.getMinutes();
      ck.textContent = d.getHours() + ':' + (m < 10 ? '0' : '') + m;
    };
    ck.hidden = false;
    tick();
    setInterval(tick, 15000);
  }

  // Mark where you are in the nav.
  document.querySelectorAll('.dock a').forEach(function (a) {
    var p = a.getAttribute('href');
    if (p === location.pathname || (p !== '/' && p.indexOf('/@') !== 0 && location.pathname.indexOf(p + '/') === 0)) a.setAttribute('aria-current', 'page');
  });

  // Copy buttons for install lines and the YSWS text.
  if (navigator.clipboard) {
    var copier = function (label, text) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'copy';
      b.textContent = label;
      b.addEventListener('click', function () {
        navigator.clipboard.writeText(text()).then(function () {
          b.textContent = 'Copied';
          setTimeout(function () { b.textContent = label; }, 1600);
        }, function () {});
      });
      return b;
    };
    document.querySelectorAll('.code').forEach(function (c) {
      var pre = c.querySelector('pre');
      if (pre) c.appendChild(copier('Copy', function () { return pre.textContent; }));
    });
    document.querySelectorAll('textarea[data-copy]').forEach(function (t) {
      var b = copier('Copy all', function () { return t.value; });
      b.className = 'btn small alt';
      t.insertAdjacentElement('afterend', b);
    });
  }

  if (!window.EventSource) return;

  // Global numbers on the landing page.
  var tk = document.getElementById('ticker');
  if (tk && tk.dataset.live) {
    var es = new EventSource(tk.dataset.live);
    es.addEventListener('stats', function (e) {
      try {
        var d = JSON.parse(e.data);
        document.querySelectorAll('[data-k]').forEach(function (el) {
          if (d[el.dataset.k] !== undefined) el.textContent = d[el.dataset.k];
        });
      } catch (_) {}
    });
  }

  // Profile / project pages: on any sync event, re-fetch the server-rendered block.
  var live = document.getElementById('live');
  if (live && live.dataset.live) {
    var timer = null, busy = false;
    var refresh = function () {
      if (busy) return;
      busy = true;
      var u = new URL(location.href);
      u.searchParams.set('frag', '1');
      fetch(u, { credentials: 'same-origin' })
        .then(function (r) { return r.ok ? r.text() : null; })
        .then(function (html) {
          if (html === null) return;
          live.innerHTML = html;
        })
        .catch(function () {})
        .then(function () { busy = false; });
    };
    var src = new EventSource(live.dataset.live);
    src.addEventListener('sync', function () {
      clearTimeout(timer);
      timer = setTimeout(refresh, 1000); // coalesce bursts
    });
  }
})();
