// Debounced game search for the report form (progressive enhancement over
// the server-rendered ?q= search).
(function () {
  var input = document.querySelector('[data-picker-input]');
  var results = document.querySelector('[data-picker-results]');
  if (!input || !results) return;
  var timer = null, last = '';
  function el(tag, className, text) {
    var e = document.createElement(tag);
    if (className) e.className = className;
    if (text) e.textContent = text;
    return e;
  }
  function render(hits) {
    if (!hits.length) {
      results.replaceChildren(el('p', 'faint small', 'No games found. Only games released on PC are listed.'));
      return;
    }
    results.replaceChildren.apply(results, hits.map(function (h) {
      var a = el('a');
      a.href = '/reports/new?game=' + encodeURIComponent(h.id);
      var name = el('span', '', h.name);
      if (h.active) name.append(' ', el('span', 'pill', 'has reports'));
      a.append(name, el('span', 'sub', [h.year || '', h.game_type || ''].filter(Boolean).join(' · ')));
      return a;
    }));
  }
  input.addEventListener('input', function () {
    clearTimeout(timer);
    var q = input.value.trim();
    if (q.length < 2) { results.replaceChildren(); return; }
    timer = setTimeout(function () {
      if (q === last) return;
      last = q;
      fetch('/api/games/search?q=' + encodeURIComponent(q), { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : { hits: [] }; })
        .then(function (d) { if (input.value.trim() === q) render(d.hits || []); })
        .catch(function () {});
    }, 200);
  });
})();
