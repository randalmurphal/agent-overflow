// Integration tests covering the command palette + keybindings + sidebar
// thread navigation working together through the full App mount.

import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Project, Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';
import { setPaneLayoutItemsForTest } from '../../lib/stores/paneLayout.svelte';
import { answerBackPress } from '../../lib/native/lifecycle';
import { registerCommand } from '../../lib/stores/commandRegistry.svelte';
import { getCompactScreen, setCompactLayoutForTest, showCompactThread } from '../../lib/stores/layoutMode.svelte';
import { collapseProject, expandProject } from '../../lib/stores/sidebar.svelte';
import { setThreadFilterQuery } from '../../lib/stores/threadFilter.svelte';
import { setSidebarCollapsed } from '../../lib/stores/sidebarLayout.svelte';
import { getSidebarJumpThreadIds } from '../../lib/stores/sidebarThreadOrder';

beforeAll(installAnimateShim);

async function loadKeybindingsFromMock(rules: Array<{
  key: string;
  command: string;
  when?: string;
  defaultId?: string;
  defaultKey?: string;
}>) {
  setBindingMock('GetKeybindings', async () => ({ bindings: rules }));
  const mod = await import('../../lib/stores/keybindings.svelte');
  await mod.loadKeybindings();
}

async function mountBareApp(threads: Thread[] = [], projects?: Project[]) {
  installAppDefaults();
  setBindingMock('ListThreads', async () => threads);
  // The sidebar is projects-first — each thread must live under a
  // project and its project must be expanded for the row to render.
  if (projects) {
    setBindingMock('ListProjects', async () => projects.map((project, index) => ({
      project,
      threadCount: threads.filter((thread) => thread.projectId === project.id).length,
      lastActive: projects.length - index,
    })));
    for (const project of projects) expandProject(project.id);
  } else if (threads.length > 0) seedSidebarProject(threads);
  // If any thread ends up active during the test, these are used.
  installThreadViewDefaults();
  for (const t of threads) installComposerDefaults(t.id);
  const rendered = render(App);
  await flush();
  return rendered;
}

async function waitForThreadStore(count: number): Promise<void> {
  const threadsMod = await import('../../lib/stores/threads.svelte');
  await waitFor(() => expect(threadsMod.getThreads()).toHaveLength(count));
}

