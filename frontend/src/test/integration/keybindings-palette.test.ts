// Integration tests covering the command palette + keybindings + sidebar
// thread navigation working together through the full App mount.

import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread } from '../../lib/types/models';
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

beforeAll(installAnimateShim);

async function loadKeybindingsFromMock(rules: Array<{ key: string; command: string; when?: string }>) {
  setBindingMock('GetKeybindings', async () => rules);
  const mod = await import('../../lib/stores/keybindings.svelte');
  await mod.loadKeybindings();
}

async function mountBareApp(threads: Thread[] = []) {
  installAppDefaults();
  setBindingMock('ListThreads', async () => threads);
  // The sidebar is projects-first — each thread must live under a
  // project and its project must be expanded for the row to render.
  if (threads.length > 0) seedSidebarProject(threads);
  // If any thread ends up active during the test, these are used.
  installThreadViewDefaults();
  for (const t of threads) installComposerDefaults(t.id);
  const rendered = render(App);
  await flush();
  return rendered;
}

describe('App integration — keybindings + palette', () => {
  beforeEach(() => {
    resetAppState();
  });

  it('Cmd+K opens the palette and focuses its input', async () => {
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
    // Svelte's $effect focuses the input on next tick.
    await waitFor(() => expect(document.activeElement).toBe(input));
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

    await waitFor(() => expect(archiveMock).toHaveBeenCalled());
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

  it('mod+1..9 jumps to the thread at that index', async () => {
    const threads: Thread[] = [
      makeThread({ id: 't-1', title: 'Thread One' }),
      makeThread({ id: 't-2', title: 'Thread Two' }),
      makeThread({ id: 't-3', title: 'Thread Three' }),
    ];
    const rendered = await mountBareApp(threads);
    await loadKeybindingsFromMock([
      { key: 'mod+1', command: 'thread.jump.1' },
      { key: 'mod+2', command: 'thread.jump.2' },
      { key: 'mod+3', command: 'thread.jump.3' },
    ]);

    // Fire mod+2 chord.
    await fireEvent.keyDown(window, { key: '2', metaKey: true });
    await fireEvent.keyDown(window, { key: '2', ctrlKey: true });
    await flush(15);

    // Pane should now be showing thread 2. Check the pane state directly.
    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();
    await waitFor(() => expect(pane.thread?.id).toBe('t-2'));
  });
});
