// Write / Preview tabs for Markdown fields. The preview is rendered by the
// server with the exact renderer used for display.
(function () {
  document.querySelectorAll('.mdedit').forEach(function (box) {
    var ta = box.querySelector('textarea');
    var preview = box.querySelector('.preview');
    var tabs = box.querySelectorAll('.tabs button');
    if (!ta || !preview || !tabs.length) return;
    box.setAttribute('data-mode', 'write');
    tabs.forEach(function (b) {
      b.addEventListener('click', function (ev) {
        ev.preventDefault();
        var mode = b.getAttribute('data-mode');
        tabs.forEach(function (x) { x.classList.toggle('is-active', x === b); });
        box.setAttribute('data-mode', mode);
        if (mode !== 'preview') return;
        if (!ta.value.trim()) { preview.innerHTML = '<p class="faint">Nothing to preview.</p>'; return; }
        preview.innerHTML = '<p class="faint">Rendering…</p>';
        fetch('/api/markdown/preview', {
          method: 'POST', credentials: 'same-origin',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'text/html', 'X-Requested-With': 'fetch' },
          body: 'text=' + encodeURIComponent(ta.value)
        }).then(function (r) { return r.text(); })
          .then(function (html) { preview.innerHTML = html; })
          .catch(function () { preview.innerHTML = '<p class="faint">Preview unavailable.</p>'; });
      });
    });
  });
})();
