// Auto-submit the list filters when a select changes.
(function () {
  var form = document.querySelector('.toolbar form');
  if (!form) return;
  form.querySelectorAll('select').forEach(function (s) {
    s.addEventListener('change', function () { form.submit(); });
  });
})();
