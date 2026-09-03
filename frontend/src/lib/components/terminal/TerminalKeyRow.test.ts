import { cleanup, render, screen } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import TerminalKeyRow from './TerminalKeyRow.svelte';

afterEach(() => cleanup());

function mount(over: { ctrlArmed?: boolean } = {}) {
  const onKey = vi.fn();
  const onToggleCtrl = vi.fn();
  render(TerminalKeyRow, {
    props: { onKey, onToggleCtrl, ctrlArmed: over.ctrlArmed ?? false },
  });
  return { onKey, onToggleCtrl };
}

function key(id: string): HTMLButtonElement {
  return screen.getByTestId(`terminal-key-${id}`) as HTMLButtonElement;
}

describe('TerminalKeyRow', () => {
  // The exact bytes are the contract: a wrong escape sequence is invisible in a
  // rendered test but wrong at the PTY, so pin every one.
  const SEQUENCES: ReadonlyArray<[string, string]> = [
    ['esc', '\x1b'],
    ['tab', '\t'],
    ['up', '\x1b[A'],
    ['down', '\x1b[B'],
    ['left', '\x1b[D'],
    ['right', '\x1b[C'],
    ['dash', '-'],
    ['slash', '/'],
    ['pipe', '|'],
    ['tilde', '~'],
  ];

  it.each(SEQUENCES)('emits the byte sequence for %s', (id, data) => {
    const { onKey } = mount();
    key(id).click();
    expect(onKey).toHaveBeenCalledTimes(1);
    expect(onKey).toHaveBeenCalledWith(data);
  });

  it('renders the keys in the ruled order, with Ctrl third', () => {
    mount();
    const row = screen.getByTestId('terminal-key-row');
    const ids = Array.from(row.querySelectorAll('button')).map((b) =>
      b.getAttribute('data-testid'),
    );
    expect(ids).toEqual([
      'terminal-key-esc',
      'terminal-key-tab',
      'terminal-key-ctrl',
      'terminal-key-up',
      'terminal-key-down',
      'terminal-key-left',
      'terminal-key-right',
      'terminal-key-dash',
      'terminal-key-slash',
      'terminal-key-pipe',
      'terminal-key-tilde',
    ]);
  });

  it('reports Ctrl through onToggleCtrl and emits no bytes of its own', () => {
    const { onKey, onToggleCtrl } = mount();
    key('ctrl').click();
    expect(onToggleCtrl).toHaveBeenCalledTimes(1);
    // Ctrl is a modifier, not a key: nothing reaches the PTY on its own press.
    expect(onKey).not.toHaveBeenCalled();
  });

  it('exposes the armed state as aria-pressed', () => {
    mount({ ctrlArmed: false });
    expect(key('ctrl').getAttribute('aria-pressed')).toBe('false');
    cleanup();
    mount({ ctrlArmed: true });
    expect(key('ctrl').getAttribute('aria-pressed')).toBe('true');
  });

  it('cancels pointerdown on every button so the terminal keeps focus', () => {
    mount();
    const row = screen.getByTestId('terminal-key-row');
    for (const button of Array.from(row.querySelectorAll('button'))) {
      // The browser focuses a button on pointerdown/mousedown. Cancelling that
      // default is what keeps the xterm textarea focused — and on a phone, what
      // keeps the soft keyboard up across a key-row tap.
      const event = new PointerEvent('pointerdown', { bubbles: true, cancelable: true });
      button.dispatchEvent(event);
      expect(event.defaultPrevented).toBe(true);
      // And they stay out of the tab order, so no tab-stop lands on them either.
      expect(button.getAttribute('tabindex')).toBe('-1');
    }
  });

  it('lets the row scroll horizontally when the keys overflow', () => {
    mount();
    const row = screen.getByTestId('terminal-key-row');
    expect(row.className).toContain('overflow-x-auto');
    expect(row.className).toContain('shrink-0');
    // pan-x, not auto: a vertical drag on the row must scroll the page/terminal
    // rather than being swallowed by the row.
    expect(row.getAttribute('style')).toContain('touch-action: pan-x');
  });
});
