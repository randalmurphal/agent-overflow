import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewCommentThread from './ReviewCommentThread.svelte';
import type { DiffReviewComment } from '../../types/models';
import { rawProps } from '../../../test/helpers/rawProps.svelte';

const comment: DiffReviewComment = {
  id: 'c1',
  threadId: 't1',
  scope: 'workspace',
  sourceKey: 'sk',
  filePath: 'src/a.ts',
  status: 'draft',
  newLine: 5,
  side: 'new',
  selectedText: 'const x = 1;',
  body: 'original body',
  createdAt: 1,
  updatedAt: 1,
};

// The edit's text is the owner's: these props hold it as the review state does.
function editableProps(overrides: Partial<{ onUpdate: (id: string, body: string) => void; onDelete: (id: string) => void }> = {}) {
  const props = rawProps({
    comment,
    editBody: null as string | null,
    onEditBodyChange: (_id: string, body: string | null) => { props.editBody = body; },
    onUpdate: vi.fn() as (id: string, body: string) => void,
    onDelete: vi.fn() as (id: string) => void,
    ...overrides,
  });
  return props;
}

async function setupEditing() {
  const onUpdate = vi.fn();
  const props = editableProps({ onUpdate });
  const view = render(ReviewCommentThread, props);
  await fireEvent.click(view.getByLabelText('Edit'));
  const textarea = view.container.querySelector('textarea')!;
  return { onUpdate, textarea };
}

describe('<ReviewCommentThread> edit keyboard save', () => {
  it('saves on Ctrl+Enter', async () => {
    const { onUpdate, textarea } = await setupEditing();
    await fireEvent.keyDown(textarea, { key: 'Enter', ctrlKey: true });
    expect(onUpdate).toHaveBeenCalledWith('c1', 'original body');
  });

  it('saves on Cmd+Enter (macOS)', async () => {
    const { onUpdate, textarea } = await setupEditing();
    await fireEvent.keyDown(textarea, { key: 'Enter', metaKey: true });
    expect(onUpdate).toHaveBeenCalledWith('c1', 'original body');
  });

  it('does not save on plain Enter', async () => {
    const { onUpdate, textarea } = await setupEditing();
    await fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onUpdate).not.toHaveBeenCalled();
  });
});

describe('<ReviewCommentThread> card', () => {
  it('labels the draft as yours with a draft chip and no orphaned chip', () => {
    const view = render(ReviewCommentThread, editableProps());
    expect(view.getByText('You')).toBeTruthy();
    expect(view.getByText('draft')).toBeTruthy();
    expect(view.queryByText('orphaned')).toBeNull();
    expect(view.getByTestId('review-avatar')).toBeTruthy();
  });

  it('marks a draft whose line left the diff as orphaned', () => {
    const view = render(ReviewCommentThread, { ...editableProps(), orphaned: true });
    expect(view.getByText('orphaned')).toBeTruthy();
  });

  it('offers Edit and Delete as icon buttons, and Delete deletes this comment', async () => {
    const onDelete = vi.fn();
    const view = render(ReviewCommentThread, editableProps({ onDelete }));
    expect(view.getByLabelText('Edit').tagName).toBe('BUTTON');
    const remove = view.getByLabelText('Delete');
    expect(remove.tagName).toBe('BUTTON');
    await fireEvent.click(remove);
    expect(onDelete).toHaveBeenCalledWith('c1');
  });
});
