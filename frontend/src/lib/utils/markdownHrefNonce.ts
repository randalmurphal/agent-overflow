// The one per-page-load nonce that gates every `agent-overflow:` href our
// markdown extensions mint.
//
// Streamdown's `transformUrl` honors a custom-scheme prefix only when the
// URL `startsWith(prefix)` (see `lib/markdown/render/elements/url.ts`). By
// baking a nonce into the prefixes handed to Streamdown, third-party or
// model-authored markdown like
// `[click](agent-overflow:open?path=/etc/passwd)` is rejected at the URL
// filter: that text can never observe the rendered nonce, so it cannot
// forge a passing prefix. Extensions construct hrefs from the same
// prefixes, so legitimate links round-trip.
//
// One value for every scheme (`open`, `image`, `forge`). A second nonce
// would buy nothing and would let one surface's prefix pass another's
// check by accident.
//
// Crypto: 16 bytes (128 bits) is more than enough — the nonce only needs
// to be unpredictable to a single page-load's worth of rendered content.
// `crypto.getRandomValues` is available in every modern browser,
// happy-dom and Node 18+.
export const MARKDOWN_HREF_NONCE = generateMarkdownHrefNonce();

function generateMarkdownHrefNonce(): string {
  const bytes = new Uint8Array(16);
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    crypto.getRandomValues(bytes);
  } else {
    // SSR / test environments without webcrypto — fail closed by
    // generating a session-stable value (Math.random is good enough
    // here because there's no live browser to attack).
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256);
  }
  let hex = '';
  for (let i = 0; i < bytes.length; i += 1) hex += bytes[i].toString(16).padStart(2, '0');
  return hex;
}
