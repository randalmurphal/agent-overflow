/*
  First-paint theme stamp. The app bundle is a deferred module, so without
  this the light-mode user gets a dark frame and everyone gets a white flash
  before app.css lands.

  WHY THIS IS A FILE AND NOT AN INLINE <script>. It was inline until the
  baseline CSP landed. `script-src 'self'` with no 'unsafe-inline' is the
  whole point of that policy, and the two ways to keep an inline script under
  it both cost more than the extra request does: a hash has to track bytes the
  frontend build owns, and a nonce means assembling the header per request
  (spec §14 forbids that outright). As a plain classic script it is
  parser-blocking, so it still runs before the deferred module and before
  first paint, and the preload scanner fetches it alongside the render-blocking
  stylesheets rather than after them.

  It lives in public/ so Vite copies it to the bundle root verbatim — no
  transform, no hashed name, no module wrapper. It must NOT become a module:
  a module is deferred, and a deferred theme stamp is a theme stamp that
  paints too late to do anything.

  The <style id="user-theme"> element this fills stays in the BODY of
  index.html on purpose. Vite appends the app's own stylesheet links to the
  end of <head>, so a style block in the head would LOSE the source-order tie
  to app.css's :root and the cached palette would be ignored at exactly the
  moment it matters. A body element wins that tie by document order, with no
  !important anywhere — and the applier then rewrites this same element by id,
  so there is still exactly one user-theme style in the document.

  The storage key is pinned against lib/theme/themeApply.svelte.ts by
  themeBootStamp.test.ts; a rename that lands on only one side is silent.

  THE CACHED CSS IS RE-VALIDATED HERE, not trusted. localStorage is
  same-origin writable, so this script is the one place in the app that takes
  CSS text from an untrusted store and puts it in the document — without a
  check, any same-origin script execution becomes a PERSISTENT injection
  primitive that outlives the page that planted it. The serializer
  (themeResolve.ts#serializeThemeCss) emits exactly one grammar, so this
  re-checks it line by line: `:root {` / `html.light {` headers, `}` closers,
  and `  --token: value;` declarations over the same conservative value
  charset with the same balanced-paren rule. Anything else falls back to the
  mode class plus the ground, which is most of what stops the flash.
  themeBootStamp.test.ts runs this validator against real serializer output
  and against input built to defeat it.
*/
(function () {
  /* boot-css-validator:start */
  function okCss(s) {
    if (s.length === 0) return false;
    var lines = s.split('\n');
    var open = false;
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      if (!open) {
        if (line !== ':root {' && line !== 'html.light {') return false;
        open = true;
        continue;
      }
      if (line === '}') {
        open = false;
        continue;
      }
      var m = /^ {2}--[a-z][a-z0-9]*(-[a-z0-9]+)*: ([-#()%,.\/+_ a-zA-Z0-9]+);$/.exec(line);
      if (!m) return false;
      var value = m[2];
      /* Mirror of themeResolve.ts REFUSED_FUNCTIONS: this CSS paints before
         any app code runs, and app.css uses the shorthand
         `background: var(--surface-0)`, so a url() smuggled into a planted
         localStorage stamp would beacon on the first frame. The CSP's
         img-src admits http(s) for chat markdown, so it does not stand in
         for this check. Case-folded and whitespace-free so `URL (` cannot
         slip past. */
      var folded = value.toLowerCase().replace(/[ \t]+/g, '');
      var refused = ['url(', 'image-set(', 'src(', 'var(', 'attr(', 'env('];
      for (var k = 0; k < refused.length; k++) {
        if (folded.indexOf(refused[k]) !== -1) return false;
      }
      var depth = 0;
      for (var j = 0; j < value.length; j++) {
        var ch = value.charAt(j);
        if (ch === '(') {
          depth++;
          if (depth > 8) return false;
        } else if (ch === ')') {
          depth--;
          if (depth < 0) return false;
        }
      }
      if (depth !== 0) return false;
    }
    return !open;
  }
  /* boot-css-validator:end */
  try {
    var raw = localStorage.getItem('agent-overflow:theme:boot');
    if (!raw) return;
    var stamp = JSON.parse(raw);
    var root = document.documentElement;
    if (stamp.c === 'light' || stamp.c === 'dark') root.classList.add(stamp.c);
    if (typeof stamp.b === 'string' && /^#[0-9a-fA-F]{6}$/.test(stamp.b)) {
      root.style.backgroundColor = stamp.b;
    }
    if (typeof stamp.s === 'string' && stamp.s.length <= 32768 && okCss(stamp.s)) {
      document.getElementById('user-theme').textContent = stamp.s;
    }
  } catch (err) {
    /* A wrong-colored first frame is the whole cost of any failure here. */
  }
})();
