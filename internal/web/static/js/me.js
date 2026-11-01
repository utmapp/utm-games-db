// Hydrates per-user state on cached public pages: the sign-in link, starred
// games and +1'd reports. Public HTML is identical for everyone; this is the
// only place personalization happens.
(function () {
  document.documentElement.classList.add('js');

  var me = null;
  function apply() {
    var nav = document.getElementById('nav-user');
    if (nav) {
      var link = document.createElement('a');
      if (me && me.user) {
        link.href = '/account';
        link.title = 'Account';
        link.textContent = me.user.display_name;
      } else {
        link.href = '/login?next=' + encodeURIComponent(location.pathname);
        link.textContent = 'Sign in';
      }
      nav.replaceChildren(link);
    }
    // Empty Mac fields on any form take the values saved on the profile.
    if (me && me.host) {
      var host = { macos_version: me.host.macos_version, device_model: me.host.device_model, chip: me.host.chip, ram_gb: me.host.ram_gb ? String(me.host.ram_gb) : '' };
      Object.keys(host).forEach(function (name) {
        document.querySelectorAll('form input[name="' + name + '"]').forEach(function (input) {
          if (!input.value && host[name]) input.value = host[name];
        });
      });
    }
    document.querySelectorAll('[data-star-game]').forEach(function (b) {
      var id = Number(b.getAttribute('data-star-game'));
      var on = !!(me && me.stars && me.stars.indexOf(id) >= 0);
      b.classList.toggle('is-on', on);
      var label = b.querySelector('.label');
      if (label) label.textContent = on ? 'Starred' : 'Star';
    });
    document.querySelectorAll('article.report[data-author]').forEach(function (a) {
      var mine = !!(me && me.user && Number(a.getAttribute('data-author')) === me.user.id);
      a.querySelectorAll('[data-edit-link]').forEach(function (l) { l.hidden = !mine; });
    });
    document.querySelectorAll('.comment[data-author]').forEach(function (c) {
      var mine = !!(me && me.user && Number(c.getAttribute('data-author')) === me.user.id);
      c.querySelectorAll('[data-delete-comment]').forEach(function (l) { l.hidden = !mine; });
    });
    document.querySelectorAll('[data-plusone-report]').forEach(function (b) {
      var id = Number(b.getAttribute('data-plusone-report'));
      var on = !!(me && me.plus_ones && me.plus_ones.indexOf(id) >= 0);
      b.classList.toggle('is-on', on);
      var article = b.closest('article.report');
      var mine = !!(me && me.user && article && Number(article.getAttribute('data-author')) === me.user.id);
      b.disabled = mine;
      b.title = mine ? "You can't +1 your own report" : 'I have the same experience';
    });
  }

  fetch('/api/me', { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
    .then(function (r) { return r.ok ? r.json() : null; })
    .then(function (data) { me = data; apply(); })
    .catch(function () {});

  function needLogin() {
    location.href = '/login?next=' + encodeURIComponent(location.pathname);
  }

  // Star / +1 buttons are real forms; intercept them for an in-place toggle.
  document.addEventListener('submit', function (ev) {
    var form = ev.target;
    if (!form.matches('form[data-toggle]')) return;
    ev.preventDefault();
    if (!me || !me.user) { needLogin(); return; }
    var btn = form.querySelector('button');
    if (btn) btn.disabled = true;
    fetch(form.action, {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Accept': 'application/json', 'X-Requested-With': 'fetch' },
      body: new FormData(form)
    }).then(function (r) {
      if (r.status === 401) { needLogin(); return null; }
      return r.json();
    }).then(function (data) {
      if (!data) return;
      if (data.error) { alert(data.error); return; }
      var kind = form.getAttribute('data-toggle');
      var id = Number(form.getAttribute('data-id'));
      if (kind === 'star') {
        me.stars = (me.stars || []).filter(function (x) { return x !== id; });
        if (data.on) me.stars.push(id);
        document.querySelectorAll('[data-star-count="' + id + '"]').forEach(function (el) { el.textContent = data.count; });
      } else if (kind === 'plusone') {
        me.plus_ones = (me.plus_ones || []).filter(function (x) { return x !== id; });
        if (data.on) me.plus_ones.push(id);
        document.querySelectorAll('[data-plusone-count="' + id + '"]').forEach(function (el) { el.textContent = data.count; });
      }
      apply();
    }).catch(function () {}).then(function () { if (btn) btn.disabled = false; });
  });
})();
