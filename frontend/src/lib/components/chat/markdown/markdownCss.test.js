import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const appCss = readFileSync('src/app.css', 'utf8');

describe('markdown CSS', () => {
  it('wraps Streamdown tables within the markdown width', () => {
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s*\{[^}]*max-width:\s*100%;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s*\{[^}]*overflow-x:\s*visible;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*display:\s*table;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*overflow:\s*visible;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*table-layout:\s*fixed;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*width:\s*100%;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*max-width:\s*100%;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*margin:\s*0;/s);
    expect(appCss).toMatch(/\.markdown-body\s+th,\s*\n\.markdown-body\s+td\s*\{[^}]*overflow-wrap:\s*anywhere;/s);
    expect(appCss).toMatch(/\.markdown-body\s+th,\s*\n\.markdown-body\s+td\s*\{[^}]*word-break:\s*break-word;/s);
  });
});
