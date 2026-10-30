// Sign-in form: solves the server's proof-of-work puzzle before the form can
// be submitted. The puzzle is bound to the single-use form token, so one
// solution cannot be reused. A pure-JS SHA-256 is used because crypto.subtle
// is unavailable on plain-http LAN addresses (e.g. testing from a phone).
(function () {
  var form = document.querySelector('form[data-login-form]');
  if (!form) return;
  var token = form.getAttribute('data-form-token') || '';
  var bits = Number(form.getAttribute('data-pow-bits') || 0);
  var minAge = Number(form.getAttribute('data-min-age-ms') || 0);
  var nonceInput = form.querySelector('[name="pow_nonce"]');
  var button = form.querySelector('button[type="submit"]');
  var status = form.querySelector('[data-pow-status]');
  var started = Date.now();
  var ready = false;

  // --- compact SHA-256 (ASCII input) -> Uint8Array ---------------------------
  var K = [];
  (function () {
    var primes = [], n = 2;
    while (primes.length < 64) { var p = true; for (var i = 0; i < primes.length; i++) { if (n % primes[i] === 0) { p = false; break; } } if (p) primes.push(n); n++; }
    for (var j = 0; j < 64; j++) K[j] = (Math.pow(primes[j], 1 / 3) * 4294967296) >>> 0;
  })();
  var H0 = [0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19];
  function rotr(x, n) { return (x >>> n) | (x << (32 - n)); }
  function sha256(str) {
    var len = str.length, words = [], i;
    for (i = 0; i < len; i++) words[i >> 2] |= (str.charCodeAt(i) & 0xff) << (24 - (i % 4) * 8);
    words[len >> 2] |= 0x80 << (24 - (len % 4) * 8);
    var nWords = (((len + 8) >> 6) + 1) * 16;
    for (i = words.length; i < nWords; i++) words[i] = 0;
    words[nWords - 1] = (len * 8) >>> 0;
    words[nWords - 2] = Math.floor((len * 8) / 4294967296);
    var h = H0.slice(0), w = new Array(64);
    for (var off = 0; off < nWords; off += 16) {
      for (i = 0; i < 16; i++) w[i] = words[off + i] | 0;
      for (i = 16; i < 64; i++) {
        var s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
        var s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
        w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
      }
      var a = h[0], b = h[1], c = h[2], d = h[3], e = h[4], f = h[5], g = h[6], hh = h[7];
      for (i = 0; i < 64; i++) {
        var S1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
        var ch = (e & f) ^ (~e & g);
        var t1 = (hh + S1 + ch + K[i] + w[i]) | 0;
        var S0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
        var maj = (a & b) ^ (a & c) ^ (b & c);
        var t2 = (S0 + maj) | 0;
        hh = g; g = f; f = e; e = (d + t1) | 0; d = c; c = b; b = a; a = (t1 + t2) | 0;
      }
      h[0] = (h[0] + a) | 0; h[1] = (h[1] + b) | 0; h[2] = (h[2] + c) | 0; h[3] = (h[3] + d) | 0;
      h[4] = (h[4] + e) | 0; h[5] = (h[5] + f) | 0; h[6] = (h[6] + g) | 0; h[7] = (h[7] + hh) | 0;
    }
    var out = new Uint8Array(32);
    for (i = 0; i < 8; i++) { out[i * 4] = h[i] >>> 24; out[i * 4 + 1] = (h[i] >>> 16) & 255; out[i * 4 + 2] = (h[i] >>> 8) & 255; out[i * 4 + 3] = h[i] & 255; }
    return out;
  }
  function leadingZeroBits(bytes) {
    var n = 0;
    for (var i = 0; i < bytes.length; i++) {
      if (bytes[i] === 0) { n += 8; continue; }
      for (var m = 0x80; m && !(bytes[i] & m); m >>= 1) n++;
      break;
    }
    return n;
  }
  window.__utmdbSha256Hex = function (s) { return Array.prototype.map.call(sha256(s), function (b) { return ('0' + b.toString(16)).slice(-2); }).join(''); };

  function enable() {
    ready = true;
    var wait = Math.max(0, minAge - (Date.now() - started));
    setTimeout(function () {
      button.disabled = false;
      button.textContent = button.getAttribute('data-label') || 'Send code';
      if (status) status.textContent = '';
    }, wait);
  }

  button.setAttribute('data-label', button.textContent);
  button.disabled = true;
  button.textContent = 'Preparing…';

  if (bits <= 0 || !token) { enable(); return; }

  var nonce = 0;
  function work() {
    var end = Date.now() + 40; // yield to keep the page responsive
    while (Date.now() < end) {
      for (var k = 0; k < 200; k++, nonce++) {
        if (leadingZeroBits(sha256(token + '.' + nonce)) >= bits) {
          nonceInput.value = String(nonce);
          enable();
          return;
        }
      }
    }
    setTimeout(work, 0);
  }
  work();

  form.addEventListener('submit', function (ev) {
    if (!ready) ev.preventDefault();
  });
})();
