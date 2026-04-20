import { describe, expect, it } from 'vitest';
import { ansiToHtml } from './ansi';

describe('ansiToHtml', () => {
  it('escapes plain HTML when no ANSI codes are present', () => {
    expect(ansiToHtml('<script>alert(1)</script>')).toBe('&lt;script&gt;alert(1)&lt;/script&gt;');
  });

  it('renders simple ANSI colors as span classes', () => {
    const html = ansiToHtml('\u001b[31merror\u001b[0m ok');
    expect(html).toContain('text-red-400');
    expect(html).toContain('error');
    expect(html).toContain(' ok');
  });

  it('resets styles after code 0', () => {
    const html = ansiToHtml('\u001b[1;32mgreen\u001b[0m plain');
    expect(html).toContain('font-semibold');
    expect(html).toContain('text-emerald-400');
    expect(html).toContain('plain');
  });
});
