// A new comment's editor whose file changes under it, in real Chromium on
// the desktop and the compact layout: a reload keeps the box where it was
// on screen with its focus, caret and text; the box offers the suggested
// row (marked) or a line the reader picks; nothing saves it until then.
// An editor whose file leaves the diff waits in the pane-level group.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';
import ReviewPane from './ReviewPane.svelte';
import { __resetReviewPaneStateForTest, reviewStateForPane, reviewSubjectForPane, type ReviewPaneState } from '../../stores/reviewPane.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { resetForTest as resetDiffReviewComments } from '../../stores/diffReviewComments.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { DiffReviewComment } from '../../types/models';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { setBindingMock, setReviewDiffMock } from '../../../test/mocks/bindings-app';

const LINES = 400;

function patch(above = 0): string {
  const out = ['diff --git a/src/long.ts b/src/long.ts', 'new file mode 100644', '--- /dev/null', '+++ b/src/long.ts', `@@ -0,0 +1,${LINES + above} @@`];
  for (let line = 1; line <= above; line += 1) out.push(`+inserted ${line}`);
  for (let line = 1; line <= LINES; line += 1) out.push(`+line ${line}`);
  return out.join('\n') + '\n';
}

let app: ReturnType<typeof mount> | undefined;
let target: HTMLDivElement;

beforeEach(() => {
  resetAppStorageForTest();
  __resetReviewPaneStateForTest();
  resetPanesForTest();
  resetDiffReviewComments();
  target = document.createElement('div');
  document.body.append(target);
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  setBindingMock('HighlightPatchWithContext', async () => ({ lines: [] }));
  setBindingMock('GetThread', async () => ({ id: 'thread-1', workspacePath: '/repo' }));
  setBindingMock('GetGitStatus', async () => ({}));
  setBindingMock('GitListBranches', async () => [{ name: 'main', isCurrent: true, isDefault: true }]);
  setBindingMock('ListDiffReviewComments', async () => []);
});

afterEach(async () => {
  if (app) await unmount(app);
  app = undefined;
  target.remove();
  setCompactLayoutForTest(false);
  __resetReviewPaneStateForTest();
});

function q<T extends Element = HTMLElement>(selector: string): T | null {
  return target.querySelector<T>(selector);
}

async function frames(count = 2): Promise<void> {
  for (let i = 0; i < count; i += 1) await new Promise((done) => requestAnimationFrame(() => done(null)));
}

function button(root: Element, label: string): HTMLButtonElement {
  const found = Array.from(root.querySelectorAll<HTMLButtonElement>('button')).find((each) => each.textContent?.trim() === label);
  if (!found) throw new Error(`no ${label} button`);
  return found;
}

function box(): HTMLElement | null {
  return q('[data-testid="review-draft-editor"]');
}

function textarea(): HTMLTextAreaElement {
  return box()!.querySelector('textarea')!;
}

// Mounts the pane on `current()`'s diff with an editor open on line 40,
// focused, typed into and scrolled to the middle of the viewport.
async function focusedEditor(current: () => string, layout: 'desktop' | 'compact') {
  setCompactLayoutForTest(layout === 'compact');
  target.style.cssText = layout === 'compact'
    ? 'position:fixed;top:0;left:0;width:390px;height:780px;display:flex;flex-direction:column'
    : 'position:fixed;top:0;left:0;width:1100px;height:700px;display:flex;flex-direction:column';
  setReviewDiffMock('OpenWorkspaceDiff', async () => current());
  const create = setBindingMock('CreateDiffReviewComment', async (_thread: string, input: Partial<DiffReviewComment>) => ({ id: 'c1', ...input }));
  const ctx = makeStubPanelContext();
  app = mount(ReviewPane, { target, props: { ctx } });
  const review: ReviewPaneState = reviewStateForPane(ctx.paneId, reviewSubjectForPane(ctx)!);
  await vi.waitFor(() => expect(review.files).toHaveLength(1), { timeout: 5_000 });
  const id = review.openDraftEditor({ filePath: 'src/long.ts', side: 'new', newLine: 40, selectedText: 'line 40' })!;
  await vi.waitFor(() => expect(box()).not.toBeNull());
  const scroller = q('[data-testid="review-scroll"]')!;
  scroller.scrollTop += box()!.getBoundingClientRect().top - scroller.getBoundingClientRect().top - 200;
  await frames(4);
  const area = textarea();
  area.focus();
  area.value = 'Half typed';
  area.dispatchEvent(new Event('input', { bubbles: true }));
  area.setSelectionRange(4, 4);
  return { review, id, create, area, top: () => box()!.getBoundingClientRect().top };
}

