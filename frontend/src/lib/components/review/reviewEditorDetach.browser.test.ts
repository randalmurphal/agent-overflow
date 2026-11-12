/// <reference types="@vitest/browser-playwright" />
// A new comment's editor whose file changes under it, in real Chromium on
// the desktop and the compact layout: a reload keeps the box where it was
// on screen with its focus, caret and text; the box offers the suggested
// row (marked) or a line the reader picks; nothing saves it until then.
// An editor whose file leaves the diff waits in the pane-level group.
// The gutter's add buttons and "Use this line" act on what the reader saw
// when the press began, by real pointer, touch and key events.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import { cdp, page, userEvent } from 'vitest/browser';
import '../../../app.css';
import ReviewPane from './ReviewPane.svelte';
import ReviewDraftEditor from './ReviewDraftEditor.svelte';
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

beforeEach(async () => {
  // Wide enough for the desktop pane, so a pointer reaches all of it.
  await page.viewport(1200, 800);
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

// Mounts the pane on `current()`'s diff.
async function mountDiff(current: () => string, layout: 'desktop' | 'compact') {
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
  await frames(2);
  return { review, create };
}

// Mounts the pane on `current()`'s diff with an editor open on line 40,
// focused, typed into and scrolled to the middle of the viewport.
async function focusedEditor(current: () => string, layout: 'desktop' | 'compact') {
  const { review, create } = await mountDiff(current, layout);
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

  it('keeps its place, focus and caret when its row moves outside the rows the list holds', async () => {
    let current = patch();
    const { review, id, top } = await focusedEditor(() => current, layout);
    // Earlier code at the top of the viewport, the box below it.
    expect(top()).toBeGreaterThan(100);
    const before = top();
    current = patch(1000);
    await reload(review);
    // Its line moved a thousand rows down, out of the rows the list held.
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ id, state: 'detached', suggested: { newLine: 1040 } }]));
    await vi.waitFor(() => expect(q('[data-testid="review-draft-editor-note"]')?.textContent).toContain('The file changed under this comment.'));
    await vi.waitFor(() => expect(document.activeElement).toBe(textarea()));
    expect(textarea().value).toBe('Half typed');
    expect([textarea().selectionStart, textarea().selectionEnd]).toEqual([4, 4]);
    expect(Math.abs(top() - before)).toBeLessThanOrEqual(2);
    expect(target.querySelector('[data-review-target]')?.textContent).toContain('line 40');
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

describe('a new comment\'s focus request', () => {
  it('never takes focus back from where the reader moved it while its row was hidden', async () => {
    const host = document.createElement('div');
    host.style.visibility = 'hidden';
    target.append(host);
    const other = document.createElement('input');
    target.append(other);
    const editor = mount(ReviewDraftEditor, {
      target: host,
      props: {
        editor: { id: 'e1', filePath: 'src/a.ts', side: 'new', state: 'placed', anchor: { filePath: 'src/a.ts', side: 'new', newLine: 1 }, suggested: null, armed: false, occurrence: 0, repeated: false, generation: 1 },
        body: '',
        onBodyChange: () => {},
        onCancel: () => {},
        onSubmit: () => {},
        consumeFocus: () => true,
      },
    });
    try {
      await frames(1);
      other.focus();
      host.style.visibility = 'visible';
      await frames(4);
      expect(document.activeElement).toBe(other);
    } finally {
      await unmount(editor);
    }
  });
});

// The gutter's add buttons on screen, in row order (line N is the N-1th).
function addButtons(): HTMLButtonElement[] {
  return Array.from(target.querySelectorAll<HTMLButtonElement>('[data-testid="review-add-comment"]'));
}

// The code text of the row holding `control`: somewhere to release a press
// that is not a button.
function rowText(control: Element): Element {
  return control.closest('.group')!.querySelector('[data-find-key]')!;
}

// The middle of `element` in the page's coordinates, which the input
// events of the browser's protocol take (this document is in a frame).
function pagePoint(element: Element): { x: number; y: number } {
  const frame = window.frameElement?.getBoundingClientRect();
  const scale = frame ? frame.width / window.innerWidth : 1;
  const rect = element.getBoundingClientRect();
  return { x: (frame?.left ?? 0) + (rect.left + rect.width / 2) * scale, y: (frame?.top ?? 0) + (rect.top + rect.height / 2) * scale };
}

// A real press on `from` released on `to`: a mouse down, a move and a
// release, which makes no click on `from`.
async function pressAndRelease(from: Element, to: Element): Promise<void> {
  window.focus();
  // Clear of the sticky file bar.
  from.scrollIntoView({ block: 'center' });
  await frames(1);
  const session = cdp();
  const start = pagePoint(from);
  const end = pagePoint(to);
  await session.send('Input.dispatchMouseEvent', { type: 'mouseMoved', ...start });
  await session.send('Input.dispatchMouseEvent', { type: 'mousePressed', ...start, button: 'left', buttons: 1, clickCount: 1 });
  await session.send('Input.dispatchMouseEvent', { type: 'mouseMoved', ...end, button: 'left', buttons: 1 });
  await session.send('Input.dispatchMouseEvent', { type: 'mouseReleased', ...end, button: 'left', buttons: 0, clickCount: 1 });
  await frames(1);
}

// A real touch on `element` that the browser cancels (pointercancel).
async function touchCancelled(element: Element): Promise<void> {
  window.focus();
  const session = cdp();
  const { x, y } = pagePoint(element);
  await session.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] });
  await session.send('Input.dispatchTouchEvent', { type: 'touchCancel', touchPoints: [] });
  await frames(1);
}

