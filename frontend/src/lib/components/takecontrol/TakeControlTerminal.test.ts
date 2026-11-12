import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';

import TakeControlTerminal from './TakeControlTerminal.svelte';
import { encodeTerminalInput } from '../../types/terminal';
import { isPaneFindOpen, resetPaneFindForTest } from '../../stores/paneFind.svelte';
import { getTerminalFocused } from '../terminal/terminalStore.svelte';
import { setResolvedThemeForTest } from '../../../test/mocks/resolvedThemeMock.svelte';

// xterm can't render under happy-dom, and these tests need to observe the exact
// sequence of write() calls (to prove the replay/live dedup), so swap in a fake
// Terminal that records each write as a decoded string and captures the onData
// handler so we can drive input. Mirrors TerminalBody.test.ts's fake.
const mocks = vi.hoisted(() => {
  const writes: string[] = [];
  const resizes: Array<[number, number]> = [];
  const decoder = new TextDecoder();
  let lastTerminal: FakeTerminal | null = null;
  const track = (terminal: FakeTerminal): void => {
    lastTerminal = terminal;
  };
  class FakeTerminal {
    options: Record<string, unknown> = {};
    dataHandler: ((data: string) => void) | null = null;
    keyEventHandler: ((event: KeyboardEvent) => boolean) | null = null;
    // Records every term.paste(...) so a test can prove the read-only lease
    // gates a right-click paste the same way it gates typed input.
    pastes: string[] = [];
    unicode = { activeVersion: '6' };
    constructor(options: Record<string, unknown> = {}) {
      this.options = { ...options };
      track(this);
    }
    textarea: HTMLTextAreaElement | null = null;
    loadAddon(): void {}
    open(mount?: HTMLElement): void {
      if (!mount) return;
      this.textarea = mount.ownerDocument.createElement('textarea');
      mount.appendChild(this.textarea);
    }
    onWriteParsed(): { dispose(): void } {
      return { dispose: (): void => {} };
    }
    onResize(): { dispose(): void } {
      return { dispose: (): void => {} };
    }
    hasSelection(): boolean {
      return false;
    }
    getSelection(): string {
      return '';
    }
    clearSelection(): void {}
    paste(data: string): void {
      this.pastes.push(data);
      // Real xterm emits pasted text through onData, which is where the
      // component's lease gate lives.
      this.dataHandler?.(data);
    }
    attachCustomKeyEventHandler(handler: (event: KeyboardEvent) => boolean): void {
      this.keyEventHandler = handler;
    }
    // Parsed on a later task, in order, as real xterm does; an empty write
    // only waits for the writes before it.
    write(data: Uint8Array | string, parsed?: () => void): void {
      const text = typeof data === 'string' ? data : decoder.decode(data);
      if (text !== '') writes.push(text);
      if (parsed) {
        setTimeout(() => {
          searches.push('parsed');
          parsed();
        });
      }
    }
    clear(): void {}
    reset(): void {}
    // The buffer find scans (terminalFind.svelte.ts): one row of text.
    // Its decorations log on searches, each disposal once.
    bufferText = 'needle needle';
    get buffer() {
      const text = this.bufferText;
      const cell = { chars: '', getWidth: (): number => 1, getChars(): string { return this.chars; } };
      const line = {
        isWrapped: false,
        length: 80,
        getCell(x: number) {
          cell.chars = text[x] ?? '';
          return cell;
        },
        translateToString(trim?: boolean, start = 0, end = 80): string {
          const slice = text.padEnd(80).slice(start, end);
          return trim ? slice.trimEnd() : slice;
        },
      };
      return { active: { type: 'normal', length: 1, baseY: 0, cursorY: 0, viewportY: 0, getNullCell: () => cell, getLine: (row: number) => (row === 0 ? line : undefined) } };
    }
    registerMarker() {
      const owned: Array<() => void> = [];
      return {
        line: 0,
        isDisposed: false,
        owned,
        dispose(): void {
          this.isDisposed = true;
          for (const dispose of owned.splice(0)) dispose();
        },
      };
    }
    registerDecoration({ marker }: { marker: { owned: Array<() => void> } }) {
      searches.push('decorate');
      let live = true;
      const dispose = (): void => {
        if (!live) return;
        live = false;
        searches.push('undecorate');
      };
      marker.owned.push(dispose);
      return { onRender: () => ({ dispose(): void {} }), dispose };
    }
    scrollLines(): void {}
    resize(cols: number, rows: number): void {
      resizes.push([cols, rows]);
    }
    onData(cb: (data: string) => void): { dispose(): void } {
      this.dataHandler = cb;
      return { dispose: (): void => {} };
    }
    focus(): void {}
    dispose(): void {}
    get rows(): number {
      return 24;
    }
    get cols(): number {
      return 80;
    }
  }
  // What the parse and the mirror's find did, in order.
  const searches: string[] = [];
  return {
    writes,
    resizes,
    searches,
    FakeTerminal,
    getLastTerminal: (): FakeTerminal | null => lastTerminal,
    // Reset between tests so a test waiting on `getLastTerminal()` observes its
    // OWN component's terminal, not a stale one left mounted by a prior test
    // (whose dataHandler is already set, which would let a waitFor pass early).
    resetLastTerminal: (): void => {
      lastTerminal = null;
    },
    ProviderTerminalAttach: vi.fn(),
    ProviderTerminalReplay: vi.fn(),
    ProviderTerminalInput: vi.fn(async () => {}),
    ProviderTerminalResize: vi.fn(async () => {}),
    ProviderTerminalSetControl: vi.fn(async () => {}),
    ProviderTerminalDetach: vi.fn(async () => {}),
    wailsEventOn: vi.fn(),
    addToast: vi.fn(),
    addErrorToast: vi.fn(),
  };
});

