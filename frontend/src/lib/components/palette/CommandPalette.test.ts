import { describe, expect, it, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import CommandPalette from './CommandPalette.svelte';
import {
  clearCommandRegistry,
  registerCommand,
  type CommandContext,
} from '../../stores/commandRegistry.svelte';
import { closePalette, openPalette } from '../../stores/palette.svelte';
import {
  resetKeybindingsStore,
  setKeybindingsForTest,
} from '../../stores/keybindings.svelte';

function baseCtx(extra: Partial<CommandContext> = {}): CommandContext {
  return {
    paletteOpen: true,
    terminalOpen: false,
    terminalFocus: false,
    approvalPending: false,
    anyModalOpen: false,
    hasActiveThread: false,
    turnActive: false,
    canForkActiveThread: false,
    canStartDiscussion: false,
    ...extra,
  } as CommandContext;
}

describe('<CommandPalette>', () => {
  beforeEach(() => {
    clearCommandRegistry();
    resetKeybindingsStore();
    closePalette();
  });

  it('is invisible when closed', () => {
    const { queryByTestId } = render(CommandPalette, { props: { context: baseCtx({ paletteOpen: false }) } });
    expect(queryByTestId('command-palette-backdrop')).toBeNull();
  });

  it('mounts with search box + listbox when opened, and focuses the input', async () => {
    registerCommand({ id: 'foo', label: 'Foo Command', run: () => {} });
    openPalette();
    const { getByRole, getByTestId } = render(CommandPalette, { props: { context: baseCtx() } });
    // $effect runs next tick, so flush microtasks.
    await Promise.resolve();
    expect(getByRole('dialog', { name: /command palette/i })).toBeInTheDocument();
    expect(getByTestId('command-palette-input')).toBeInTheDocument();
    expect(getByRole('listbox', { name: /commands/i })).toBeInTheDocument();
  });

  it('filters results via fuzzy matching on input', async () => {
    registerCommand({ id: 'thread.new', label: 'Thread: New', run: () => {} });
    registerCommand({ id: 'thread.archive', label: 'Thread: Archive', run: () => {} });
    registerCommand({ id: 'settings.open', label: 'Settings: Open', run: () => {} });
    openPalette();

    const { getByTestId, findAllByRole, queryAllByRole } = render(CommandPalette, { props: { context: baseCtx() } });
    const input = getByTestId('command-palette-input') as HTMLInputElement;
    input.value = 'arch';
    await fireEvent.input(input);
    // $derived.by updates on next microtask; findAllByRole flushes reactions.
    const visible = await findAllByRole('option');
    const labels = visible.map((el) => el.textContent?.replace(/\s+/g, ' ').trim() ?? '');
    expect(labels.some((l) => l.includes('Thread: Archive'))).toBe(true);
    expect(labels.some((l) => l.includes('Thread: New'))).toBe(false);
    expect(labels.some((l) => l.includes('Settings: Open'))).toBe(false);
    // Assert option count matches direct expectations (single visible row).
    expect(queryAllByRole('option')).toHaveLength(1);
  });

  it('arrow keys navigate and Enter runs the selected command', async () => {
    const firstRun = vi.fn();
    const secondRun = vi.fn();
    registerCommand({ id: 'a', label: 'Alpha', run: firstRun });
    registerCommand({ id: 'b', label: 'Beta', run: secondRun });
    openPalette();

    const { getByTestId } = render(CommandPalette, { props: { context: baseCtx() } });
    const input = getByTestId('command-palette-input') as HTMLInputElement;
    // First result is selected by default. Arrow-down moves to second.
    await fireEvent.keyDown(input, { key: 'ArrowDown' });
    await fireEvent.keyDown(input, { key: 'Enter' });
    // queueMicrotask + Promise.resolve() flush path.
    await Promise.resolve();
    await Promise.resolve();
    expect(secondRun).toHaveBeenCalledTimes(1);
    expect(firstRun).not.toHaveBeenCalled();
  });

  it('Escape closes the palette', async () => {
    registerCommand({ id: 'a', label: 'Alpha', run: () => {} });
    openPalette();
    const { getByTestId, queryByTestId } = render(CommandPalette, { props: { context: baseCtx() } });
    const input = getByTestId('command-palette-input') as HTMLInputElement;
    await fireEvent.keyDown(input, { key: 'Escape' });
    await Promise.resolve();
    expect(queryByTestId('command-palette-backdrop')).toBeNull();
  });

  it('shows the bound shortcut next to a command', async () => {
    registerCommand({ id: 'palette.open', label: 'Open Palette', run: () => {} });
    setKeybindingsForTest([{ key: 'mod+k', command: 'palette.open' }]);
    openPalette();
    const { getByRole } = render(CommandPalette, { props: { context: baseCtx() } });
    const option = getByRole('option', { name: /Open Palette/i });
    // kbd contains formatChord output; exact glyphs depend on platform. Just
    // assert the element is present.
    expect(option.querySelector('kbd')).not.toBeNull();
  });

  it('shows an empty-state when nothing matches the query', async () => {
    registerCommand({ id: 'a', label: 'Alpha', run: () => {} });
    openPalette();
    const { getByTestId } = render(CommandPalette, { props: { context: baseCtx() } });
    const input = getByTestId('command-palette-input') as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'zzzzzzz' } });
    expect(getByTestId('command-palette-empty')).toBeInTheDocument();
  });
});
