import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import Pencil from '@lucide/svelte/icons/pencil';
import Trash2 from '@lucide/svelte/icons/trash-2';
import ReviewCommentActions from './ReviewCommentActions.svelte';

function setup(confirming: string | null = null) {
  const edit = vi.fn();
  const del = vi.fn();
  const onConfirmingChange = vi.fn();
  const view = render(ReviewCommentActions, {
    props: {
      actions: [
        { id: 'edit', icon: Pencil, label: 'Edit', run: edit, testid: 'edit' },
        { id: 'delete', icon: Trash2, label: 'Delete', run: del, confirm: 'Delete?', testid: 'delete' },
      ],
      confirming,
      onConfirmingChange,
    },
  });
  return { view, edit, del, onConfirmingChange };
}

describe('<ReviewCommentActions>', () => {
  it('runs an action at once, and asks for one that confirms', async () => {
    const { view, edit, del, onConfirmingChange } = setup();
    await fireEvent.click(view.getByTestId('edit'));
    expect(edit).toHaveBeenCalledTimes(1);
    await fireEvent.click(view.getByTestId('delete'));
    expect(del).not.toHaveBeenCalled();
    expect(onConfirmingChange).toHaveBeenCalledWith('delete');
    expect(view.queryByTestId('review-comment-confirm')).toBeNull();
  });

  it('asks in place with Yes and Cancel, no modal', async () => {
    const { view, del, onConfirmingChange } = setup('delete');
    const confirm = view.getByTestId('review-comment-confirm');
    expect(confirm.textContent).toContain('Delete?');
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    await fireEvent.click(view.getByTestId('review-comment-confirm-cancel'));
    expect(onConfirmingChange).toHaveBeenCalledWith(null);
    expect(del).not.toHaveBeenCalled();
    await fireEvent.click(view.getByTestId('review-comment-confirm-yes'));
    expect(del).toHaveBeenCalledTimes(1);
  });

  it('renders nothing with no actions', () => {
    const view = render(ReviewCommentActions, { props: { actions: [], confirming: null, onConfirmingChange: vi.fn() } });
    expect(view.queryByTestId('review-comment-actions')).toBeNull();
  });

  it('is revealed on hover and always on compact', () => {
    const { view } = setup();
    const icons = view.getByTestId('review-comment-actions').firstElementChild!;
    expect(icons.className).toContain('opacity-0');
    expect(icons.className).toContain('group-hover/comment:opacity-100');
    expect(icons.className).toContain('compact:opacity-100');
  });
});
