import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewDraftEditor from './ReviewDraftEditor.svelte';
import type { CommentAnchor } from '../../utils/reviewRows';

const anchor: CommentAnchor = { filePath: 'src/a.ts', newLine: 5, side: 'new' };

function setup() {
  const onSubmit = vi.fn();
  const onCancel = vi.fn();
  // `anchor` is a reserved Svelte mount option, so props must be nested.
  const view = render(ReviewDraftEditor, {
    props: {
      anchor,
      body: 'draft text',
      onBodyChange: vi.fn(),
      onCancel,
      onSubmit,
    },
  });
  const textarea = view.container.querySelector('textarea')!;
  return { onSubmit, onCancel, textarea };
}

describe('<ReviewDraftEditor> keyboard submit', () => {
  it('submits on Ctrl+Enter', async () => {
    const { onSubmit, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Enter', ctrlKey: true });
    expect(onSubmit).toHaveBeenCalledWith(anchor, 'draft text');
  });

  it('submits on Cmd+Enter (macOS)', async () => {
    const { onSubmit, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Enter', metaKey: true });
    expect(onSubmit).toHaveBeenCalledWith(anchor, 'draft text');
  });

  it('does not submit on plain Enter', async () => {
    const { onSubmit, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('cancels on Escape', async () => {
    const { onCancel, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Escape' });
    expect(onCancel).toHaveBeenCalledWith(anchor);
  });
});

describe('<ReviewDraftEditor> whose line changed', () => {
  it('says so, keeps the text and cannot be saved', async () => {
    const onSubmit = vi.fn();
    const view = render(ReviewDraftEditor, {
      props: {
        anchor: { ...anchor, lineChanged: true },
        body: 'draft text',
        onBodyChange: vi.fn(),
        onCancel: vi.fn(),
        onSubmit,
      },
    });
    const textarea = view.container.querySelector('textarea')!;
    expect(textarea.value).toBe('draft text');
    expect(view.getByTestId('review-draft-editor-line-changed').textContent).toContain('The line this comment was on (line 5) changed.');
    expect((view.getByRole('button', { name: 'Add comment' }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.keyDown(textarea, { key: 'Enter', ctrlKey: true });
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('shows no note on a line that did not change', () => {
    const view = render(ReviewDraftEditor, {
      props: { anchor, body: 'draft text', onBodyChange: vi.fn(), onCancel: vi.fn(), onSubmit: vi.fn() },
    });
    expect(view.queryByTestId('review-draft-editor-line-changed')).toBeNull();
  });
});
