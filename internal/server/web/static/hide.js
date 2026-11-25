// SeaTime web: the hide switch for new projects (templates/_newprojects.html)
// answers in place, without a page reload. Without JS the form posts and
// the server sends you back to the same page.
(function () {
  'use strict';
  document.querySelectorAll('form[data-visibility]').forEach(function (f) {
    f.addEventListener('submit', function (e) {
      if (!window.fetch || !e.submitter) return;
      e.preventDefault();
      var body = new URLSearchParams(new FormData(f));
      body.set('hidden', e.submitter.value);
      f.querySelectorAll('button').forEach(function (b) { b.disabled = true; });
      fetch(f.action, { method: 'POST', body: body, credentials: 'same-origin', headers: { Accept: 'application/json' } })
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function (d) {
          f.querySelectorAll('button').forEach(function (b) { b.hidden = true; });
          f.querySelector('[data-result]').textContent = d.hidden ? 'Hidden from your public profile.' : 'Public on your profile.';
        })
        .catch(function () {
          f.querySelectorAll('button').forEach(function (b) { b.disabled = false; });
          f.querySelector('[data-result]').textContent = 'That did not save. Try again.';
        });
    });
  });
})();
