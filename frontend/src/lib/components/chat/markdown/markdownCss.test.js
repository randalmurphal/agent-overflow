import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const appCss = readFileSync('src/app.css', 'utf8');

describe('markdown CSS', () => {
  it('keeps Streamdown table overflow owned by the outer table wrapper', () => {
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*display:\s*table;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*overflow:\s*visible;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*max-width:\s*none;/s);
    expect(appCss).toMatch(/\.markdown-body\s+\[data-streamdown-table\]\s+table\s*\{[^}]*margin:\s*0;/s);
  });
});
