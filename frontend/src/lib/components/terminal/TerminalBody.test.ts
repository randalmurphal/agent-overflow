import { cleanup, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi, type MockInstance } from 'vitest';

import TerminalBody from './TerminalBody.svelte';
import { createThreadTerminalState } from './terminalStore.svelte';
import type { TerminalSessionSummary } from '../../types/terminal';
import { encodeTerminalInput } from '../../types/terminal';
import { eventEscapesTerminalToCommand } from '../../stores/keybindings.svelte';

// xterm can't render under happy-dom (no real canvas/WebGL context), and these
// tests need to observe the exact ORDER of write() calls — so swap in a fake
// Terminal that records every write as a decoded string. The addons are stubbed
// to no-ops; only the write sequence matters here.
const mocks = vi.hoisted(() => {
  const writes: string[] = [];
  // Decoded payloads added here make write() throw, simulating an xterm write
  // failure mid-drain (drives the gate-stays-open regression test).
  const throwOn = new Set<string>();
  const decoder = new TextDecoder();
  let lastTerminal: FakeTerminal | null = null;
  class FakeTerminal {
    options: Record<string, unknown>;
    // Captured so a test can invoke the registered handler directly.
    keyEventHandler: ((event: KeyboardEvent) => boolean) | null = null;
    constructor(options: Record<string, unknown> = {}) {
      this.options = { ...options };
      lastTerminal = this;
    }
    loadAddon(): void {}
    open(): void {}
    attachCustomKeyEventHandler(handler: (event: KeyboardEvent) => boolean): void {
      this.keyEventHandler = handler;
    }
    write(data: Uint8Array | string): void {
      const text = typeof data === 'string' ? data : decoder.decode(data);
      if (throwOn.has(text)) throw new Error(`xterm write failed: ${text}`);
      writes.push(text);
    }
    onData(): { dispose(): void } {
      return { dispose(): void {} };
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
  return {
    writes,
    throwOn,
    FakeTerminal,
    getLastTerminal: (): FakeTerminal | null => lastTerminal,
    GetTerminalReplay: vi.fn(),
    WriteTerminal: vi.fn(async () => {}),
    ResizeTerminal: vi.fn(async () => {}),
  };
});

// The key handler delegates the escape decision to this predicate; its own
// logic (which chords escape) is unit-tested in keybindings.svelte.test.ts.
// Here we mock it to verify the handler's wiring in isolation.
vi.mock('../../stores/keybindings.svelte', () => ({
  eventEscapesTerminalToCommand: vi.fn(() => false),
}));

vi.mock('@xterm/xterm', () => ({ Terminal: mocks.FakeTerminal }));
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { fit(): void {} } }));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@xterm/addon-webgl', () => ({
  WebglAddon: class {
    onContextLoss(): void {}
    dispose(): void {}
  },
}));
vi.mock('../../stores/bindings', () => ({
  GetTerminalReplay: mocks.GetTerminalReplay,
  WriteTerminal: mocks.WriteTerminal,
  ResizeTerminal: mocks.ResizeTerminal,
}));

function makeSummary(terminalID: string): TerminalSessionSummary {
  return {
    terminalID,
    threadID: 'thread-1',
    shell: '/bin/bash',
    cwd: '/home/user',
    rows: 24,
    cols: 80,
    pid: 4242,
    startedAt: 0,
    running: true,
    exitCode: 0,
    exitReason: '',
  };
}

const enc = (text: string): Uint8Array => new TextEncoder().encode(text);

interface ReplayResult {
  data: string;
  fromSequence: number;
  throughSequence: number;
}

// Mount TerminalBody with GetTerminalReplay parked on an unresolved promise so
// the test controls exactly when the replay round-trip completes — reproducing
// the window where a live `terminal:output` event arrives mid-hydrate.
async function mountWithPendingReplay(terminalID: string) {
  const handle = createThreadTerminalState();
  handle.addTab(makeSummary(terminalID));

  let resolveReplay!: (replay: ReplayResult) => void;
  let rejectReplay!: (reason: unknown) => void;
  mocks.GetTerminalReplay.mockReturnValueOnce(
    new Promise<ReplayResult>((resolve, reject) => {
      resolveReplay = resolve;
      rejectReplay = reject;
    }),
  );

  render(TerminalBody, { props: { handle, terminalID, paneId: 'pane-test' } });
  // Let onMount → hydrate() run up to the awaited GetTerminalReplay.
  await tick();
  await Promise.resolve();

  return { handle, resolveReplay, rejectReplay };
}

