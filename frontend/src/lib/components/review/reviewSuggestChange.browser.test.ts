// In real Chromium: `Suggest change` on a CRLF file. The lines come from
// the diff without their line endings, the textarea holds an LF fence,
// the caret sits after the last suggested line, and one typed character
// lands there, leaving the fence well formed.

import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, describe, expect, it } from 'vitest';
import { userEvent } from 'vitest/browser';
import '../../../app.css';
import ReviewCommentEditorShell from './ReviewCommentEditorShell.svelte';
import { parseReviewFiles } from '../../utils/patchStore';
import { readNewSideLines } from '../../utils/suggestionLines';

const CRLF_PATCH = [
  'diff --git a/w.txt b/w.txt',
  '--- a/w.txt',
  '+++ b/w.txt',
  '@@ -1,3 +1,3 @@',
  ' first line\r',
  '-old second\r',
  '+new second\r',
  ' third line\r',
  '',
].join('\n');

afterEach(() => {
  document.body.innerHTML = '';
});

async function suggested(body: string, from: number, to: number): Promise<HTMLTextAreaElement> {
  const [file] = parseReviewFiles(CRLF_PATCH);
  const view = render(ReviewCommentEditorShell, {
    props: {
      body,
      onBodyChange: (next: string) => { void view.rerender({ body: next }); },
      primary: { label: 'Comment', run: () => {} },
      onCancel: () => {},
      suggest: () => readNewSideLines(file, from, to),
    },
  });
  const box = view.container.querySelector('textarea')!;
  box.focus();
  box.setSelectionRange(box.value.length, box.value.length);
  view.container.querySelector<HTMLButtonElement>('[data-testid="review-editor-suggest"]')!.click();
  await expect.poll(() => box.value.includes('```suggestion')).toBe(true);
  await tick();
  return box;
}

describe('Suggest change on a CRLF file', () => {
  for (const [name, from, to, lines] of [
    ['one line', 2, 2, ['new second']],
    ['a range', 1, 3, ['first line', 'new second', 'third line']],
  ] as const) {
    it(`prefills ${name} in LF with the caret after the last suggested line`, async () => {
      const box = await suggested('Try:', from, to);
      const block = `Try:\n\`\`\`suggestion\n${lines.join('\n')}`;
      expect(box.value).toBe(`${block}\n\`\`\``);
      expect(box.value).not.toContain('\r');
      expect(document.activeElement).toBe(box);
      expect([box.selectionStart, box.selectionEnd]).toEqual([block.length, block.length]);
      await userEvent.keyboard('Z');
      expect(box.value).toBe(`${block}Z\n\`\`\``);
    });
  }

  it('places the caret by the text the textarea holds when the body itself is CRLF', async () => {
    const box = await suggested('Use:\r\nthis\r\n', 2, 2);
    const block = 'Use:\nthis\n```suggestion\nnew second';
    expect(box.value).toBe(`${block}\n\`\`\``);
    expect(box.selectionStart).toBe(block.length);
    await userEvent.keyboard('Z');
    expect(box.value).toBe(`${block}Z\n\`\`\``);
  });
});