describe('App integration — keybindings + palette', () => {
  beforeEach(() => {
    resetAppState();
  });

  it('Android Back navigates from a focused composer without executing its Escape shortcut', async () => {
    const thread = makeThread({ title: 'Keep this turn running' });
    const rendered = await mountBareApp([thread]);
    await fireEvent.click(rendered.getAllByText(thread.title)[0]);
    await flush(15);
    const interrupt = vi.fn();
    registerCommand({ id: 'thread.interrupt', label: 'Interrupt', editableReachable: true, run: interrupt });
    await loadKeybindingsFromMock([{ key: 'escape', command: 'thread.interrupt' }]);
    setCompactLayoutForTest(true);
    showCompactThread();
    try {
      rendered.getByLabelText('Message Input').focus();
      expect(answerBackPress()).toBe(true);
      expect(interrupt).not.toHaveBeenCalled();
      expect(getCompactScreen()).toBe('list');
      // Physical Escape keeps its configured keyboard meaning.
      await fireEvent.keyDown(window, { key: 'Escape' });
      expect(interrupt).toHaveBeenCalledOnce();
    } finally {
      setCompactLayoutForTest(false);
    }
  });

  it('Cmd+K opens the palette and focuses the list (mod+/ toggles to the input)', async () => {
    await loadKeybindingsFromMock([{ key: 'mod+k', command: 'palette.open' }]);
    const rendered = await mountBareApp();
    // Reload keybindings AFTER mount so the mount-triggered loadKeybindings
    // call doesn't wipe our rules.
    await loadKeybindingsFromMock([{ key: 'mod+k', command: 'palette.open' }]);

    await fireEvent.keyDown(window, { key: 'k', metaKey: true });
    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    await flush();

    const input = await waitFor(() => rendered.getByTestId('command-palette-input'));
    expect(input).toBeInTheDocument();
    // Default focus is the list root (so plain j/k navigates without
    // typing into the search box); mod+/ swings focus to the input.
    const list = rendered.container.querySelector<HTMLElement>('#palette-listbox');
    expect(list).not.toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(list));
  });

  it('filters commands as the user types', async () => {
    // Seed a thread so commands gated on `hasActiveThread` (archive,
    // rename, etc) are eligible for the filter to surface.
    const thread = makeThread({ title: 'To Filter' });
    const rendered = await mountBareApp([thread]);
    const rows = rendered.getAllByText(thread.title);
    await fireEvent.click(rows[0]);
    await flush(15);

    const { openPalette } = await import('../../lib/stores/palette.svelte');
    openPalette();
    await flush();

    const input = rendered.getByTestId('command-palette-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'archive' } });
    await flush();
    // Every visible option should match "archive" fuzzy.
    const options = rendered.getAllByRole('option');
    expect(options.length).toBeGreaterThan(0);
    for (const opt of options) {
      const txt = opt.textContent?.replace(/\s+/g, ' ').trim() ?? '';
      expect(txt.toLowerCase()).toContain('archive');
    }
  });

  it('Enter executes the highlighted command', async () => {
    const thread = makeThread({ title: 'To Be Archived' });
    const archiveMock = setBindingMock('ArchiveThread', async () => {});
    setBindingMock('StopSession', async () => {});
    const rendered = await mountBareApp([thread]);
    // Activate the thread so `hasActiveThread` when-gate lets thread.archive
    // surface.
    const rows = rendered.getAllByText(thread.title);
    await fireEvent.click(rows[0]);
    await flush(15);

    const { openPalette } = await import('../../lib/stores/palette.svelte');
    openPalette();
    await flush();
    const input = rendered.getByTestId('command-palette-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'thread archive' } });
    await flush();
    await fireEvent.keyDown(input, { key: 'Enter' });
    await flush(10);

    expect(archiveMock).not.toHaveBeenCalled();
    await waitFor(() => expect(rendered.getByRole('dialog', { name: 'Archive Thread' })).toBeInTheDocument());
    await fireEvent.click(rendered.getByRole('button', { name: 'Archive' }));
    await flush(10);

    await waitFor(() => expect(archiveMock).toHaveBeenCalled());
  });

  it('palette thread delete requires confirmation before deleting', async () => {
    const thread = makeThread({ title: 'To Be Deleted' });
    const deleteMock = setBindingMock('DeleteThread', async () => {});
    setBindingMock('StopSession', async () => {});
    const rendered = await mountBareApp([thread]);
    const rows = rendered.getAllByText(thread.title);
    await fireEvent.click(rows[0]);
    await flush(15);

    const { openPalette } = await import('../../lib/stores/palette.svelte');
    openPalette();
    await flush();
    const input = rendered.getByTestId('command-palette-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'thread delete' } });
    await flush();
    await fireEvent.keyDown(input, { key: 'Enter' });
    await flush(10);

    expect(deleteMock).not.toHaveBeenCalled();
    await waitFor(() => expect(rendered.getByRole('dialog', { name: 'Delete Thread' })).toBeInTheDocument());
    await fireEvent.click(rendered.getByRole('button', { name: 'Delete' }));
    await flush(10);

    await waitFor(() => expect(deleteMock).toHaveBeenCalled());
  });

  it('Escape closes the palette and returns to the app shell', async () => {
    const rendered = await mountBareApp();
    const { openPalette } = await import('../../lib/stores/palette.svelte');
    openPalette();
    await flush();
    const input = rendered.getByTestId('command-palette-input') as HTMLInputElement;
    await fireEvent.keyDown(input, { key: 'Escape' });
    await flush();
    expect(rendered.queryByTestId('command-palette-backdrop')).toBeNull();
  });

  it('numbers front-burner pins across projects and respects collapse and search', async () => {
    const projects: Project[] = ['proj-int', 'proj-next'].map((id, index) => ({
      id, name: id, path: `/tmp/project-${index}`, sortPosition: index,
      createdAt: 0, updatedAt: 0, archived: false,
    }));
    const threads: Thread[] = [
      makeThread({ id: 't-1', title: 'Thread One', pinnedAt: 1, updatedAt: 100 }),
      makeThread({ id: 't-2', title: 'Thread Two', pinnedAt: 1, updatedAt: 90 }),
      makeThread({ id: 't-3', title: 'Thread Three', pinnedAt: 1, updatedAt: 80 }),
      makeThread({ id: 'back', title: 'Back Burner', pinnedAt: 1, pinGroup: 1 }),
      makeThread({ id: 'unpinned', title: 'Unpinned' }),
      makeThread({ id: 't-4', title: 'Thread Four', pinnedAt: 1, projectId: 'proj-next', updatedAt: 70 }),
      makeThread({ id: 't-5', title: 'Thread Five', pinnedAt: 1, projectId: 'proj-next', updatedAt: 60 }),
    ];
    await mountBareApp(threads, projects);
    await waitForThreadStore(threads.length);
    await loadKeybindingsFromMock([
      { key: 'mod+1', command: 'thread.jump.1' },
      { key: 'mod+4', command: 'thread.jump.4' },
      { key: 'mod+9', command: 'thread.jump.9' },
    ]);
    await waitFor(() => expect(getSidebarJumpThreadIds()).toEqual(['t-1', 't-2', 't-3', 't-4', 't-5']));

    async function jump(key: string) {
      await fireEvent.keyDown(window, { key, metaKey: true });
      await fireEvent.keyDown(window, { key, ctrlKey: true });
      await flush(15);
    }

    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.ensureMainPane();
    // Jumping from the collapsed sidebar must wait for pin rows to mount.
    setSidebarCollapsed(true);
    await flush();
    await jump('4');
    await waitFor(() => expect(pane.thread?.id).toBe('t-4'));
    await jump('9');
    expect(pane.thread?.id).toBe('t-4');

    collapseProject('proj-int');
    await waitFor(() => expect(getSidebarJumpThreadIds()).toEqual(['t-4', 't-5']));
    await jump('1');
    expect(pane.thread?.id).toBe('t-4');

    setThreadFilterQuery('Thread Five');
    await waitFor(() => expect(getSidebarJumpThreadIds()).toEqual(['t-5']));
    await jump('1');
    await waitFor(() => expect(pane.thread?.id).toBe('t-5'));

    setThreadFilterQuery('nothing matches');
    await waitFor(() => expect(getSidebarJumpThreadIds()).toEqual([]));
    await jump('1');
    expect(pane.thread?.id).toBe('t-5');

    expandProject('proj-int');
    setThreadFilterQuery('Unpinned');
    await waitFor(() => expect(document.querySelector('[data-sidebar-thread-id="unpinned"]')).not.toBeNull());
    expect(getSidebarJumpThreadIds()).toEqual([]);
    await jump('1');
    expect(pane.thread?.id).toBe('t-5');
  });

  it('mod+1..9 jumps while the composer textarea is focused', async () => {
    const threads: Thread[] = [
      makeThread({ id: 't-1', title: 'Thread One', pinnedAt: 1 }),
      makeThread({ id: 't-2', title: 'Thread Two', pinnedAt: 1 }),
      makeThread({ id: 't-3', title: 'Thread Three', pinnedAt: 1 }),
    ];
    const rendered = await mountBareApp(threads);
    await waitForThreadStore(threads.length);
    await loadKeybindingsFromMock([
      { key: 'mod+1', command: 'thread.jump.1' },
      { key: 'ctrl+alt+2', command: 'thread.jump.2' },
      { key: 'mod+3', command: 'thread.jump.3' },
    ]);

    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.ensureMainPane();
    await pane.switchThread(threads[0]);
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 1 }]);
    await flush(15);

    const input = rendered.getByLabelText('Message Input') as HTMLTextAreaElement;
    input.focus();
    await fireEvent.keyDown(input, { key: '2', ctrlKey: true, altKey: true });
    await flush(15);

    await waitFor(() => expect(pane.thread?.id).toBe('t-2'));
  });

  // Regression: pane navigation/management chords used to be eaten by the
  // editable bail-out. The default pane-nav chords are alt+h/l so macOS
  // Option+Arrow remains available for normal word-motion.
  it('default pane focus / move / close chords fire while the composer textarea is focused', async () => {
    const threads: Thread[] = [
      makeThread({ id: 't-1', title: 'Thread One' }),
      makeThread({ id: 't-2', title: 'Thread Two' }),
    ];
    const rendered = await mountBareApp(threads);
    await waitForThreadStore(threads.length);
    await loadKeybindingsFromMock([
      { key: 'alt+h', command: 'pane.focusLeft' },
      { key: 'alt+shift+l', command: 'pane.moveRight' },
      { key: 'mod+w', command: 'pane.close', when: '!terminalFocus' },
    ]);

    const panesMod = await import('../../lib/stores/panes.svelte');
    const layoutMod = await import('../../lib/stores/paneLayout.svelte');
    const main = panesMod.ensureMainPane();
    await main.switchThread(threads[0]);
    const secondary = panesMod.createPane('secondary');
    secondary.replaceThread(threads[1]);
    layoutMod.setPaneLayoutItemsForTest([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 1 },
      { id: 'secondary', paneId: 'secondary', kind: 'thread', widthPx: 1 },
    ]);
    panesMod.focusPane('secondary');
    await flush(15);

    // With two panes mounted there are two composer textareas; either is
    // fine as the event target since handleGlobalKeydown reads its
    // `editable` predicate off `ev.target` only.
    const input = rendered.getAllByLabelText('Message Input')[0] as HTMLTextAreaElement;
    input.focus();
    input.value = 'alpha beta';
    input.setSelectionRange(input.value.length, input.value.length);

    const focusedBeforeArrow = panesMod.getFocusedPaneId();
    await fireEvent.keyDown(input, { key: 'ArrowLeft', altKey: true });
    await flush();
    expect(panesMod.getFocusedPaneId()).toBe(focusedBeforeArrow);
    expect(input.selectionStart).toBe(6);
    expect(input.selectionEnd).toBe(6);

    await fireEvent.keyDown(input, { key: 'h', altKey: true });
    await flush();
    await waitFor(() => expect(panesMod.getFocusedPaneId()).toBe('main'));

    // alt+shift+l should move the focused pane (reorder).
    panesMod.focusPane('main');
    await flush();
    await fireEvent.keyDown(input, { key: 'l', altKey: true, shiftKey: true });
    await flush();
    await waitFor(() =>
      expect(layoutMod.getPaneLayoutItems().map((item) => item.paneId)).toEqual([
        'secondary',
        'main',
      ]),
    );

    // mod+w should close the focused pane.
    await fireEvent.keyDown(input, { key: 'w', ctrlKey: true });
    await fireEvent.keyDown(input, { key: 'w', metaKey: true });
    await flush();
    await waitFor(() =>
      expect(layoutMod.getPaneLayoutItems().map((item) => item.paneId)).toEqual(['secondary']),
    );
  });

  it('pane focus can be rebound to alt+arrow without changing dispatcher code', async () => {
    const threads: Thread[] = [
      makeThread({ id: 't-1', title: 'Thread One' }),
      makeThread({ id: 't-2', title: 'Thread Two' }),
    ];
    const rendered = await mountBareApp(threads);
    await waitForThreadStore(threads.length);
    await loadKeybindingsFromMock([
      {
        key: 'alt+arrowleft',
        command: 'pane.focusLeft',
        defaultId: 'pane.focusLeft.vim',
        defaultKey: 'alt+h',
      },
    ]);

    const panesMod = await import('../../lib/stores/panes.svelte');
    const layoutMod = await import('../../lib/stores/paneLayout.svelte');
    const main = panesMod.ensureMainPane();
    await main.switchThread(threads[0]);
    const secondary = panesMod.createPane('secondary');
    secondary.replaceThread(threads[1]);
    layoutMod.setPaneLayoutItemsForTest([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 1 },
      { id: 'secondary', paneId: 'secondary', kind: 'thread', widthPx: 1 },
    ]);
    panesMod.focusPane('secondary');
    await flush(15);

    const input = rendered.getAllByLabelText('Message Input')[0] as HTMLTextAreaElement;
    input.focus();

    await fireEvent.keyDown(input, { key: 'ArrowLeft', altKey: true });
    await flush();
    await waitFor(() => expect(panesMod.getFocusedPaneId()).toBe('main'));
  });

  // Regression: pressing alt+h from a textarea should not only flip the
  // pane focus indicator but also move DOM focus to the newly focused
  // pane's composer — otherwise the user's next keystroke still lands in
  // the previously focused pane and the chord feels like a no-op.
  it('alt+h moves DOM focus to the newly focused pane composer', async () => {
    const threads: Thread[] = [
      makeThread({ id: 't-1', title: 'Thread One' }),
      makeThread({ id: 't-2', title: 'Thread Two' }),
    ];
    const rendered = await mountBareApp(threads);
    await waitForThreadStore(threads.length);
    await loadKeybindingsFromMock([
      { key: 'alt+h', command: 'pane.focusLeft', when: '!terminalFocus' },
    ]);

    const panesMod = await import('../../lib/stores/panes.svelte');
    const layoutMod = await import('../../lib/stores/paneLayout.svelte');
    const main = panesMod.ensureMainPane();
    await main.switchThread(threads[0]);
    const secondary = panesMod.createPane('secondary');
    secondary.replaceThread(threads[1]);
    layoutMod.setPaneLayoutItemsForTest([
      { id: 'main', paneId: 'main', kind: 'thread', widthPx: 1 },
      { id: 'secondary', paneId: 'secondary', kind: 'thread', widthPx: 1 },
    ]);
    panesMod.focusPane('secondary');
    await flush(15);

    // Focus the SECONDARY pane's composer textarea — alt+h should move pane
    // focus AND DOM focus to the MAIN pane's composer.
    const secondaryPaneEl = rendered.container.querySelector<HTMLElement>(
      '[data-pane-id="secondary"]',
    );
    expect(secondaryPaneEl).not.toBeNull();
    const secondaryInput = secondaryPaneEl!.querySelector<HTMLTextAreaElement>(
      'textarea[aria-label="Message Input"]',
    )!;
    secondaryInput.focus();
    expect(document.activeElement).toBe(secondaryInput);

    await fireEvent.keyDown(secondaryInput, { key: 'h', altKey: true });
    await flush();

    await waitFor(() => expect(panesMod.getFocusedPaneId()).toBe('main'));
    const mainPaneEl = rendered.container.querySelector<HTMLElement>(
      '[data-pane-id="main"]',
    )!;
    const mainInput = mainPaneEl.querySelector<HTMLTextAreaElement>(
      'textarea[aria-label="Message Input"]',
    )!;
    await waitFor(() => expect(document.activeElement).toBe(mainInput));
  });
});
