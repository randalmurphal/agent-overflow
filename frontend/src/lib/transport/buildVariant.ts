// The product variant of the binary that served this page. A build compiled
// without remote access (Go build tag `noremote`) stamps
// `<meta name="ao-remote-access" content="off">` into the served index.html;
// the standard build serves the page unchanged. The meta is in the document
// before any module runs, so the answer is synchronous and fixed for the page
// lifetime. Absence, or any other content, means the standard build.

const META_SELECTOR = 'meta[name="ao-remote-access"]';

let cached: boolean | null = null;

function detect(): boolean {
  if (typeof document === 'undefined') return true;
  return document.querySelector(META_SELECTOR)?.getAttribute('content') !== 'off';
}

/** Whether this build can offer remote access at all. */
export function remoteAccessAvailable(): boolean {
  if (cached === null) cached = detect();
  return cached;
}

/**
 * Whether boot should open the pairing screen for a `#pair=` link. A build
 * without remote access cannot pair: the link, and the secret it carries,
 * are dropped from the address and boot proceeds as an ordinary launch.
 */
export function admitPairingLink(): boolean {
  if (!location.hash.startsWith('#pair=')) return false;
  if (remoteAccessAvailable()) return true;
  history.replaceState(null, '', location.pathname + location.search);
  return false;
}

/** Test-only: re-read the page on the next call. */
export function __resetBuildVariantForTest(): void {
  cached = null;
}
