import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewCommentThread from './ReviewCommentThread.svelte';
import type { DiffReviewComment } from '../../types/models';

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

async function setupEditing() {
  const onUpdate = vi.fn();
  const view = render(ReviewCommentThread, {
    comment,
    onUpdate,
    onDelete: vi.fn(),
  });
  await fireEvent.click(view.getByText('Edit'));
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