// Events seen on `element`, by type, to prove a real gesture reached it.
function seen(element: Element, types: string[]): Map<string, number> {
  const counts = new Map<string, number>();
  for (const type of types) element.addEventListener(type, () => counts.set(type, (counts.get(type) ?? 0) + 1));
  return counts;
}

async function reloadKeepingFocus(review: ReviewPaneState): Promise<void> {
  const active = document.activeElement;
  await reload(review);
  // The reload button was clicked by script: focus never left `active`.
  if (active instanceof HTMLElement && active.isConnected && document.activeElement !== active) throw new Error('reload moved focus');
}

describe.each(['desktop', 'compact'] as const)('a press on a gutter add button, %s', (layout) => {
  it('opens nothing from a press dragged off, and the next button\'s Enter opens on its own line', async () => {
    const { review } = await mountDiff(() => patch(), layout);
    const [, , onThree, onFour, , , , , onNine] = addButtons();
    const events = seen(onThree, ['pointerdown', 'click']);
    await pressAndRelease(onThree, rowText(onNine));
    expect([events.get('pointerdown'), events.get('click')]).toEqual([1, undefined]);
    expect(review.openEditors).toEqual([]);
    expect(document.activeElement).toBe(onThree);
    await userEvent.tab();
    expect(document.activeElement).toBe(onFour);
    await userEvent.keyboard('{Enter}');
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ state: 'placed', anchor: { newLine: 4 } }]));
  });

  it('forgets a touch the browser cancelled: a later click on that button acts on the line it shows then', async () => {
    let current = patch();
    const { review } = await mountDiff(() => current, layout);
    const onThree = addButtons()[2]!;
    const events = seen(onThree, ['pointerdown', 'pointercancel', 'click']);
    await touchCancelled(onThree);
    expect([events.get('pointerdown'), events.get('pointercancel'), events.get('click')]).toEqual([1, 1, undefined]);
    expect(review.openEditors).toEqual([]);
    current = patch(5);
    await reload(review);
    // A click no press began, on the button the touch was on.
    expect(onThree.isConnected).toBe(true);
    onThree.click();
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ state: 'placed', anchor: { newLine: 3, selectedText: 'inserted 3' } }]));
  });

  it('forgets a press dragged off: a later click on that button acts on the line it shows then', async () => {
    let current = patch();
    const { review } = await mountDiff(() => current, layout);
    const onThree = addButtons()[2]!;
    await pressAndRelease(onThree, rowText(addButtons()[8]!));
    expect(document.activeElement).toBe(onThree);
    current = patch(5);
    await reload(review);
    expect(onThree.isConnected).toBe(true);
    onThree.click();
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ state: 'placed', anchor: { newLine: 3, selectedText: 'inserted 3' } }]));
  });

  it('forgets a held key once focus leaves the button', async () => {
    let current = patch();
    const { review } = await mountDiff(() => current, layout);
    const onThree = addButtons()[2]!;
    onThree.focus();
    await userEvent.keyboard('[Space>]');
    await userEvent.tab();
    expect(document.activeElement).not.toBe(onThree);
    current = patch(5);
    await reload(review);
    expect(onThree.isConnected).toBe(true);
    // The key is still down: only the blur ended the activation.
    onThree.click();
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ state: 'placed', anchor: { newLine: 3, selectedText: 'inserted 3' } }]));
    await userEvent.keyboard('[/Space]');
  });

  it('acts on its own line for a click while another button\'s key is held', async () => {
    const { review } = await mountDiff(() => patch(), layout);
    const [, , onThree, onFour] = addButtons();
    onThree.focus();
    await userEvent.keyboard('[Space>]');
    onFour.click();
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ anchor: { newLine: 4 } }]));
    await userEvent.keyboard('[/Space]');
  });

  it('acts on neither button for a press on one released on another', async () => {
    const { review } = await mountDiff(() => patch(), layout);
    const [, , onThree, onFour] = addButtons();
    await pressAndRelease(onThree, onFour);
    expect(review.openEditors).toEqual([]);
    onFour.focus();
    await userEvent.keyboard('{Enter}');
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ anchor: { newLine: 4 } }]));
  });

  it('acts on the line it was pressed on when Space is held across a reload, and the next press on the line now there', async () => {
    let current = patch();
    const { review } = await mountDiff(() => current, layout);
    const onThree = addButtons()[2]!;
    onThree.focus();
    await userEvent.keyboard('[Space>]');
    current = patch(5);
    await reloadKeepingFocus(review);
    // The same button now shows another line.
    expect(onThree.isConnected).toBe(true);
    expect(rowText(onThree).textContent).toContain('inserted 3');
    await userEvent.keyboard('[/Space]');
    await frames(2);
    // Pressed on the diff before the reload: refused, never opened on the
    // line that replaced it.
    expect(review.openEditors).toEqual([]);
    await userEvent.keyboard('[Space>][/Space]');
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ state: 'placed', anchor: { newLine: 3, selectedText: 'inserted 3' } }]));
  });

  it('opens on its line for Enter, and for a click no press began', async () => {
    const { review } = await mountDiff(() => patch(), layout);
    addButtons()[2]!.focus();
    await userEvent.keyboard('{Enter}');
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ anchor: { newLine: 3 } }]));
    review.closeDraftEditor(review.openEditors[0]!.id);
    await frames(2);
    addButtons()[3]!.click();
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ anchor: { newLine: 4 } }]));
  });

  it('moves an armed editor to the line whose button was activated, not one pressed and dragged off before', async () => {
    let current = patch();
    const { review, id } = await focusedEditor(() => current, layout);
    current = patch(5);
    await reload(review);
    await vi.waitFor(() => expect(box()?.getAttribute('data-state')).toBe('detached'));
    button(box()!, 'Move').click();
    await vi.waitFor(() => expect(review.armedEditorId).toBe(id));
    const buttons = addButtons();
    const [, , onThree, onFour] = buttons;
    await pressAndRelease(onThree, rowText(buttons[8]!));
    expect(review.armedEditorId).toBe(id);
    await userEvent.tab();
    expect(document.activeElement).toBe(onFour);
    await userEvent.keyboard('{Enter}');
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ id, state: 'placed', anchor: { newLine: 4 } }]));
  });
});

