import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import KeybindingsCheatSheet from './KeybindingsCheatSheet.svelte';
import {
  registerCommand,
  clearCommandRegistry,
} from '../../stores/commandRegistry.svelte';
import { setKeybindingsForTest, resetKeybindingsStore } from '../../stores/keybindings.svelte';
import { installAnimateShim } from '../../../test/integration/_helpers';

beforeAll(installAnimateShim);

beforeEach(() => {
  clearCommandRegistry();
  resetKeybindingsStore();
});

function seedThreeCommands() {
  registerCommand({ id: 'thread.new', label: 'Thread: New', run: () => {} });
  registerCommand({ id: 'thread.archive', label: 'Thread: Archive', run: () => {} });
  registerCommand({
    id: 'terminal.toggle',
    label: 'Terminal: Toggle',
    description: 'Show or hide the drawer.',
    run: () => {},
  });
}

function bindDefault() {
  setKeybindingsForTest([
    { key: 'mod+n', command: 'thread.new' },
    { key: 'mod+shift+a', command: 'thread.archive' },
    // terminal.toggle intentionally unbound.
  ]);
}

// ---- Visibility ----

describe('<KeybindingsCheatSheet> — visibility', () => {
  it('does not render anything when closed', () => {
    seedThreeCommands();
    const { queryByTestId } = render(KeybindingsCheatSheet, { open: false, onClose: vi.fn() });
    expect(queryByTestId('keybindings-cheatsheet')).toBeNull();
  });

  it('renders the dialog when open', () => {
    seedThreeCommands();
    const { getByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    expect(getByTestId('keybindings-cheatsheet')).toBeInTheDocument();
  });
});

// ---- Grouping + rendering ----

describe('<KeybindingsCheatSheet> — grouping and rows', () => {
  it('lists every registered command grouped by category prefix', () => {
    seedThreeCommands();
    const { getByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    expect(getByTestId('keybindings-cheatsheet-group-Thread')).toBeInTheDocument();
    expect(getByTestId('keybindings-cheatsheet-group-Terminal')).toBeInTheDocument();
    expect(getByTestId('keybindings-cheatsheet-row-thread.new')).toBeInTheDocument();
    expect(getByTestId('keybindings-cheatsheet-row-thread.archive')).toBeInTheDocument();
    expect(getByTestId('keybindings-cheatsheet-row-terminal.toggle')).toBeInTheDocument();
  });

  it('shows the bound chord for commands with a keybinding', () => {
    seedThreeCommands();
    bindDefault();
    const { getByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    const chord = getByTestId('keybindings-cheatsheet-chord-thread.new');
    expect(chord).toBeInTheDocument();
    // The raw chord is preserved in the `title` attribute for fidelity.
    expect(chord.getAttribute('title')).toBe('mod+n');
  });

  it('shows "unbound" text for commands that have no keybinding', () => {
    seedThreeCommands();
    bindDefault();
    const { getByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    expect(getByTestId('keybindings-cheatsheet-unbound-terminal.toggle')).toBeInTheDocument();
  });

  it('renders the description when the command defines one', () => {
    seedThreeCommands();
    const { getByText } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    expect(getByText('Show or hide the drawer.')).toBeInTheDocument();
  });

  it('renders the command id alongside the label for discoverability', () => {
    seedThreeCommands();
    const { getByText } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    expect(getByText('thread.new')).toBeInTheDocument();
    expect(getByText('thread.archive')).toBeInTheDocument();
    expect(getByText('terminal.toggle')).toBeInTheDocument();
  });

  it('alphabetizes rows within a category', () => {
    registerCommand({ id: 'z.alpha', label: 'Z: Alpha', run: () => {} });
    registerCommand({ id: 'a.alpha', label: 'A: Alpha', run: () => {} });
    registerCommand({ id: 'a.beta', label: 'A: Beta', run: () => {} });
    const { container } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    const groupOrder = Array.from(container.querySelectorAll('section h3')).map((h) => h.textContent?.trim());
    expect(groupOrder).toEqual(['A', 'Z']);
    const aRows = container.querySelectorAll(
      '[data-testid^="keybindings-cheatsheet-row-a."]',
    );
    expect(Array.from(aRows).map((r) => r.textContent)).toEqual([
      expect.stringContaining('A: Alpha'),
      expect.stringContaining('A: Beta'),
    ]);
  });

  it('groups commands without a dot under "General"', () => {
    registerCommand({ id: 'help', label: 'Show help', run: () => {} });
    const { getByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    expect(getByTestId('keybindings-cheatsheet-group-General')).toBeInTheDocument();
    expect(getByTestId('keybindings-cheatsheet-row-help')).toBeInTheDocument();
  });
});

// ---- Search ----

describe('<KeybindingsCheatSheet> — search', () => {
  it('filters rows by label substring (case-insensitive)', async () => {
    seedThreeCommands();
    const { getByTestId, queryByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    await fireEvent.input(getByTestId('keybindings-cheatsheet-search'), { target: { value: 'ARCHIVE' } });
    expect(getByTestId('keybindings-cheatsheet-row-thread.archive')).toBeInTheDocument();
    expect(queryByTestId('keybindings-cheatsheet-row-thread.new')).toBeNull();
    expect(queryByTestId('keybindings-cheatsheet-row-terminal.toggle')).toBeNull();
  });

  it('filters by command id', async () => {
    seedThreeCommands();
    const { getByTestId, queryByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    await fireEvent.input(getByTestId('keybindings-cheatsheet-search'), { target: { value: 'terminal.' } });
    expect(getByTestId('keybindings-cheatsheet-row-terminal.toggle')).toBeInTheDocument();
    expect(queryByTestId('keybindings-cheatsheet-row-thread.new')).toBeNull();
  });

  it('filters by chord text', async () => {
    seedThreeCommands();
    bindDefault();
    const { getByTestId, queryByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    await fireEvent.input(getByTestId('keybindings-cheatsheet-search'), { target: { value: 'shift' } });
    expect(getByTestId('keybindings-cheatsheet-row-thread.archive')).toBeInTheDocument();
    expect(queryByTestId('keybindings-cheatsheet-row-thread.new')).toBeNull();
  });

  it('filters by description', async () => {
    seedThreeCommands();
    const { getByTestId, queryByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    await fireEvent.input(getByTestId('keybindings-cheatsheet-search'), { target: { value: 'drawer' } });
    expect(getByTestId('keybindings-cheatsheet-row-terminal.toggle')).toBeInTheDocument();
    expect(queryByTestId('keybindings-cheatsheet-row-thread.new')).toBeNull();
  });

  it('shows an empty-state message when nothing matches', async () => {
    seedThreeCommands();
    const { getByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    await fireEvent.input(getByTestId('keybindings-cheatsheet-search'), { target: { value: 'xyzzy' } });
    expect(getByTestId('keybindings-cheatsheet-empty').textContent).toContain('xyzzy');
  });

  it('resets the search on reopen', async () => {
    seedThreeCommands();
    const { getByTestId, queryByTestId, rerender } = render(KeybindingsCheatSheet, {
      open: true,
      onClose: vi.fn(),
    });
    await fireEvent.input(getByTestId('keybindings-cheatsheet-search'), { target: { value: 'archive' } });
    expect(queryByTestId('keybindings-cheatsheet-row-thread.new')).toBeNull();
    await rerender({ open: false, onClose: vi.fn() });
    await rerender({ open: true, onClose: vi.fn() });
    expect(
      (getByTestId('keybindings-cheatsheet-search') as HTMLInputElement).value,
    ).toBe('');
    // Full list is visible again.
    expect(getByTestId('keybindings-cheatsheet-row-thread.new')).toBeInTheDocument();
  });
});

// ---- Close interactions ----

describe('<KeybindingsCheatSheet> — closing', () => {
  it('calls onClose when Close is clicked', async () => {
    seedThreeCommands();
    const onClose = vi.fn();
    const { getByTestId } = render(KeybindingsCheatSheet, { open: true, onClose });
    await fireEvent.click(getByTestId('keybindings-cheatsheet-close'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('calls onClose on Escape', async () => {
    seedThreeCommands();
    const onClose = vi.fn();
    const { container } = render(KeybindingsCheatSheet, { open: true, onClose });
    const backdrop = container.querySelector('[data-modal-backdrop]')!;
    await fireEvent.keyDown(backdrop, { key: 'Escape' });
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('calls onClose on backdrop click (but not on dialog click)', async () => {
    seedThreeCommands();
    const onClose = vi.fn();
    const { getByTestId, container } = render(KeybindingsCheatSheet, { open: true, onClose });
    const backdrop = container.querySelector('[data-modal-backdrop]') as HTMLElement;
    const dialog = getByTestId('keybindings-cheatsheet');
    // Clicking the dialog should NOT close (event.target != currentTarget).
    await fireEvent.click(dialog);
    expect(onClose).not.toHaveBeenCalled();
    // Clicking the backdrop directly closes.
    await fireEvent.click(backdrop);
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});

// ---- Chord pretty-printing ----

describe('<KeybindingsCheatSheet> — chord display', () => {
  it('renders a single-character chord with the character in caps', () => {
    registerCommand({ id: 'x', label: 'X', run: () => {} });
    setKeybindingsForTest([{ key: 'k', command: 'x' }]);
    const { getByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    const chord = getByTestId('keybindings-cheatsheet-chord-x');
    expect(chord.textContent?.trim()).toBe('K');
  });

  it('renders the raw chord in the title attribute regardless of pretty-print', () => {
    registerCommand({ id: 'x', label: 'X', run: () => {} });
    setKeybindingsForTest([{ key: 'mod+shift+p', command: 'x' }]);
    const { getByTestId } = render(KeybindingsCheatSheet, { open: true, onClose: vi.fn() });
    expect(getByTestId('keybindings-cheatsheet-chord-x').getAttribute('title')).toBe('mod+shift+p');
  });
});
