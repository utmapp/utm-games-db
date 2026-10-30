// Questionnaire branching and upload pre-checks. The server enforces the same
// rules; this only hides sections that do not apply.
(function () {
  document.querySelectorAll('form[data-report-form], form[data-config-form]').forEach(wire);
  document.querySelectorAll('form[data-triage-form]').forEach(wireTriage);

  // Auto-Triage request: the chosen source reveals its own fields; notes and
  // the submit button appear once the source is fully specified.
  function wireTriage(form) {
    function val(name) {
      var el = form.querySelector('[name="' + name + '"]:checked');
      return el ? el.value : '';
    }
    function show(sel, on) {
      form.querySelectorAll(sel).forEach(function (el) {
        el.hidden = !on;
        el.querySelectorAll('input, select, textarea, button').forEach(function (i) { i.disabled = !on; });
      });
    }
    function update() {
      var kind = val('kind');
      show('[data-when="gift"]', kind === 'gift');
      show('[data-when="demo"]', kind === 'demo');
      var store = val('storefront');
      form.querySelectorAll('[data-store]').forEach(function (el) {
        el.hidden = !(kind === 'gift' && el.getAttribute('data-store') === store);
      });
      var demo = form.querySelector('[name="demo_url"]');
      var done = (kind === 'gift' && store !== '') ||
        (kind === 'demo' && demo && demo.value.trim() !== '');
      show('[data-when="source"]', done);
    }
    form.addEventListener('change', update);
    form.addEventListener('input', update);
    update();
  }

  function wire(form) {

  function val(name) {
    var el = form.querySelector('[name="' + name + '"]:checked, select[name="' + name + '"], input[type="hidden"][name="' + name + '"]');
    return el ? el.value : '';
  }
  function show(sel, on) {
    form.querySelectorAll(sel).forEach(function (el) {
      el.hidden = !on;
      el.querySelectorAll('input, select, textarea').forEach(function (i) { i.disabled = !on; });
    });
  }
  function text(name) {
    var el = form.querySelector('[name="' + name + '"]');
    return el ? el.value.trim() : '';
  }
  // Each required answer reveals the next part of the form. Sections are
  // nested, so every condition includes the ones before it.
  function update() {
    var isReport = form.hasAttribute('data-report-form');
    var rating = val('rating');
    var gold = rating === 'gold';
    var confirmed = val('completion_confirmed') === '1';
    var ratingDone = !isReport || (rating !== '' && (!gold || confirmed));
    var incl = form.querySelector('[name="include_config"]');
    if (incl) {
      show('[data-when="config"]', incl.checked);
      ratingDone = ratingDone && incl.checked;
    }
    show('[data-when="gold"]', gold);
    show('[data-when="rating"]', ratingDone);
    var version = val('utm_version_id');
    var versionDone = ratingDone && version !== '';
    show('[data-when="version"]', versionDone);
    var os = versionDone ? val('guest_os') : '';
    show('[data-when="windows"]', os === 'windows');
    show('[data-when="linux"]', os === 'linux');
    show('[data-when="other"]', os === 'other');
    show('[data-when="not-windows"]', os !== '' && os !== 'windows');
    var setup = os !== '' && (os !== 'linux' || text('linux_distro') !== '') && (os !== 'other' || text('other_os_name') !== '');
    show('[data-when="setup"]', setup);
    var step4 = setup && isReport && !gold;
    show('[data-when="step4"]', step4);
    var issue = step4 ? val('issue_type') : '';
    var issueDone = issue !== '' && issue !== 'unknown';
    show('[data-when="issue"]', issueDone);
    var needsDetails = issue === 'workaround' || issue === 'other';
    form.querySelectorAll('[data-details-optional]').forEach(function (el) { el.hidden = needsDetails; });
    var described = issueDone && (!needsDetails || text('description') !== '');
    show('[data-when="described"]', described);
    show('[data-when="repro"]', described && !needsDetails);
    show('[data-when="submit"]', setup && (!issueDone || described));
  }
  form.addEventListener('change', update);
  form.addEventListener('input', update);
  update();

  var files = form.querySelector('input[type="file"]');
  if (files) {
    var maxFile = Number(files.getAttribute('data-max-file') || 0);
    var maxTotal = Number(files.getAttribute('data-max-total') || 0);
    var maxCount = Number(files.getAttribute('data-max-count') || 0);
    var list = form.querySelector('[data-file-list]');
    files.addEventListener('change', function () {
      if (!list) return;
      var total = 0;
      function line(text, tooBig) {
        var span = document.createElement('span');
        if (tooBig) span.className = 'too-big';
        span.textContent = text;
        list.appendChild(span);
      }
      list.textContent = '';
      Array.prototype.forEach.call(files.files, function (f) {
        total += f.size;
        var big = maxFile && f.size > maxFile;
        line(f.name + ' — ' + (f.size / 1048576).toFixed(1) + ' MB' + (big ? ' (too large)' : ''), big);
      });
      if (maxCount && files.files.length > maxCount) line('Too many files (max ' + maxCount + ')', true);
      if (maxTotal && total > maxTotal) line('Total exceeds ' + (maxTotal / 1048576) + ' MB', true);
    });
  }
  }
})();