// hydrate() awaits a replay round-trip before draining buffered output. If the
// drain $effect is allowed to fire on output that lands during that await, two
// corruptions follow: (1) live output is written BEFORE the replay buffer, so
// the replay's cursor/clear control sequences re-apply on top of live state;
// (2) a chunk already covered by the replay snapshot (its live event in flight
// when GetTerminalReplay was captured) gets written by the effect before
// markReplayed can dedupe it — rendering those bytes twice. The hydrate gate
// keeps buffered output flowing through hydrate's own post-markReplayed drain.
describe('TerminalBody hydrate ordering', () => {
  // Set by the failure-path tests to silence + assert the error log; restored
  // in afterEach so a thrown assertion can't leak the spy into the next test.
  let consoleErrorSpy: MockInstance | null = null;

  beforeEach(() => {
    mocks.writes.length = 0;
    mocks.throwOn.clear();
    mocks.GetTerminalReplay.mockReset();
    mocks.WriteTerminal.mockClear();
    mocks.ResizeTerminal.mockClear();
  });

  afterEach(() => {
    consoleErrorSpy?.mockRestore();
    consoleErrorSpy = null;
    cleanup();
  });

  it('writes the replay buffer before live output buffered during hydrate', async () => {
    const { handle, resolveReplay } = await mountWithPendingReplay('t1');

    // A live chunk emitted AFTER the replay snapshot (sequence 5 > the snapshot
    // watermark 3) arrives while the replay round-trip is still in flight — it
    // is genuinely new output, not covered by the replay.
    handle.appendOutput('t1', enc('LIVE'), 5);
    await tick();

    // Replay resolves only now — after the live chunk was already queued. It
    // covers (1, 3]; the live chunk (seq 5) is outside that range, so it must
    // drain on top of the replay buffer, never before it.
    resolveReplay({ data: btoa('REPLAY'), fromSequence: 1, throughSequence: 3 });
    await tick();
    await Promise.resolve();
    await tick();

    expect(mocks.writes).toEqual(['REPLAY', 'LIVE']);
  });

  it('drops replay-covered output queued during hydrate instead of double-writing it', async () => {
    const { handle, resolveReplay } = await mountWithPendingReplay('t2');

    // This chunk's sequence (2) falls inside the replay's proven-covered range
    // (1, 3] — its live `terminal:output` event was in flight when
    // GetTerminalReplay captured the buffer, so the same bytes are already in
    // the replay snapshot. markReplayed must drop it; it must never reach xterm
    // on its own, or the bytes render twice (once standalone, once in replay).
    handle.appendOutput('t2', enc('DUP'), 2);
    await tick();

    resolveReplay({ data: btoa('REPLAY'), fromSequence: 1, throughSequence: 3 });
    await tick();
    await Promise.resolve();
    await tick();

    // Only the replay write survives — the covered chunk (1 < seq 2 ≤ 3) was
    // deduped by markReplayed, not drained to xterm. Without the hydrate gate
    // the racing drain effect would have written 'DUP' before markReplayed
    // could drop it (the drain runs after markReplayed; the effect did not).
    expect(mocks.writes).toEqual(['REPLAY']);
  });

  it('drains output buffered during hydrate even when the replay round-trip fails', async () => {
    consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { handle, rejectReplay } = await mountWithPendingReplay('t3');

    // A live chunk lands while the replay round-trip is still in flight.
    handle.appendOutput('t3', enc('LIVE'), 5);
    await tick();

    // GetTerminalReplay rejects. hydrate()'s catch logs and falls through to the
    // drain, which opens the gate — the buffered chunk must still reach xterm. A
    // failed replay that left the gate shut would strand this output forever:
    // hydrate() runs un-awaited, so the rejection would be silent.
    rejectReplay(new Error('replay unavailable'));
    await tick();
    await Promise.resolve();
    await tick();

    expect(mocks.writes).toEqual(['LIVE']);
    // Surfaced, not swallowed.
    expect(consoleErrorSpy).toHaveBeenCalled();
  });

  it('opens the drain gate even if a buffered write throws, so later output still renders', async () => {
    consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { handle, resolveReplay } = await mountWithPendingReplay('t4');

    // The first buffered chunk throws when written. Seq 5 > throughSequence 3, so
    // markReplayed keeps it and it actually reaches xterm.write() in the drain.
    mocks.throwOn.add('BAD');
    handle.appendOutput('t4', enc('BAD'), 5);
    await tick();

    resolveReplay({ data: btoa('REPLAY'), fromSequence: 1, throughSequence: 3 });
    await tick();
    await Promise.resolve();
    await tick();

    // REPLAY wrote; BAD threw and was logged+skipped, not aborting the drain.
    expect(mocks.writes).toEqual(['REPLAY']);
    expect(consoleErrorSpy).toHaveBeenCalled();

    // The throw did NOT leave the gate shut: a later live chunk drains through the
    // now-open $effect. Without the per-write guard the throw would reject
    // hydrate() before `hydrated = true`, stranding this chunk (and all future
    // output) forever.
    handle.appendOutput('t4', enc('AFTER'), 6);
    await tick();
    await Promise.resolve();

    expect(mocks.writes).toEqual(['REPLAY', 'AFTER']);
  });
});

