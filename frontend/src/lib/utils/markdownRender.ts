// Tiny shared helper used by AnsiText to escape user-controlled text
// before stuffing into a DOM string. The legacy `renderMarkdown` /
// `sanitizeRenderedHtml` / `sanitizeRenderedSvg` exports went away
// when ChatMarkdown switched to `<Streamdown>` — the library handles
// markdown parsing, sanitization, math/code/diagram rendering, and
// link-prefix safety end-to-end.

export function escapeHtml(source: string): string {
  return source
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');
}