async function reload(review: ReviewPaneState): Promise<void> {
  q<HTMLButtonElement>('[data-testid="review-reload"]')!.click();
  await vi.waitFor(() => expect(review.loading).toBe(false));
  await frames(4);
}

describe.each(['desktop', 'compact'] as const)('a comment whose file changed, %s', (layout) => {
  it('stays where it was on screen with its focus, caret and text, and saves on the row the reader takes', async () => {
    let current = patch();
    const { review, id, create, area, top } = await focusedEditor(() => current, layout);
    const before = top();
    current = patch(5);
    await reload(review);

    await vi.waitFor(() => expect(q('[data-testid="review-draft-editor-note"]')?.textContent).toContain('The file changed under this comment.'));
    expect(textarea()).toBe(area);
    expect(document.activeElement).toBe(area);
    expect([area.selectionStart, area.selectionEnd]).toEqual([4, 4]);
    expect(area.value).toBe('Half typed');
    expect(Math.abs(top() - before)).toBeLessThanOrEqual(2);
    // The suggested row (its text moved five rows down) is marked.
    const marked = target.querySelectorAll('[data-review-target]');
    expect(marked).toHaveLength(1);
    expect(marked[0]!.textContent).toContain('line 40');
    expect(button(box()!, 'Add comment').disabled).toBe(true);
    expect(target.textContent?.toLowerCase()).not.toMatch(/unplaced|detached/);

    button(box()!, 'Use this line').click();
    await frames(2);
    expect(q('[data-testid="review-draft-editor-note"]')).toBeNull();
    expect(target.querySelectorAll('[data-review-target]')).toHaveLength(0);
    expect(document.activeElement).toBe(area);
    button(box()!, 'Add comment').click();
    await vi.waitFor(() => expect(create).toHaveBeenCalledTimes(1));
    expect(create.mock.calls[0]?.[1]).toMatchObject({ newLine: 45, selectedText: 'line 40', body: 'Half typed' });
    await vi.waitFor(() => expect(review.openEditors.some((editor) => editor.id === id)).toBe(false));
  });

  it('moves to a line the reader picks, and Escape cancels the move', async () => {
    let current = patch();
    const { review, id } = await focusedEditor(() => current, layout);
    current = patch(5);
    await reload(review);
    await vi.waitFor(() => expect(box()?.getAttribute('data-state')).toBe('detached'));

    button(box()!, 'Move').click();
    await vi.waitFor(() => expect(q('[data-testid="review-draft-editor-note"]')?.textContent).toContain('Choose a line for this comment.'));
    textarea().dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(review.armedEditorId).toBeNull());
    expect(box()).not.toBeNull();

    button(box()!, 'Move').click();
    await vi.waitFor(() => expect(review.armedEditorId).toBe(id));
    const add = target.querySelectorAll<HTMLButtonElement>('[data-testid="review-add-comment"]')[2]!;
    add.click();
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ id, state: 'placed', anchor: { newLine: 3 } }]));
    await frames(2);
    expect(target.querySelectorAll('[data-testid="review-draft-editor"]')).toHaveLength(1);
    expect(textarea().value).toBe('Half typed');
    expect(document.activeElement).toBe(textarea());
  });

  it('waits in the group when its file leaves the diff, through the empty state', async () => {
    let current = patch();
    const { review } = await focusedEditor(() => current, layout);
    current = '';
    await reload(review);
    const group = await vi.waitFor(() => {
      const found = q('[data-testid="review-gone-editors"]');
      expect(found).not.toBeNull();
      return found!;
    });
    expect(group.textContent).toContain('Comments on files not in this diff');
    expect(q('[data-testid="review-draft-editor-path"]')!.textContent).toBe('src/long.ts');
    expect(textarea().value).toBe('Half typed');
    expect(button(box()!, 'Copy text')).toBeTruthy();
    button(box()!, 'Discard').click();
    await vi.waitFor(() => expect(q('[data-testid="review-gone-editors"]')).toBeNull());
    expect(target.textContent?.toLowerCase()).not.toMatch(/unplaced|detached/);
  });
});