vi.mock('@xterm/xterm', () => ({ Terminal: mocks.FakeTerminal }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { fit(): void {} } }));
vi.mock('@xterm/addon-unicode11', () => ({ Unicode11Addon: class {} }));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@xterm/addon-webgl', () => ({
  WebglAddon: class {
    onContextLoss(): void {}
    dispose(): void {}
  },
}));
vi.mock('../../stores/keybindings.svelte', () => ({
  eventEscapesTerminalToCommand: vi.fn(() => false),
}));
vi.mock('../../utils/clipboard', () => ({ copyToClipboard: vi.fn(async () => true) }));
vi.mock('../../stores/toast.svelte', () => ({ addToast: mocks.addToast, addErrorToast: mocks.addErrorToast }));
// Spread the real module: the bridge already degrades to an empty ITheme
// without a canvas, and a factory that lists only the export this component
// uses turns every LATER export of that module into `undefined` for the whole
// file (frontend/AGENTS.md § Testing). Only the console warning is stubbed.
vi.mock('../terminal/terminalTheme', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../terminal/terminalTheme')>()),
  getXtermTheme: () => ({}),
}));
// A theme a test can flip, to drive the palette effect.
vi.mock('../../stores/themeMode.svelte', async () => ({
  getResolvedTheme: (await import('../../../test/mocks/resolvedThemeMock.svelte')).getResolvedThemeMock,
}));
vi.mock('../../stores/wailsEvents', () => ({ wailsEventOn: mocks.wailsEventOn }));
vi.mock('../../stores/bindings', () => ({
  ProviderTerminalAttach: mocks.ProviderTerminalAttach,
  ProviderTerminalReplay: mocks.ProviderTerminalReplay,
  ProviderTerminalInput: mocks.ProviderTerminalInput,
  ProviderTerminalResize: mocks.ProviderTerminalResize,
  ProviderTerminalSetControl: mocks.ProviderTerminalSetControl,
  ProviderTerminalDetach: mocks.ProviderTerminalDetach,
}));

function b64(text: string): string {
  return btoa(text);
}

let outputHandler: ((payload: unknown) => void) | null = null;

