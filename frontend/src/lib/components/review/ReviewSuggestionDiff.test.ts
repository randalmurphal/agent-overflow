import { render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ReviewCommentMarkdown from './ReviewCommentMarkdown.svelte';
import ReviewEditorPreview from './ReviewEditorPreview.svelte';
import { parseReviewFiles } from '../../utils/patchStore';
import type { CommentSuggestion } from '../../types/models';

// A suggestion block renders as the diff it proposes, in place of its
// fence, with the markdown around it: the replaced lines from the forge
// when it reports them, else from the diff the pane holds, else the
// additions alone.

const PATCH = [
  'diff --git a/src/a.ts b/src/a.ts', '--- a/src/a.ts', '+++ b/src/a.ts',
  '@@ -1,3 +1,3 @@', ' const a = 1;', '-const b = 2;', '+const b = 3;', ' const c = 4;', '',
].join('\n');
const BODY = 'Before the block.\n```suggestion\nconst b = 30;\n```\nAfter the block.';

function lines(root: HTMLElement, kind: 'removed' | 'added'): string[] {
  return [...root.querySelectorAll(`[data-testid="review-suggestion-${kind}"]`)].map((cell) => cell.textContent!);
}

describe('a rendered suggestion', () => {
  it('shows the forge\'s replaced lines and its own, with the markdown around it and no code block', () => {
    const suggestion: CommentSuggestion = { index: 0, from: ['const b = 3;'], fromKnown: true, to: ['const b = 30;'], appliable: true };
    const { container } = render(ReviewCommentMarkdown, { body: BODY, suggestions: [suggestion] });
    expect(lines(container, 'removed')).toEqual(['const b = 3;']);
    expect(lines(container, 'added')).toEqual(['const b = 30;']);
    expect(container.textContent).toContain('Before the block.');
    expect(container.textContent).toContain('After the block.');
    expect(container.querySelector('pre')).toBeNull();
  });

  it('reads the replaced lines from the diff when the forge does not report them', async () => {
    const file = parseReviewFiles(PATCH)[0];
    const suggestion: CommentSuggestion = { index: 0, startLine: 2, endLine: 2, to: ['const b = 30;'], appliable: true };
    const { container } = render(ReviewCommentMarkdown, {
      body: BODY,
      suggestions: [suggestion],
      sourceFor: (_block, found) => (found?.startLine ? { file, from: found.startLine, to: found.endLine ?? found.startLine } : null),
    });
    await waitFor(() => expect(lines(container, 'removed')).toEqual(['const b = 3;']));
    expect(lines(container, 'added')).toEqual(['const b = 30;']);
  });

  it('shows the additions alone when the replaced text is not known', () => {
    const { container } = render(ReviewCommentMarkdown, { body: BODY, suggestions: [{ index: 0, to: ['const b = 30;'], appliable: false }] });
    expect(lines(container, 'removed')).toEqual([]);
    expect(lines(container, 'added')).toEqual(['const b = 30;']);
  });

  it('adds one empty line for [""] and deletes for []', () => {
    const empty = render(ReviewCommentMarkdown, { body: BODY, suggestions: [{ index: 0, from: ['x'], fromKnown: true, to: [''], appliable: false }] });
    expect(lines(empty.container, 'added')).toEqual(['']);
    empty.unmount();
    const deletion = render(ReviewCommentMarkdown, { body: BODY, suggestions: [{ index: 0, from: ['x'], fromKnown: true, to: [], appliable: false }] });
    expect(lines(deletion.container, 'added')).toEqual([]);
    expect(lines(deletion.container, 'removed')).toEqual(['x']);
  });
});

describe('an editor\'s preview', () => {
  it('renders a fence as the diff it will become, its replaced lines read from the editor\'s lines', async () => {
    const file = parseReviewFiles(PATCH)[0];
    const { container } = render(ReviewEditorPreview, { body: BODY, height: 80, source: { file, from: 2, to: 2 } });
    await waitFor(() => expect(lines(container, 'removed')).toEqual(['const b = 3;']));
    expect(lines(container, 'added')).toEqual(['const b = 30;']);
    expect(container.querySelector<HTMLElement>('[data-testid="review-editor-preview"]')!.style.height).toBe('80px');
  });
});
