import { describe, expect, it } from 'vitest';
import shared from '../../test/fixtures/ansiShownVectors.json';
import { ansiHtml, ansiHtmlWithState, ansiScanStepsForTest, ansiShownText, appendAnsiDeltaToDom } from './ansiText';
import { countFindMatches, findNeedle } from './findMatch';

// internal/cilog/search_test.go reads the same vectors, and
// AnsiText.browser.test.ts holds them to AnsiText's DOM.
describe('ansiShownText', () => {
  it.each(shared.vectors.map((vector) => [vector.name, vector] as const))('%s', (_name, vector) => {
    expect(ansiShownText(vector.input)).toBe(vector.shown);
    expect(countFindMatches(ansiShownText(vector.input), findNeedle(vector.query))).toBe(vector.count);
  });

  it('never hides across a newline, so a run of lines shows each line', () => {
    const lines = ['a\x1b]0;open title', 'needle\x07 b', 'c\x1b_x', '\x1b\\d'];
    expect(ansiShownText(lines.join('\n'))).toBe(lines.map(ansiShownText).join('\n'));
    expect(ansiShownText(lines.join('\n'))).toContain('needle');
  });

  // Each form of input the scan could rescan: unterminated starts, strings
  // stopped by another escape, long runs of parameters, many forms.
  const adversarial: Record<string, (n: number) => string> = {
    'CSI starts with no final byte': (n) => '\x1b['.repeat(n),
    'SGR parameters with no final byte': (n) => '\x1b[' + '1;'.repeat(n),
    'one unterminated OSC': (n) => '\x1b]' + 'a'.repeat(n),
    'OSC starts': (n) => '\x1b]'.repeat(n),
    'one unterminated APC': (n) => '\x1b_' + 'a'.repeat(n),
    'APC starts': (n) => '\x1b_'.repeat(n),
    'APC stopped by an ESC': (n) => '\x1b_a\x1b'.repeat(n),
    'one unterminated DCS': (n) => '\x1bP' + 'a'.repeat(n),
    'nested starts': (n) => '\x1b[\x1b]\x1b_'.repeat(n),
    'many SGR': (n) => 'a\x1b[31mb\x1b[0m'.repeat(n),
    'many OSC': (n) => 'a\x1b]0;t\x07b'.repeat(n),
  };

  it.each(Object.entries(adversarial))('reads %s in one pass: at most twice its length, so ten times the input is ten times the work', (_name, make) => {
    const small = make(1_000);
    const large = make(10_000);
    expect(ansiScanStepsForTest(small)).toBeLessThanOrEqual(2 * small.length);
    expect(ansiScanStepsForTest(large)).toBeLessThanOrEqual(2 * large.length);
    expect(ansiScanStepsForTest(large)).toBeLessThanOrEqual(11 * ansiScanStepsForTest(small));
  });

  it('colours by SGR and escapes the text', () => {
    expect(ansiHtml('\x1b[31m<b>\x1b[0m')).toBe('<span class="ansi-fg-31">&lt;b&gt;</span>');
  });

  it('does not double-newline when CR and LF are separated by OSC/APC within or across chunks', () => {
    // Chunk ending with \r followed by OSC preserves endsWithCR so a subsequent
    // \n chunk triggers fallback instead of appending a second newline.
    const first = ansiHtmlWithState('a\r\x1b]0;title\x07');
    expect(first.state.endsWithCR).toBe(true);

    // Within one chunk via appendAnsiDeltaToDom: \r + OSC + \n collapses to one \n.
    const hostWithin = document.createElement('div');
    const stWithin = appendAnsiDeltaToDom(hostWithin, 'a\r\x1b]0;title\x07\nb', {
      openClass: '',
      incompleteTail: false,
      endsWithCR: false,
    });
    expect(stWithin).not.toBeNull();
    expect(hostWithin.textContent).toBe('a\nb');

    // Across two chunks: chunk 1 ends with \r + OSC, chunk 2 starts with \n (or OSC + \n).
    // appendAnsiDeltaToDom returns null so AnsiText falls back to full morphing instead of
    // duplicating the newline.
    const hostAcross = document.createElement('div');
    const st1 = appendAnsiDeltaToDom(hostAcross, 'a\r\x1b]0;title\x07', {
      openClass: '',
      incompleteTail: false,
      endsWithCR: false,
    });
    expect(st1?.endsWithCR).toBe(true);
    expect(appendAnsiDeltaToDom(hostAcross, '\nb', st1!)).toBeNull();
    expect(appendAnsiDeltaToDom(hostAcross, '\x1b]0;t2\x07\nb', st1!)).toBeNull();
  });
});
