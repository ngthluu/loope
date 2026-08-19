// app.js — the dashboard's residual client policy.
//
// Everything the old hand-rolled reconciler did about node identity (disclosure
// state, scroll offsets, focus, no whole-screen fadein replay) is now idiomorph's
// job, driven by hx-swap="morph:innerHTML". What is left here is policy morphing
// cannot express.
//
// Vendored versions: htmx 2.0.4, idiomorph-ext 0.7.3.

// copySid copies a full session id to the clipboard, with a textarea fallback for
// non-secure contexts. Called from an inline onclick, so it must stay global.
function copySid(btn) {
  if (btn.dataset.copying) return;
  var id = btn.getAttribute('data-sid') || '';
  var orig = btn.textContent;
  var done = function () {
    btn.dataset.copying = '1';
    btn.textContent = 'copied';
    setTimeout(function () { delete btn.dataset.copying; btn.textContent = orig; }, 1200);
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(id).then(done, done);
  } else {
    var ta = document.createElement('textarea');
    ta.value = id;
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) {}
    document.body.removeChild(ta);
    done();
  }
}

// The "live · Ns ago" ticker. #ago is re-created by every out-of-band statbar
// swap, so the element is looked up on each tick rather than cached.
var since = 0;
setInterval(function () {
  since++;
  var el = document.getElementById('ago');
  if (el) el.textContent = since + 's';
}, 1000);

// Only a request that actually came back resets the clock: htmx fires
// afterRequest for failures too, and a ticker that resets on those would keep
// reading "0s ago" against a server that stopped answering.
document.body.addEventListener('htmx:afterRequest', function (e) {
  if (!e.detail.successful) return;
  since = 0;
  var el = document.getElementById('ago');
  if (el) el.textContent = '0s';
});

// A transcript feed the user had pinned to the bottom keeps following new lines.
// One they scrolled up in keeps its offset for free — idiomorph never re-creates
// the node — so this only re-pins the feeds that were already at the bottom.
var pinned = {};
document.body.addEventListener('htmx:beforeSwap', function (e) {
  e.target.querySelectorAll('.txfeed').forEach(function (f) {
    pinned[f.getAttribute('data-seq')] = f.scrollHeight - f.scrollTop - f.clientHeight < 8;
  });
});
document.body.addEventListener('htmx:afterSwap', function (e) {
  e.target.querySelectorAll('.txfeed').forEach(function (f) {
    var k = f.getAttribute('data-seq');
    if (pinned[k] === undefined || pinned[k]) f.scrollTop = f.scrollHeight;
  });
});

// The disclosure rule: a running step's fragment always ships <details open>, so
// re-applying the server's `open` attribute would re-open a disclosure the user
// just closed three seconds ago. Newly appearing step cards still auto-open,
// because idiomorph inserts new nodes wholesale rather than morphing them.
if (window.Idiomorph) {
  window.Idiomorph.defaults.callbacks.beforeAttributeUpdated = function (name, node) {
    if (name === 'open' && node.hasAttribute && node.hasAttribute('data-disc')) return false;
  };
}
