import { describe, expect, it } from 'vitest';
import shared from '../../test/fixtures/ansiShownVectors.json';
import { ansiHtml, ansiShownText } from './ansiText';
import { countFindMatches, findNeedle, foldFindText } from './findMatch';

// internal/cilog/search_test.go reads the same vectors, and
// AnsiText.browser.test.ts holds them to AnsiText's DOM.
describe('ansiShownText', () => {
  it.each(shared.vectors.map((vector) => [vector.name, vector] as const))('%s', (_name, vector) => {
    expect(ansiShownText(vector.input)).toBe(vector.shown);
    expect(countFindMatches(foldFindText(ansiShownText(vector.input)), findNeedle(vector.query))).toBe(vector.count);
  });

  it('never hides across a newline, so a run of lines shows each line', () => {
    const lines = ['a\x1b]0;open title', 'needle\x07 b', 'c\x1b_x', '\x1b\\d'];
    expect(ansiShownText(lines.join('\n'))).toBe(lines.map(ansiShownText).join('\n'));
    expect(ansiShownText(lines.join('\n'))).toContain('needle');
  });

  it('colours by SGR and escapes the text', () => {
    expect(ansiHtml('\x1b[31m<b>\x1b[0m')).toBe('<span class="ansi-fg-31">&lt;b&gt;</span>');
  });
});
