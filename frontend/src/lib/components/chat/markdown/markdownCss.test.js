import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const appCss = readFileSync('src/app.css', 'utf8');

describe('markdown CSS', () => {
  it('wraps markdown code spans and code blocks instead of scrolling horizontally', () => {
    expect(appCss).toMatch(/\.markdown-body\s+code\s*\{[^}]*display:\s*inline;/s);
    expect(appCss).toMatch(/\.markdown-body\s+code\s*\{[^}]*white-space:\s*pre-wrap;/s);
    expect(appCss).toMatch(/\.markdown-body\s+code\s*\{[^}]*overflow-wrap:\s*anywhere;/s);
    expect(appCss).toMatch(/\.markdown-body\s+code\s*\{[^}]*overflow-x:\s*visible;/s);
    expect(appCss).toMatch(/\.markdown-body\s+pre\s*\{[^}]*white-space:\s*pre-wrap;/s);
    expect(appCss).toMatch(/\.markdown-body\s+pre\s*\{[^}]*overflow-wrap:\s*anywhere;/s);
    expect(appCss).toMatch(/\.markdown-body\s+pre\s*\{[^}]*overflow-x:\s*visible;/s);
  });

  it('wraps Streamdown tables within the markdown width', () => {
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s*\{[^}]*max-width:\s*100%;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s*\{[^}]*overflow-x:\s*visible;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*display:\s*table;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*overflow:\s*visible;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*table-layout:\s*auto;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*width:\s*100%;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*max-width:\s*100%;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*margin:\s*0;/s);
    // Cells use break-word, NOT anywhere. With table-layout:auto the browser
    // sizes each column from its cell's min-content width; `anywhere` collapses
    // that to ~1ch and starves narrow columns (#, Sev), splitting short values
    // mid-token. `break-word` sizes columns from real word widths while still
    // wrapping truly overlong tokens. The negative guard keeps anyone from
    // regressing it back to `anywhere`. See the table block in app.css.
    expect(appCss).toMatch(/\.markdown-body\s+th,\s*\n\.markdown-body\s+td\s*\{[^}]*overflow-wrap:\s*break-word;/s);
    expect(appCss).not.toMatch(/\.markdown-body\s+th,\s*\n\.markdown-body\s+td\s*\{[^}]*overflow-wrap:\s*anywhere;/s);
  });

  it('consumes every markdown prose token, with the precedence carve-outs', () => {
    // The --md-* tokens resolve and paint nothing unless these rules exist;
    // the theme suites can only prove the tokens RESOLVE. A regex pin per
    // consumption keeps a refactor from silently dropping one — the failure
    // mode is invisible (prose falls back to inherited color and every
    // other test stays green).
    expect(appCss).toMatch(/\.markdown-body\s+h6\s*\{[^}]*color:\s*var\(--md-heading\);/s);
    expect(appCss).toMatch(/\.markdown-body\s+strong\s*\{[^}]*color:\s*var\(--md-bold\);/s);
    expect(appCss).toMatch(/\.markdown-body\s+a\s*\{[^}]*color:\s*var\(--md-link\);/s);
    expect(appCss).toMatch(/\.markdown-body\s+code\s*\{[^}]*color:\s*var\(--md-inline-code\);/s);
    expect(appCss).toMatch(/\.markdown-body\s+blockquote\s*\{[^}]*color:\s*var\(--md-blockquote\);/s);
    expect(appCss).toMatch(/\.markdown-body\s+li::marker\s*\{[^}]*color:\s*var\(--md-marker\);/s);
    // Carve-outs: bold inherits inside headings, links and quotes; code
    // inherits inside links (live and blocked); pre code stays on the
    // block's own text so syntax spans keep sole ownership.
    expect(appCss).toMatch(
      /\.markdown-body\s+:is\(h1, h2, h3, h4, h5, h6, a, blockquote\)\s+strong,\s*\n\.markdown-body\s+a\s+code,\s*\n\.markdown-body\s+\[data-streamdown-link-blocked\]\s+code\s*\{\s*color:\s*inherit;/s,
    );
    expect(appCss).toMatch(/\.markdown-body\s+pre\s+code\s*\{[^}]*color:\s*inherit;/s);
  });
});