describe.each(['desktop', 'compact'] as const)('a press on "Use this line", %s', (layout) => {
  // An editor detached by a reload that moved its line five rows down.
  async function detachedEditor() {
    let current = patch();
    const opened = await focusedEditor(() => current, layout);
    current = patch(5);
    await reload(opened.review);
    await vi.waitFor(() => expect(opened.review.openEditors).toMatchObject([{ id: opened.id, state: 'detached', suggested: { newLine: 45 } }]));
    return { ...opened, setCurrent: (next: string) => { current = next; } };
  }

  it('is forgotten when dragged off, so Enter after a reload takes the suggestion shown then', async () => {
    const { review, id, setCurrent } = await detachedEditor();
    const use = button(box()!, 'Use this line');
    const events = seen(use, ['pointerdown', 'click']);
    await pressAndRelease(use, textarea());
    expect([events.get('pointerdown'), events.get('click')]).toEqual([1, undefined]);
    expect(review.openEditors).toMatchObject([{ id, state: 'detached' }]);
    setCurrent(patch(7));
    await reload(review);
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ id, state: 'detached', suggested: { newLine: 47 } }]));
    const now = button(box()!, 'Use this line');
    now.focus();
    await userEvent.keyboard('{Enter}');
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ id, state: 'placed', anchor: { newLine: 47 } }]));
  });

  it('takes nothing when Space is held across a reload, and the next press takes the new suggestion', async () => {
    const { review, id, setCurrent } = await detachedEditor();
    const use = button(box()!, 'Use this line');
    use.focus();
    await userEvent.keyboard('[Space>]');
    setCurrent(patch(7));
    await reloadKeepingFocus(review);
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ id, state: 'detached', suggested: { newLine: 47 } }]));
    expect(use.isConnected).toBe(true);
    await userEvent.keyboard('[/Space]');
    await frames(2);
    // Pressed on the suggestion before the reload: refused.
    expect(review.openEditors).toMatchObject([{ id, state: 'detached', suggested: { newLine: 47 } }]);
    expect(document.activeElement).toBe(use);
    await userEvent.keyboard('[Space>][/Space]');
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ id, state: 'placed', anchor: { newLine: 47 } }]));
  });

  it('takes the suggestion shown for a click no press began', async () => {
    const { review, id } = await detachedEditor();
    button(box()!, 'Use this line').click();
    await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ id, state: 'placed', anchor: { newLine: 45 } }]));
  });
});

