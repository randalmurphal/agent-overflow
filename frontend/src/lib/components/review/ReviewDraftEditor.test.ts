import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewDraftEditor from './ReviewDraftEditor.svelte';
import type { EditorRow } from '../../utils/reviewRows';

const editor: EditorRow = { id: 'e1', filePath: 'src/a.ts', side: 'new', state: 'placed', anchor: { filePath: 'src/a.ts', newLine: 5, side: 'new' }, suggested: null, armed: false, occurrence: 0, repeated: false, generation: 1 };
const suggested = { filePath: 'src/a.ts', newLine: 6, side: 'new' as const, selectedText: 'x' };

function setup() {
  const onSubmit = vi.fn();
  const onCancel = vi.fn();
  const view = render(ReviewDraftEditor, {
    props: {
      editor,
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
    expect(onSubmit).toHaveBeenCalledWith('e1', 'draft text');
  });

  it('submits on Cmd+Enter (macOS)', async () => {
    const { onSubmit, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Enter', metaKey: true });
    expect(onSubmit).toHaveBeenCalledWith('e1', 'draft text');
  });

  it('does not submit on plain Enter', async () => {
    const { onSubmit, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('cancels on Escape', async () => {
    const { onCancel, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Escape' });
    expect(onCancel).toHaveBeenCalledWith('e1');
  });
});

function renderEditor(row: EditorRow, handlers: Partial<Record<'onSubmit' | 'onCancel' | 'onUseLine' | 'onMove' | 'onCancelMove', (...args: any[]) => any>> = {}) {
  const props = {
    editor: row,
    body: 'draft text',
    onBodyChange: vi.fn(),
    onCancel: handlers.onCancel ?? vi.fn(),
    onSubmit: handlers.onSubmit ?? vi.fn(),
    onUseLine: handlers.onUseLine ?? vi.fn(),
    onMove: handlers.onMove ?? vi.fn(),
    onCancelMove: handlers.onCancelMove ?? vi.fn(),
  };
  const view = render(ReviewDraftEditor, { props });
  return { view, textarea: view.container.querySelector('textarea')!, props };
}

describe('<ReviewDraftEditor> whose file changed', () => {
  it('says so, offers the suggested line or a move, and cannot be saved', async () => {
    const onSubmit = vi.fn();
    const onUseLine = vi.fn(() => true);
    const onMove = vi.fn();
    const { view, textarea } = renderEditor({ ...editor, state: 'detached', anchor: null, suggested }, { onSubmit, onUseLine, onMove });
    expect(textarea.value).toBe('draft text');
    expect(view.getByTestId('review-draft-editor-note').textContent).toContain('The file changed under this comment.');
    expect((view.getByRole('button', { name: 'Add comment' }) as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.keyDown(textarea, { key: 'Enter', ctrlKey: true });
    expect(onSubmit).not.toHaveBeenCalled();
    await fireEvent.click(view.getByRole('button', { name: 'Use this line' }));
    expect(onUseLine).toHaveBeenCalledWith('e1', { generation: 1, suggested });
    expect(document.activeElement).toBe(textarea);
    await fireEvent.click(view.getByRole('button', { name: 'Move' }));
    expect(onMove).toHaveBeenCalledWith('e1');
  });

  it('asks for a line while armed; Cancel and Escape cancel the move, not the comment', async () => {
    const onCancelMove = vi.fn();
    const onCancel = vi.fn();
    const { view, textarea } = renderEditor({ ...editor, state: 'detached', anchor: null, suggested, armed: true }, { onCancelMove, onCancel });
    expect(view.getByTestId('review-draft-editor-note').textContent).toContain('Choose a line for this comment.');
    expect(view.queryByRole('button', { name: 'Add comment' })).toBeNull();
    await fireEvent.click(view.getByRole('button', { name: 'Cancel' }));
    await fireEvent.keyDown(textarea, { key: 'Escape' });
    expect(onCancelMove).toHaveBeenCalledTimes(2);
    expect(onCancel).not.toHaveBeenCalled();
  });

  it('passes the suggestion the reader pressed on, even when a reload changed it before the click', async () => {
    const onUseLine = vi.fn();
    const { view } = renderEditor({ ...editor, state: 'detached', anchor: null, suggested }, { onUseLine });
    const button = view.getByRole('button', { name: 'Use this line' });
    await fireEvent.pointerDown(button);
    const later = { ...suggested, newLine: 9 };
    await view.rerender({ editor: { ...editor, state: 'detached', anchor: null, suggested: later, generation: 2 } });
    await fireEvent.click(view.getByRole('button', { name: 'Use this line' }));
    expect(onUseLine).toHaveBeenCalledWith('e1', { generation: 1, suggested });
  });

  it('keeps focus on "Use this line" when the line is refused', async () => {
    const { view, textarea } = renderEditor({ ...editor, state: 'detached', anchor: null, suggested }, { onUseLine: () => false });
    const button = view.getByRole('button', { name: 'Use this line' });
    button.focus();
    await fireEvent.click(button);
    expect(document.activeElement).toBe(button);
    expect(document.activeElement).not.toBe(textarea);
  });

  it('shows no note on a placed editor', () => {
    const { view } = renderEditor(editor);
    expect(view.queryByTestId('review-draft-editor-note')).toBeNull();
  });
});

describe('<ReviewDraftEditor> on a line number two rows share', () => {
  it('says a comment cannot be attached there, and offers Move', async () => {
    const onMove = vi.fn();
    const { view } = renderEditor({ ...editor, repeated: true }, { onMove });
    expect(view.getByTestId('review-draft-editor-note').textContent).toContain('This line appears more than once in this view, so a comment cannot be attached to it.');
    expect((view.getByRole('button', { name: 'Add comment' }) as HTMLButtonElement).disabled).toBe(true);
    expect(view.queryByRole('button', { name: 'Use this line' })).toBeNull();
    await fireEvent.click(view.getByRole('button', { name: 'Move' }));
    expect(onMove).toHaveBeenCalledWith('e1');
  });
});

describe('<ReviewDraftEditor> whose file is not in the diff', () => {
  it('shows its path and text, and offers Copy text and Discard only', async () => {
    const onCancel = vi.fn();
    const { view, textarea } = renderEditor({ ...editor, state: 'gone', anchor: null }, { onCancel });
    expect(view.getByTestId('review-draft-editor-path').textContent).toBe('src/a.ts');
    expect(textarea.value).toBe('draft text');
    expect(view.getByTestId('review-draft-editor-note').textContent).toContain('This file is no longer in the diff.');
    expect(view.getAllByRole('button').map((button) => button.textContent?.trim())).toEqual(['Copy text', 'Discard']);
    await fireEvent.click(view.getByRole('button', { name: 'Discard' }));
    expect(onCancel).toHaveBeenCalledWith('e1');
  });
});

it('never says unplaced or detached', () => {
  for (const row of [editor, { ...editor, state: 'detached' as const, anchor: null, suggested }, { ...editor, state: 'detached' as const, anchor: null, suggested, armed: true }, { ...editor, state: 'gone' as const, anchor: null }]) {
    const { view } = renderEditor(row);
    expect(view.container.textContent?.toLowerCase()).not.toMatch(/unplaced|detached/);
    view.unmount();
  }
});