// The xterm custom key handler decides per-keydown whether a chord should
// bubble to the app (pane navigation) or be written to the PTY. The escape
// decision itself lives in eventEscapesTerminalToCommand (mocked here, unit-
// tested in keybindings.svelte.test.ts); this asserts the handler's wiring.
describe('TerminalBody pane-nav key handler', () => {
  beforeEach(() => {
    mocks.GetTerminalReplay.mockReset();
    vi.mocked(eventEscapesTerminalToCommand).mockReset();
    vi.mocked(eventEscapesTerminalToCommand).mockReturnValue(false);
  });

  afterEach(() => cleanup());

  it('bubbles an escaping chord (false) and keeps every other key for the PTY (true)', async () => {
    const { resolveReplay } = await mountWithPendingReplay('t-keys');
    resolveReplay({ data: '', fromSequence: 0, throughSequence: 0 });
    await tick();

    const handler = mocks.getLastTerminal()?.keyEventHandler;
    expect(handler).toBeTypeOf('function');
    const keydown = (key: string): KeyboardEvent => ({ type: 'keydown', key }) as KeyboardEvent;

    // Non-keydown events (keyup/keypress) are always left to xterm → the PTY,
    // without even consulting the predicate.
    expect(handler!({ type: 'keyup', key: 'h' } as KeyboardEvent)).toBe(true);
    expect(vi.mocked(eventEscapesTerminalToCommand)).not.toHaveBeenCalled();

    // keydown matching an escaping command → false: xterm skips its own
    // handling (no PTY write, no preventDefault) so the event bubbles to the app.
    vi.mocked(eventEscapesTerminalToCommand).mockReturnValue(true);
    expect(handler!(keydown('h'))).toBe(false);

    // keydown for anything else → true: xterm writes it to the PTY as usual.
    vi.mocked(eventEscapesTerminalToCommand).mockReturnValue(false);
    expect(handler!(keydown('x'))).toBe(true);
  });
});

// Shift+Enter is special-cased ahead of the escape predicate: it writes a
// newline (LF) to the PTY and fully consumes the event, instead of letting
// xterm send its default carriage-return submit. LF is what Claude Code /
// Codex read as "newline, don't submit" (= Ctrl+J); at a bare shell it is
// accept-line, identical to the CR it replaces.
describe('TerminalBody Shift+Enter newline', () => {
  beforeEach(() => {
    mocks.GetTerminalReplay.mockReset();
    mocks.WriteTerminal.mockClear();
    vi.mocked(eventEscapesTerminalToCommand).mockReset();
    vi.mocked(eventEscapesTerminalToCommand).mockReturnValue(false);
  });

  afterEach(() => cleanup());

  function enterEvent(over: Partial<KeyboardEvent>): KeyboardEvent {
    return {
      type: 'keydown',
      key: 'Enter',
      shiftKey: false,
      ctrlKey: false,
      altKey: false,
      metaKey: false,
      preventDefault: vi.fn(),
      stopPropagation: vi.fn(),
      ...over,
    } as unknown as KeyboardEvent;
  }

  it('writes a single LF and consumes the event on bare Shift+Enter', async () => {
    const { resolveReplay } = await mountWithPendingReplay('t-se');
    resolveReplay({ data: '', fromSequence: 0, throughSequence: 0 });
    await tick();
    mocks.WriteTerminal.mockClear();

    const handler = mocks.getLastTerminal()?.keyEventHandler;
    expect(handler).toBeTypeOf('function');

    const event = enterEvent({ shiftKey: true });
    // false → xterm must NOT also send its default CR.
    expect(handler!(event)).toBe(false);
    // Consumed: no stray textarea newline (preventDefault), no bubble to the
    // app's window keydown handler (stopPropagation).
    expect(event.preventDefault).toHaveBeenCalledOnce();
    expect(event.stopPropagation).toHaveBeenCalledOnce();
    // Exactly one LF, encoded the same way onData encodes keystrokes.
    expect(mocks.WriteTerminal).toHaveBeenCalledTimes(1);
    expect(mocks.WriteTerminal).toHaveBeenCalledWith('t-se', encodeTerminalInput('\n'));
    // The escape predicate is never consulted — the chord short-circuits it.
    expect(vi.mocked(eventEscapesTerminalToCommand)).not.toHaveBeenCalled();
  });

  it('leaves plain Enter and modifier-combined Enter to the PTY', async () => {
    const { resolveReplay } = await mountWithPendingReplay('t-se2');
    resolveReplay({ data: '', fromSequence: 0, throughSequence: 0 });
    await tick();
    mocks.WriteTerminal.mockClear();

    const handler = mocks.getLastTerminal()?.keyEventHandler;
    expect(handler).toBeTypeOf('function');

    // Plain Enter → xterm owns it (default CR submit); no custom write.
    expect(handler!(enterEvent({}))).toBe(true);
    // Ctrl+Shift+Enter is NOT the bare chord — it falls through so
    // mod+shift+enter (sidebar.cursor.openInNewPane) isn't stolen.
    expect(handler!(enterEvent({ shiftKey: true, ctrlKey: true }))).toBe(true);
    // Alt+Shift+Enter and Meta+Shift+Enter likewise fall through.
    expect(handler!(enterEvent({ shiftKey: true, altKey: true }))).toBe(true);
    expect(handler!(enterEvent({ shiftKey: true, metaKey: true }))).toBe(true);

    expect(mocks.WriteTerminal).not.toHaveBeenCalled();
  });
});
