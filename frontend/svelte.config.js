import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

// This config's own directory, and the vendor tree inside it. vite-plugin-svelte
// loads `svelte.config.js` from vite's `root`, so these two are the same
// directory — which is what makes resolving a warning's filename against
// CONFIG_DIR below exact rather than a guess.
const CONFIG_DIR = fileURLToPath(new URL('./', import.meta.url));
const VENDOR_DIR = `${resolve(CONFIG_DIR, 'vendor').replace(/\\/g, '/')}/`;

/**
 * True when a compiler warning came from a file inside THIS config's `vendor/`.
 *
 * Two shapes have to be handled, and getting this wrong fails silently in
 * opposite directions:
 *
 *   - `warning.filename` is normally RELATIVE to vite's root, because svelte's
 *     compiler re-bases it against the `rootDir` that vite-plugin-svelte passes
 *     (see svelte/src/compiler/state.js `adjust()`) — e.g.
 *     `vendor/svelte-streamdown/dist/Elements/TableDownload.svelte`. Matching
 *     only absolute paths therefore suppresses nothing at all.
 *   - Without a `rootDir` it stays absolute, and on Windows a vite module id is
 *     `C:/…` while `fileURLToPath` yields `C:\…`, so both sides are normalized
 *     to forward slashes before comparison.
 *
 * Resolving against CONFIG_DIR covers both (`resolve` returns an absolute input
 * unchanged) and — unlike the obvious `/(?:^|\/)vendor\//` test — cannot be
 * satisfied by a `vendor` segment somewhere ABOVE the project. That pattern
 * would silence every warning in `src/` for anyone whose checkout lives under
 * `~/vendor/…` or whose CI workspace sits on a `/vendor/` mount, and nothing
 * would announce that it had happened.
 */
function isVendorWarning(filename) {
  if (!filename) return false;
  return resolve(CONFIG_DIR, filename).replace(/\\/g, '/').startsWith(VENDOR_DIR);
}

/** @type {import('@sveltejs/vite-plugin-svelte').SvelteConfig} */
export default {
  // Vendored packages (frontend/vendor/*, see their VENDOR.md) compile as
  // project source now that they are no longer node_modules installs, so
  // their component warnings land in every build and test run. They are
  // upstream's authoring choices, not ours — the bar we hold vendored code
  // to is the regression battery in src/, not our lint surface — and a
  // warning we can't act on without re-diverging from upstream is noise
  // that trains everyone to ignore the ones that matter. Suppress by
  // directory; everything under src/ still warns normally, and a warning
  // with no filename at all always reaches the handler.
  onwarn(warning, handler) {
    if (isVendorWarning(warning.filename)) return;
    handler(warning);
  },
};