// The key of the focused element: its control in an editor, else its test id.
function focusedKey(): string | null {
  const active = document.activeElement;
  return active?.getAttribute('data-editor-control') ?? active?.getAttribute('data-testid') ?? null;
}

function control(key: string): HTMLElement {
  return box()!.querySelector<HTMLElement>(`[data-editor-control="${key}"]`)!;
}

describe.each(['desktop', 'compact'] as const)('a refresh while a control of an editor has focus, %s', (layout) => {
  // An editor detached by a reload that moved its line five rows down, with
  // `key` focused; returns where Tab goes from it.
  async function focusedControl(key: string) {
    let current = patch();
    const opened = await focusedEditor(() => current, layout);
    current = patch(5);
    await reload(opened.review);
    await vi.waitFor(() => expect(opened.review.openEditors).toMatchObject([{ id: opened.id, state: 'detached' }]));
    await frames(2);
    const focused = control(key);
    focused.focus();
    if (focused instanceof HTMLTextAreaElement) focused.setSelectionRange(2, 6);
    await userEvent.tab();
    const next = focusedKey();
    await userEvent.tab({ shift: true });
    expect(document.activeElement).toBe(focused);
    return { ...opened, focused, next, setCurrent: (patchText: string) => { current = patchText; } };
  }

  for (const key of ['move', 'use-line', 'cancel', 'text']) {
    it(`leaves focus on ${key} when the box does not move, and Tab goes on from it`, async () => {
      const { review, focused, next, top, setCurrent } = await focusedControl(key);
      const before = top();
      // The same diff again: every row is rebuilt, the box stays.
      setCurrent(patch(5));
      await reload(review);
      expect(document.activeElement).toBe(focused);
      expect(Math.abs(top() - before)).toBeLessThanOrEqual(2);
      if (key === 'text') expect([textarea().selectionStart, textarea().selectionEnd]).toEqual([2, 6]);
      await userEvent.tab();
      expect(focusedKey()).toBe(next);
    });

    it(`leaves focus on ${key} when the box's row moves out of the rows the list holds, and Tab goes on from it`, async () => {
      const { review, id, focused, next, top, setCurrent } = await focusedControl(key);
      const before = top();
      setCurrent(patch(1000));
      await reload(review);
      await vi.waitFor(() => expect(review.openEditors).toMatchObject([{ id, state: 'detached', suggested: { newLine: 1040 } }]));
      await vi.waitFor(() => expect(document.activeElement).toBe(focused));
      expect(Math.abs(top() - before)).toBeLessThanOrEqual(2);
      await userEvent.tab();
      expect(focusedKey()).toBe(next);
    });
  }

  it('gives the text back its focus and caret when the box is mounted again elsewhere', async () => {
    const { review, focused, top, setCurrent } = await focusedControl('text');
    const before = top();
    // The file leaves the diff: the box is mounted again, in the group.
    setCurrent('');
    await reload(review);
    await vi.waitFor(() => expect(q('[data-testid="review-gone-editors"]')).not.toBeNull());
    expect(focused.isConnected).toBe(false);
    await vi.waitFor(() => expect(document.activeElement).toBe(textarea()));
    expect([textarea().selectionStart, textarea().selectionEnd]).toEqual([2, 6]);
    // The group is the list's one row: it stays where the list can put it.
    expect(top()).toBeLessThanOrEqual(before);
    await userEvent.tab();
    expect(focusedKey()).toBe('copy');
  });

  it('puts focus on the text when the box is mounted again with no control like the focused one', async () => {
    let current = patch();
    const { review } = await focusedEditor(() => current, layout);
    current = '';
    await reload(review);
    await vi.waitFor(() => expect(q('[data-testid="review-gone-editors"]')).not.toBeNull());
    const discard = control('discard');
    discard.focus();
    // The file is back: the box is mounted again in it, with no Discard.
    current = patch(5);
    await reload(review);
    await vi.waitFor(() => expect(box()?.getAttribute('data-state')).toBe('detached'));
    expect(discard.isConnected).toBe(false);
    await vi.waitFor(() => expect(focusedKey()).toBe('text'));
    // And from the file into the group again with Move focused: no Move.
    control('move').focus();
    current = '';
    await reload(review);
    await vi.waitFor(() => expect(q('[data-testid="review-gone-editors"]')).not.toBeNull());
    await vi.waitFor(() => expect(focusedKey()).toBe('text'));
  });
});
