// Runs before first paint so the page does not flash.
// Theme: a stored choice, else the system setting. Without scripts the page is light.
// Look: one of four skins, channel by default. The settings page links to
// ?look=grid and so on; the choice is remembered in this browser. ?theme=dark
// does the same for the theme.
(function () {
  var root = document.documentElement, q = new URLSearchParams(location.search);
  var pick = function (key, param, ok) {
    var v = q.get(param);
    try {
      if (v && ok.indexOf(v) >= 0) localStorage.setItem(key, v);
      else v = localStorage.getItem(key);
    } catch (_) {}
    return ok.indexOf(v) >= 0 ? v : null;
  };
  var t = pick('seatime-theme', 'theme', ['light', 'dark']);
  if (!t) t = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  root.setAttribute('data-theme', t);
  var s = pick('seatime-look', 'look', ['channel', 'grid', 'shop', 'gel']);
  if (s && s !== 'channel') root.setAttribute('data-skin', s);
})();