function emit(sequence: number, text: string, threadID = 'thread-1'): void {
  outputHandler?.({ terminalID: 'term-1', threadID, sequence, data: b64(text) });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

describe('<TakeControlTerminal>', () => {
  let restoreRaf: (() => void) | undefined;

  beforeEach(() => {
    mocks.writes.length = 0;
    mocks.resizes.length = 0;
    outputHandler = null;
    mocks.resetLastTerminal();
    vi.clearAllMocks();
    mocks.ProviderTerminalInput.mockImplementation(async () => {});
    mocks.ProviderTerminalResize.mockImplementation(async () => {});
    mocks.ProviderTerminalSetControl.mockImplementation(async () => {});
    mocks.ProviderTerminalDetach.mockImplementation(async () => {});
    mocks.wailsEventOn.mockImplementation((name: string, handler: (p: unknown) => void) => {
      if (name === 'provider:terminal_output') outputHandler = handler;
      return () => {};
    });
    mocks.ProviderTerminalAttach.mockResolvedValue({
      terminalID: 'term-1',
      threadID: 'thread-1',
      summary: { cols: 80, rows: 24 },
    });
    // Run rAF synchronously so scheduleFit() resolves without a real frame.
    const frame = vi.spyOn(window, 'requestAnimationFrame')
      .mockImplementation((cb: FrameRequestCallback) => {
        cb(0);
        return 0;
      });
    restoreRaf = () => frame.mockRestore();
  });

  afterEach(() => {
    cleanup();
    restoreRaf?.();
    restoreRaf = undefined;
  });

  it('dedupes the replay/live overlap by the watermark and drains only newer chunks', async () => {
    // The backend ring keeps buffering across attach, so chunks emitted in the
    // attach→replay window appear in BOTH the live stream and the replay
    // snapshot. The component must write the replay frame once and drain only
    // the buffered chunks above the watermark — never double-render the overlap.
    const replay = deferred<{ data: string; fromSequence: number; throughSequence: number }>();
    mocks.ProviderTerminalReplay.mockReturnValue(replay.promise);

    render(TakeControlTerminal, { props: { paneId: 'tc-1', threadId: 'thread-1' } });

    // Wait until hydrate() has built the terminal and is awaiting the replay.
    await waitFor(() => {
      expect(mocks.getLastTerminal()).not.toBeNull();
      expect(mocks.ProviderTerminalReplay).toHaveBeenCalled();
    });

    // Emit live chunks BEFORE the replay resolves — these buffer. seq 1 & 2 are
    // also covered by the replay watermark (2); seq 3 is genuinely newer.
    emit(1, 'a');
    emit(2, 'b');
    emit(3, 'c');
    // A chunk for a different thread must be ignored entirely.
    emit(99, 'OTHER', 'thread-2');

    replay.resolve({ data: b64('REPLAY'), fromSequence: 1, throughSequence: 2 });

    await waitFor(() => expect(mocks.writes).toContain('c'));
    // Post-hydrate live chunk writes straight through.
    emit(4, 'd');
    await waitFor(() => expect(mocks.writes).toContain('d'));

    // Replay frame, then only the above-watermark buffered chunk, then the live
    // one. No 'a'/'b' (deduped), no duplicates, no cross-thread 'OTHER'.
    expect(mocks.writes).toEqual(['REPLAY', 'c', 'd']);
  });

  it('sizes the grid to the backend summary before writing the replay frame', async () => {
    mocks.ProviderTerminalReplay.mockResolvedValue({
      data: b64('FRAME'),
      fromSequence: 0,
      throughSequence: 0,
    });

    render(TakeControlTerminal, { props: { paneId: 'tc-1', threadId: 'thread-1' } });

    await waitFor(() => expect(mocks.writes).toContain('FRAME'));
    // The pre-replay resize uses the summary's (cols, rows).
    expect(mocks.resizes[0]).toEqual([80, 24]);
  });

  it('gates input on the take-control lease', async () => {
    mocks.ProviderTerminalReplay.mockResolvedValue({
      data: '',
      fromSequence: 0,
      throughSequence: 0,
    });

    const { getByRole } = render(TakeControlTerminal, {
      props: { paneId: 'tc-1', threadId: 'thread-1' },
    });

    await waitFor(() => expect(mocks.getLastTerminal()?.dataHandler).toBeTruthy());
    const term = mocks.getLastTerminal()!;

    // Read-only by default: a keystroke is swallowed, never sent to the PTY.
    term.dataHandler!('x');
    expect(mocks.ProviderTerminalInput).not.toHaveBeenCalled();

    // Acquire control, then the same keystroke is delivered.
    await fireEvent.click(getByRole('button', { name: 'Take control' }));
    await waitFor(() =>
      expect(mocks.ProviderTerminalSetControl).toHaveBeenCalledWith('thread-1', true),
    );
    await waitFor(() => expect(getByRole('button', { name: 'Release control' })).toBeTruthy());

    term.dataHandler!('y');
    expect(mocks.ProviderTerminalInput).toHaveBeenCalledWith(
      'thread-1',
      encodeTerminalInput('y'),
    );
  });

  it('gates a right-click paste on the same lease as typed input', async () => {
    mocks.ProviderTerminalReplay.mockResolvedValue({
      data: '',
      fromSequence: 0,
      throughSequence: 0,
    });
    const readText = vi.fn(async () => 'pasted');
    Object.defineProperty(globalThis.navigator, 'clipboard', {
      value: { readText },
      configurable: true,
      writable: true,
    });

    const { getByRole, container } = render(TakeControlTerminal, {
      props: { paneId: 'tc-rc', threadId: 'thread-1' },
    });
    await waitFor(() => expect(mocks.getLastTerminal()?.dataHandler).toBeTruthy());
    const term = mocks.getLastTerminal()!;
    const mount = container.querySelector<HTMLElement>('[data-testid="take-control-mount"]');
    expect(mount).toBeTruthy();

    // Read-only: the paste lands in xterm (so the user sees the same thing a
    // keystroke does — nothing) but never reaches the PTY.
    mount!.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true }));
    await waitFor(() => expect(term.pastes).toEqual(['pasted']));
    expect(mocks.ProviderTerminalInput).not.toHaveBeenCalled();

    await fireEvent.click(getByRole('button', { name: 'Take control' }));
    await waitFor(() => expect(getByRole('button', { name: 'Release control' })).toBeTruthy());

    mount!.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true }));
    await waitFor(() =>
      expect(mocks.ProviderTerminalInput).toHaveBeenCalledWith(
        'thread-1',
        encodeTerminalInput('pasted'),
      ),
    );
  });

  it('waits for the in-flight input write before sending later keystrokes', async () => {
    mocks.ProviderTerminalReplay.mockResolvedValue({
      data: '',
      fromSequence: 0,
      throughSequence: 0,
    });
    const firstWrite = deferred<void>();
    mocks.ProviderTerminalInput
      .mockReturnValueOnce(firstWrite.promise)
      .mockResolvedValue(undefined);

    const { getByRole } = render(TakeControlTerminal, {
      props: { paneId: 'tc-1', threadId: 'thread-1' },
    });
    await waitFor(() => expect(mocks.getLastTerminal()?.dataHandler).toBeTruthy());
    await fireEvent.click(getByRole('button', { name: 'Take control' }));
    await waitFor(() => expect(getByRole('button', { name: 'Release control' })).toBeTruthy());

    const handler = mocks.getLastTerminal()!.dataHandler!;
    handler('a');
    handler('b');

    expect(mocks.ProviderTerminalInput).toHaveBeenCalledTimes(1);
    expect(mocks.ProviderTerminalInput).toHaveBeenCalledWith(
      'thread-1',
      encodeTerminalInput('a'),
    );

    firstWrite.resolve();
    await firstWrite.promise;
    await Promise.resolve();

    expect(mocks.ProviderTerminalInput).toHaveBeenCalledTimes(2);
    expect(mocks.ProviderTerminalInput).toHaveBeenLastCalledWith(
      'thread-1',
      encodeTerminalInput('b'),
    );
  });

  it('releases the lease and re-gates input when control is dropped', async () => {
    mocks.ProviderTerminalReplay.mockResolvedValue({
      data: '',
      fromSequence: 0,
      throughSequence: 0,
    });

    const { getByRole } = render(TakeControlTerminal, {
      props: { paneId: 'tc-1', threadId: 'thread-1' },
    });

    await waitFor(() => expect(mocks.getLastTerminal()?.dataHandler).toBeTruthy());
    const term = mocks.getLastTerminal()!;

    // Acquire, then release control.
    await fireEvent.click(getByRole('button', { name: 'Take control' }));
    await waitFor(() => expect(getByRole('button', { name: 'Release control' })).toBeTruthy());
    await fireEvent.click(getByRole('button', { name: 'Release control' }));
    await waitFor(() =>
      expect(mocks.ProviderTerminalSetControl).toHaveBeenLastCalledWith('thread-1', false),
    );
    await waitFor(() => expect(getByRole('button', { name: 'Take control' })).toBeTruthy());

    // Back to read-only: a keystroke is swallowed again, never sent to the PTY.
    mocks.ProviderTerminalInput.mockClear();
    term.dataHandler!('z');
    expect(mocks.ProviderTerminalInput).not.toHaveBeenCalled();
  });

  it('drains accepted input before releasing control and drops input typed during release', async () => {
    mocks.ProviderTerminalReplay.mockResolvedValue({
      data: '',
      fromSequence: 0,
      throughSequence: 0,
    });
    const activeWrite = deferred<void>();
    mocks.ProviderTerminalInput.mockReturnValueOnce(activeWrite.promise);

    const { getByRole } = render(TakeControlTerminal, {
      props: { paneId: 'tc-1', threadId: 'thread-1' },
    });
    await waitFor(() => expect(mocks.getLastTerminal()?.dataHandler).toBeTruthy());
    await fireEvent.click(getByRole('button', { name: 'Take control' }));
    await waitFor(() => expect(getByRole('button', { name: 'Release control' })).toBeTruthy());

    const handler = mocks.getLastTerminal()!.dataHandler!;
    handler('a');
    handler('b');
    const releaseClick = fireEvent.click(getByRole('button', { name: 'Release control' }));

    await waitFor(() => expect(getByRole('button', { name: 'Take control' })).toBeDisabled());
    handler('c');
    expect(mocks.ProviderTerminalSetControl).not.toHaveBeenLastCalledWith('thread-1', false);
    expect(mocks.ProviderTerminalInput).toHaveBeenCalledTimes(1);

    // A second click during the transition cannot start a competing acquire.
    await fireEvent.click(getByRole('button', { name: 'Take control' }));
    expect(mocks.ProviderTerminalSetControl).toHaveBeenCalledTimes(1);

    activeWrite.resolve();
    await activeWrite.promise;
    await releaseClick;
    await waitFor(() =>
      expect(mocks.ProviderTerminalSetControl).toHaveBeenLastCalledWith('thread-1', false),
    );
    expect(mocks.ProviderTerminalInput).toHaveBeenCalledTimes(2);
    expect(mocks.ProviderTerminalInput).toHaveBeenLastCalledWith(
      'thread-1',
      encodeTerminalInput('b'),
    );
  });

  it('surfaces a toggle failure and leaves the lease unchanged', async () => {
    mocks.ProviderTerminalReplay.mockResolvedValue({
      data: '',
      fromSequence: 0,
      throughSequence: 0,
    });
    const failure = new Error('lease busy');
    mocks.ProviderTerminalSetControl.mockRejectedValue(failure);

    const { getByRole } = render(TakeControlTerminal, {
      props: { paneId: 'tc-1', threadId: 'thread-1' },
    });

    await waitFor(() => expect(mocks.getLastTerminal()?.dataHandler).toBeTruthy());
    const term = mocks.getLastTerminal()!;

    await fireEvent.click(getByRole('button', { name: 'Take control' }));
    await waitFor(() =>
      expect(mocks.addErrorToast).toHaveBeenCalledWith(
        expect.stringContaining('Take control failed'),
        failure,
      ),
    );

    // The lease never latched: still read-only, and input stays swallowed.
    expect(getByRole('button', { name: 'Take control' })).toBeTruthy();
    term.dataHandler!('y');
    expect(mocks.ProviderTerminalInput).not.toHaveBeenCalled();
  });

  it('detaches the provider terminal on destroy', async () => {
    mocks.ProviderTerminalReplay.mockResolvedValue({
      data: '',
      fromSequence: 0,
      throughSequence: 0,
    });

    const { unmount } = render(TakeControlTerminal, {
      props: { paneId: 'tc-1', threadId: 'thread-1' },
    });
    await waitFor(() => expect(mocks.getLastTerminal()).not.toBeNull());

    unmount();
    expect(mocks.ProviderTerminalDetach).toHaveBeenCalledWith('thread-1');
  });

  it('compensates when an acquire completes after the pane is destroyed', async () => {
    mocks.ProviderTerminalReplay.mockResolvedValue({
      data: '',
      fromSequence: 0,
      throughSequence: 0,
    });
    const acquire = deferred<void>();
    mocks.ProviderTerminalSetControl
      .mockReturnValueOnce(acquire.promise)
      .mockResolvedValue(undefined);

    const { getByRole, unmount } = render(TakeControlTerminal, {
      props: { paneId: 'tc-1', threadId: 'thread-1' },
    });
    await waitFor(() => expect(mocks.getLastTerminal()?.dataHandler).toBeTruthy());
    const acquireClick = fireEvent.click(getByRole('button', { name: 'Take control' }));
    await waitFor(() =>
      expect(mocks.ProviderTerminalSetControl).toHaveBeenCalledWith('thread-1', true),
    );

    unmount();
    expect(mocks.ProviderTerminalDetach).toHaveBeenCalledWith('thread-1');
    acquire.resolve();
    await acquireClick;

    await waitFor(() =>
      expect(mocks.ProviderTerminalSetControl).toHaveBeenLastCalledWith('thread-1', false),
    );
  });

  it('surfaces an attach failure instead of mounting a terminal', async () => {
    mocks.ProviderTerminalAttach.mockRejectedValue(new Error('no live session'));

    const { findByText, queryByTestId } = render(TakeControlTerminal, {
      props: { paneId: 'tc-1', threadId: 'thread-1' },
    });

    expect(await findByText(/Could not attach/i)).toBeTruthy();
    expect(mocks.ProviderTerminalReplay).not.toHaveBeenCalled();
    expect(queryByTestId('take-control-find')).toBeNull();
  });

  it('finds in the mirror from its button, under this pane, and a close returns focus to the mirror', async () => {
    resetPaneFindForTest();
    mocks.searches.length = 0;
    mocks.ProviderTerminalReplay.mockResolvedValue({ data: b64('FRAME'), fromSequence: 0, throughSequence: 0 });

    const { findByTestId, getByTestId, queryByTestId, unmount } = render(TakeControlTerminal, {
      props: { paneId: 'tc-find', threadId: 'thread-1' },
    });
    expect(queryByTestId('take-control-find')).toBeNull();
    const button = await findByTestId('take-control-find');
    // The find (and its button) arrive once the replay frame is parsed.
    expect(mocks.searches).toEqual(['parsed']);
    expect(button.getAttribute('title')).toBe('Find in Terminal');
    await fireEvent.click(button);

    const input = (await findByTestId('pane-find-input')) as HTMLInputElement;
    await waitFor(() => expect(document.activeElement).toBe(input));
    // Focus in the bar is focus in the mirror, as in the xterm.
    expect(getTerminalFocused('tc-find')).toBe(true);
    mocks.getLastTerminal()!.textarea!.focus();
    expect(getTerminalFocused('tc-find')).toBe(true);
    input.focus();
    expect(input.placeholder).toBe('Find in terminal');
    await fireEvent.input(input, { target: { value: 'needle' } });
    // Two matches, then the newest active.
    expect(mocks.searches).toEqual(['parsed', 'decorate', 'decorate', 'decorate']);
    await waitFor(() => expect(getByTestId('pane-find-count').textContent?.trim()).toBe('2/2'));
    await fireEvent.keyDown(input, { key: 'Enter' });
    expect(mocks.searches.slice(-2)).toEqual(['undecorate', 'decorate']);
    await waitFor(() => expect(getByTestId('pane-find-count').textContent?.trim()).toBe('1/2'));

    await fireEvent.click(getByTestId('pane-find-close'));
    expect(queryByTestId('pane-find-bar')).toBeNull();
    expect(mocks.searches.slice(-3)).toEqual(['undecorate', 'undecorate', 'undecorate']);
    await waitFor(() => expect(document.activeElement).toBe(mocks.getLastTerminal()!.textarea));
    expect(isPaneFindOpen('tc-find')).toBe(false);

    // A theme change repaints the open query's matches.
    await fireEvent.click(getByTestId('take-control-find'));
    mocks.searches.length = 0;
    try {
      setResolvedThemeForTest('light');
      const repaint = ['undecorate', 'undecorate', 'undecorate', 'decorate', 'decorate', 'decorate'];
      await waitFor(() => expect(mocks.searches).toEqual(repaint));
      setResolvedThemeForTest('dark');
      await waitFor(() => expect(mocks.searches).toEqual([...repaint, ...repaint]));
    } finally {
      setResolvedThemeForTest('dark');
    }
    mocks.searches.length = 0;
    unmount();
    // The unmount clears its matches.
    expect(mocks.searches).toEqual(['undecorate', 'undecorate', 'undecorate']);
    resetPaneFindForTest();
  });
});
