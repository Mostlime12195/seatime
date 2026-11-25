// SeaTime web: the badge beside each commit on a project page. Your browser
// asks GitHub's public API whether the linked repo shows the commit; the
// SeaTime server does not. Without JS the commits are listed with no badge.
// GitHub answers a browser that is not signed in 60 times an hour, so a round
// makes at most 5 calls: one for the repo's newest 100 commits, then one for
// each listed commit that is not among them (GitHub answers with that commit
// and 99 of its ancestors). A commit that was found is remembered in this
// browser and not asked about again; other answers are kept for 10 minutes.
(function () {
  'use strict';
  var live = document.getElementById('live');
  if (!live || !window.fetch) return;

  var KEY = 'seatime-commits', KEEP = 600, MOST = 4;
  var SEALS = {
    ok: ['seal ok', 'on GitHub', 'This commit is in the linked repo on GitHub.'],
    bad: ['seal bad', 'not found', 'The linked repo is public, and this commit is not in it. It may not be pushed yet.'],
    hidden: ['seal', 'repo not public', 'GitHub does not show the linked repo to the public. It may be private, or the link may be wrong.'],
    asking: ['seal', 'checking', ''],
    none: ['seal', 'not checked', 'GitHub has not answered for this commit. Reload the page later to ask again.']
  };
  var now = function () { return Math.floor(Date.now() / 1000); };

  // db: for each "owner/repo", found {sha: first line of the message},
  // miss {sha: when GitHub last did not list it} and hidden (when GitHub
  // last did not show the repo); db.wait is when GitHub may be asked again.
  var db = {}, busy = false;
  try { db = Object(JSON.parse(localStorage.getItem(KEY))); } catch (_) {}
  var save = function () {
    try { localStorage.setItem(KEY, JSON.stringify(db)); }
    catch (_) { try { localStorage.removeItem(KEY); } catch (_) {} }
  };

  var table = function () { return live.querySelector('table[data-repo]'); };
  var rows = function (t) { return [].slice.call(t.querySelectorAll('tr[data-sha]')); };

  var paint = function () {
    var t = table();
    if (!t) return;
    var r = db[t.dataset.repo];
    if (!r) return;
    rows(t).forEach(function (tr) {
      var sha = tr.dataset.sha, cell = tr.cells[tr.cells.length - 1];
      var k = sha in r.found ? 'ok' : r.hidden ? 'hidden' : sha in r.miss ? 'bad' : busy ? 'asking' : 'none';
      if (cell.dataset.seal === k) return;
      cell.dataset.seal = k;
      var s = document.createElement('span');
      s.className = SEALS[k][0];
      s.textContent = SEALS[k][1];
      if (SEALS[k][2]) s.title = SEALS[k][2];
      cell.textContent = '';
      cell.appendChild(s);
      if (k === 'ok' && r.found[sha]) {
        var m = document.createElement('span');
        m.className = 'muted';
        m.textContent = r.found[sha];
        tr.cells[0].appendChild(document.createTextNode(' '));
        tr.cells[0].appendChild(m);
      }
    });
  };

  // ask gives GitHub's list of commits from sha back (from the default
  // branch when sha is ''), null when GitHub has no such repo or commit, and
  // [] for a repo with no commits. Anything else is no answer, and throws.
  var ask = function (repo, sha) {
    var u = 'https://api.github.com/repos/' + repo.split('/').map(encodeURIComponent).join('/') +
      '/commits?per_page=100' + (sha ? '&sha=' + encodeURIComponent(sha) : '');
    return fetch(u, { headers: { Accept: 'application/vnd.github+json' }, credentials: 'omit', referrerPolicy: 'no-referrer' })
      .then(function (res) {
        var reset = +res.headers.get('X-RateLimit-Reset');
        if (res.headers.get('X-RateLimit-Remaining') === '0' && reset) db.wait = Math.min(reset, now() + 3600);
        if (res.status === 200) return res.json();
        if (res.status === 404 || res.status === 422) return null;
        if (res.status === 409) return [];
        throw new Error(res.status);
      });
  };

  var round = function (repo, r, need) {
    var calls = 0;
    var mark = function (list) {
      need.forEach(function (sha) {
        list.forEach(function (c) {
          if (String(c.sha).indexOf(sha) === 0) r.found[sha] = String(Object(c.commit).message || '').split('\n')[0].slice(0, 200);
        });
      });
      paint();
    };
    var next = function () {
      var sha = need.filter(function (s) { return !(s in r.found) && !(s in r.miss); })[0];
      if (!sha || calls === MOST || db.wait > now()) return;
      calls++;
      return ask(repo, sha).then(function (list) {
        mark(list || []);
        if (!(sha in r.found)) r.miss[sha] = now();
        return next();
      });
    };
    busy = true;
    paint();
    ask(repo, '').then(function (list) {
      if (!list) { r.hidden = now(); return; }
      if (!list.length) { need.forEach(function (s) { r.miss[s] = now(); }); return; }
      mark(list);
      return next();
    }).catch(function () {
      if (!(db.wait > now())) db.wait = now() + 120;
    }).then(function () {
      busy = false;
      save();
      paint();
    });
  };

  // run reads what is remembered about the listed commits and asks GitHub
  // about the rest. Commits that are no longer listed are forgotten.
  var run = function () {
    var t = table();
    if (!t) return;
    if (busy) return paint();
    var repo = t.dataset.repo, old = Object(db[repo]), found = Object(old.found), miss = Object(old.miss);
    var fresh = now() - KEEP, r = { found: {}, miss: {}, hidden: old.hidden > fresh ? old.hidden : 0 }, need = [];
    rows(t).forEach(function (tr) {
      var sha = tr.dataset.sha;
      if (sha in found) r.found[sha] = found[sha];
      else if (miss[sha] > fresh) r.miss[sha] = miss[sha];
      else need.push(sha);
    });
    db[repo] = r;
    if (need.length && !r.hidden && !(db.wait > now())) round(repo, r, need);
    else paint();
  };

  run();
  // app.js replaces the block when new records arrive
  if (window.MutationObserver) new MutationObserver(run).observe(live, { childList: true });
})();
