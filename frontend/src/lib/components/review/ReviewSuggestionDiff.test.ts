import { render, waitFor } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ReviewEditorPreview from './ReviewEditorPreview.svelte';
import ReviewSentCommentBody from './ReviewSentCommentBody.svelte';
import { parseReviewFiles } from '../../utils/patchStore';
import type { CommentSuggestion, ReviewComment, ReviewThread } from '../../types/models';
import type { Block } from '../../../../bindings/agent-overflow/internal/mdsuggest/models';

// A suggestion renders as the diff it proposes where the backend placed
// its fence, with the markdown around it: the replaced lines from the
// forge when it reports them, else from the diff the pane holds, else the
// additions alone. An unplaced one follows the whole body. A preview asks
// the backend where its fences are and drops an answer for older text.

const PATCH = [
  'diff --git a/src/a.ts b/src/a.ts', '--- a/src/a.ts', '+++ b/src/a.ts',
  '@@ -1,3 +1,3 @@', ' const a = 1;', '-const b = 2;', '+const b = 3;', ' const c = 4;', '',
].join('\n');
const BODY = 'Before the block.\n```suggestion\nconst b = 30;\n```\nAfter the block.';
const THREAD: ReviewThread = { id: 't1', path: 'src/a.ts', line: 2, side: 'right', isResolvable: true, isResolved: false, isOutdated: false, comments: [] };

function lines(root: HTMLElement, kind: 'removed' | 'added'): string[] {
  return [...root.querySelectorAll(`[data-testid="review-suggestion-${kind}"]`)].map((cell) => cell.textContent!);
}

function suggestion(over: Partial<CommentSuggestion> = {}): CommentSuggestion {
  return { index: 0, to: ['const b = 30;'], appliable: false, bodyStartLine: 1, bodyEndLine: 3, bodyPlaced: true, digest: 'a'.repeat(64), ...over };
}

function comment(suggestions: CommentSuggestion[], body = BODY): ReviewComment {
  return { id: 'c1', authorLogin: 'octo', body, createdAt: '2026-01-01T00:00:00Z', databaseID: 1, viewerCanEdit: false, viewerCanDelete: false, suggestions };
}

function sent(c: ReviewComment, file: ReturnType<typeof parseReviewFiles>[number] | null = null) {
  const review = file ? {
    sentComments: null, reactions: { reactionsOf: () => [], error: () => null, withheld: null, pending: () => false },
    editorPreviews: null,
    suggestions: { available: false, withheld: null, currentFile: () => file, pending: () => false, error: () => null },
  } as never : null;
  return render(ReviewSentCommentBody, { review, thread: THREAD, comment: c });
}

describe('a sent suggestion', () => {
  it('shows the forge\'s replaced lines and its own in place, with the markdown around it and no code block', () => {
    const { container } = sent(comment([suggestion({ from: ['const b = 3;'], fromKnown: true })]));
    expect(lines(container, 'removed')).toEqual(['const b = 3;']);
    expect(lines(container, 'added')).toEqual(['const b = 30;']);
    const text = container.textContent!;
    expect(text.indexOf('Before the block.')).toBeLessThan(text.indexOf('const b = 30;'));
    expect(text.indexOf('const b = 30;')).toBeLessThan(text.indexOf('After the block.'));
    expect(container.querySelector('pre')).toBeNull();
  });

  it('reads the replaced lines from the diff when the forge does not report them', async () => {
    const file = parseReviewFiles(PATCH)[0];
    const { container } = sent(comment([suggestion({ startLine: 2, endLine: 2 })]), file);
    await waitFor(() => expect(lines(container, 'removed')).toEqual(['const b = 3;']));
    expect(lines(container, 'added')).toEqual(['const b = 30;']);
  });

  it('shows the additions alone when the replaced text is not known', () => {
    const { container } = sent(comment([suggestion()]));
    expect(lines(container, 'removed')).toEqual([]);
    expect(lines(container, 'added')).toEqual(['const b = 30;']);
  });

  it('adds one empty line for [""] and deletes for []', () => {
    const empty = sent(comment([suggestion({ from: ['x'], fromKnown: true, to: [''] })]));
    expect(lines(empty.container, 'added')).toEqual(['']);
    empty.unmount();
    const deletion = sent(comment([suggestion({ from: ['x'], fromKnown: true, to: [] })]));
    expect(lines(deletion.container, 'added')).toEqual([]);
    expect(lines(deletion.container, 'removed')).toEqual(['x']);
  });

  it('follows the whole body, its fence as code, when the forge could not place it', () => {
    const { container } = sent(comment([suggestion({ bodyPlaced: false, from: ['const b = 3;'], fromKnown: true })]));
    expect(container.querySelector('pre')?.textContent).toContain('const b = 30;');
    const diff = container.querySelector('[data-testid="review-suggestion"]')!;
    expect(container.textContent!.indexOf('After the block.')).toBeLessThan(container.textContent!.indexOf(diff.textContent!));
  });
});

describe('an editor\'s preview', () => {
  const block: Block = { lines: ['const b = 30;'], startLine: 1, endLine: 3 };

  it('shows the fences as code until the backend answers, then the diff in place', async () => {
    let answer!: (blocks: Block[]) => void;
    const parse = (body: string) => { expect(body).toBe(BODY); return new Promise<Block[]>((resolve) => { answer = resolve; }); };
    const file = parseReviewFiles(PATCH)[0];
    const { container } = render(ReviewEditorPreview, { body: BODY, height: 80, source: { file, from: 2, to: 2 }, parse });
    expect(container.querySelector('pre')?.textContent).toContain('const b = 30;');
    expect(container.querySelector('[data-testid="review-suggestion"]')).toBeNull();
    answer([block]);
    await waitFor(() => expect(lines(container, 'removed')).toEqual(['const b = 3;']));
    expect(lines(container, 'added')).toEqual(['const b = 30;']);
    expect(container.querySelector('pre')).toBeNull();
    expect(container.querySelector<HTMLElement>('[data-testid="review-editor-preview"]')!.style.height).toBe('80px');
  });

  it('drops an answer for an older text, and never lays a text out by another\'s answer', async () => {
    const answers: ((blocks: Block[]) => void)[] = [];
    const parse = () => new Promise<Block[]>((resolve) => { answers.push(resolve); });
    const view = render(ReviewEditorPreview, { body: BODY, height: 0, parse });
    await view.rerender({ body: `${BODY}\nMore.`, height: 0, parse });
    answers[1]([block]);
    await waitFor(() => expect(lines(view.container, 'added')).toEqual(['const b = 30;']));
    // An older read answering after the newer one changes nothing.
    answers[0]([]);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(lines(view.container, 'added')).toEqual(['const b = 30;']);
    // The text changes again: until its answer, the fences are code.
    await view.rerender({ body: `Lead.\n${BODY}`, height: 0, parse });
    expect(view.container.querySelector('[data-testid="review-suggestion"]')).toBeNull();
    expect(view.container.querySelector('pre')?.textContent).toContain('const b = 30;');
  });

  it('keeps the fences as code and says why when the backend refuses the body', async () => {
    const parse = async () => { throw new Error('a comment body is at most 1000000 characters'); };
    const { container } = render(ReviewEditorPreview, { body: BODY, height: 0, parse });
    await waitFor(() => expect(container.querySelector('[data-testid="review-editor-preview-note"]')?.textContent).toContain('at most 1000000 characters'));
    expect(container.querySelector('pre')?.textContent).toContain('const b = 30;');
  });
});
