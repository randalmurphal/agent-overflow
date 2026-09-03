import { describe, expect, it, beforeEach } from 'vitest';
import {
  createThreadTerminalState,
  getThreadTerminalState,
  getThreadTerminalStateForTerminalEvent,
  getTerminalFocused,
  notifyTerminalFocus,
  PENDING_OUTPUT_LIMITS,
  resetThreadTerminalStatesForTest,
  resetTerminalFocusForTest,
  TERMINAL_DRAWER_LIMITS,
  trimPendingOutput,
} from './terminalStore.svelte';
import type { TerminalSessionSummary } from '../../types/terminal';

// pendingOutput now carries raw PTY bytes (what xterm.write consumes), so the
// fixtures build Uint8Arrays. enc/dec keep the assertions readable for the
// ASCII payloads; filled() builds large single-byte chunks for the cap tests.
const enc = (text: string): Uint8Array => new TextEncoder().encode(text);
const dec = (bytes: Uint8Array): string => new TextDecoder().decode(bytes);
const filled = (n: number, ch: string): Uint8Array => new Uint8Array(n).fill(ch.charCodeAt(0));

function makeSummary(overrides: Partial<TerminalSessionSummary> = {}): TerminalSessionSummary {
  return {
    terminalID: overrides.terminalID ?? 'term-1',
    threadID: overrides.threadID ?? 'thread-1',
    shell: overrides.shell ?? '/bin/bash',
    cwd: overrides.cwd ?? '/tmp',
    rows: overrides.rows ?? 24,
    cols: overrides.cols ?? 80,
    pid: overrides.pid ?? 1234,
    startedAt: overrides.startedAt ?? 0,
    running: overrides.running ?? true,
    exitCode: overrides.exitCode ?? 0,
    exitReason: overrides.exitReason ?? '',
  };
}

describe('ThreadTerminalState', () => {
  beforeEach(() => {
    resetThreadTerminalStatesForTest();
  });

  it('adds a tab and marks it active', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    expect(s.tabs).toHaveLength(1);
    expect(s.activeTerminalID).toBe('a');
  });

  it('appends and drains output for the matching tab', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.appendOutput('a', enc('hello '));
    s.appendOutput('a', enc('world'));
    expect(s.tabs[0]!.pendingOutput.map(dec)).toEqual(['hello ', 'world']);
    const drained = s.drainOutput('a');
    expect(drained.map(dec)).toEqual(['hello ', 'world']);
    expect(s.tabs[0]!.pendingOutput).toEqual([]);
  });

  it('returns empty list when draining an unknown terminal', () => {
    const s = createThreadTerminalState();
    expect(s.drainOutput('nope')).toEqual([]);
  });

  it('removes a tab and promotes another tab to active', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.addTab(makeSummary({ terminalID: 'b' }));
    expect(s.activeTerminalID).toBe('b');
    s.removeTab('b');
    expect(s.tabs.map((t) => t.terminalID)).toEqual(['a']);
    expect(s.activeTerminalID).toBe('a');
  });

  it('clears the active terminal when the last tab closes', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'only' }));
    s.removeTab('only');
    expect(s.tabs).toHaveLength(0);
    expect(s.activeTerminalID).toBeNull();
  });

  it('ignores setActive for unknown terminals', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.setActive('missing');
    expect(s.activeTerminalID).toBe('a');
  });

  it('clamps the drawer height between min and max', () => {
    const s = createThreadTerminalState();
    s.setDrawerHeight(-100);
    expect(s.drawerHeight).toBe(TERMINAL_DRAWER_LIMITS.min);
    s.setDrawerHeight(9999);
    expect(s.drawerHeight).toBe(TERMINAL_DRAWER_LIMITS.max);
  });

  it('updates the summary for a tab while preserving pending output', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a', rows: 24, cols: 80 }));
    s.appendOutput('a', enc('xx'));
    s.updateSummary(makeSummary({ terminalID: 'a', rows: 40, cols: 120 }));
    expect(s.tabs[0]!.summary.rows).toBe(40);
    expect(s.tabs[0]!.pendingOutput.map(dec)).toEqual(['xx']);
  });

  it('routes terminal events to an existing tab when the event thread bucket moved first', () => {
    const draftHandle = getThreadTerminalState('draft:thread');
    draftHandle.addTab(makeSummary({ terminalID: 'a', threadID: 'draft:thread' }));

    const routed = getThreadTerminalStateForTerminalEvent('thread-real', 'a');
    routed.appendOutput('a', enc('late-output'));

    expect(routed).toBe(draftHandle);
    expect(draftHandle.tabs[0]!.pendingOutput.map(dec)).toEqual(['late-output']);
  });

  it('clear wipes tabs and active state', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.clear();
    expect(s.tabs).toEqual([]);
    expect(s.activeTerminalID).toBeNull();
  });

  describe('attachXterm / clearActive', () => {
    const xterm = () => {
      const calls = { clear: 0 };
      return { calls, actions: { clear: () => { calls.clear += 1; } } };
    };

    it('clears the ACTIVE tab’s xterm only, and reports false with none mounted', () => {
      const s = createThreadTerminalState();
      s.addTab(makeSummary({ terminalID: 'a' }));
      s.addTab(makeSummary({ terminalID: 'b' })); // b is active
      expect(s.clearActive()).toBe(false);
      const a = xterm();
      const b = xterm();
      s.attachXterm('a', a.actions);
      s.attachXterm('b', b.actions);
      expect(s.clearActive()).toBe(true);
      expect(b.calls.clear).toBe(1);
      expect(a.calls.clear).toBe(0);
      s.setActive('a');
      expect(s.clearActive()).toBe(true);
      expect(a.calls.clear).toBe(1);
    });

    it('detach drops only its own registration (a remount may already have re-attached)', () => {
      const s = createThreadTerminalState();
      s.addTab(makeSummary({ terminalID: 'a' }));
      const first = xterm();
      const second = xterm();
      const detachFirst = s.attachXterm('a', first.actions);
      // Remount attaches its new xterm BEFORE the old surface's teardown runs.
      const detachSecond = s.attachXterm('a', second.actions);
      detachFirst();
      expect(s.clearActive()).toBe(true);
      expect(second.calls.clear).toBe(1);
      expect(first.calls.clear).toBe(0);
      detachSecond();
      expect(s.clearActive()).toBe(false);
    });

    it('removeTab and clear forget the mounted xterm', () => {
      const s = createThreadTerminalState();
      s.addTab(makeSummary({ terminalID: 'a' }));
      s.addTab(makeSummary({ terminalID: 'b' }));
      const a = xterm();
      const b = xterm();
      s.attachXterm('a', a.actions);
      s.attachXterm('b', b.actions);
      s.removeTab('b'); // promotes a
      expect(s.clearActive()).toBe(true);
      expect(a.calls.clear).toBe(1);
      s.clear();
      s.addTab(makeSummary({ terminalID: 'a' }));
      expect(s.clearActive()).toBe(false);
    });
  });

  it('only drops pending output proven to be covered by replay', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'a' }));
    s.appendOutput('a', enc('outside-replay'), 8);
    s.appendOutput('a', enc('first-covered-maybe-partial'), 9);
    s.appendOutput('a', enc('covered'), 10);
    s.appendOutput('a', enc('future'), 11);

    s.markReplayed('a', 9, 10);

    expect(s.tabs[0]!.pendingOutput.map(dec)).toEqual([
      'outside-replay',
      'first-covered-maybe-partial',
      'future',
    ]);
  });

  it('keeps per-chunk sequences aligned with output after eviction', () => {
    // Eviction shifts the oldest chunk off pendingOutput AND its parallel
    // sequence entry (trimPendingOutputEntries does both in lockstep). If
    // those arrays drift, markReplayed walks sequences[i] against the wrong
    // pendingOutput[i] and drops the wrong chunk. Force an eviction between
    // distinctly-sequenced chunks, then replay-cover one survivor and assert
    // the *correct* chunk remains.
    const cap = PENDING_OUTPUT_LIMITS.bytes;
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'T' }));
    s.appendOutput('T', filled(cap / 2, 'A'), 1);
    s.appendOutput('T', filled(cap / 2, 'B'), 2); // queue=[A(1), B(2)] at exactly the cap
    s.appendOutput('T', filled(cap / 2, 'C'), 3); // overflows: evicts A → queue=[B(2), C(3)]

    // Sequences must now be [2, 3]. Replay covering (1, 2] drops B(2) only.
    s.markReplayed('T', 1, 2);

    const queue = s.tabs[0]!.pendingOutput;
    expect(queue).toHaveLength(1);
    // Had the sequence array drifted to [1, 2], C(3) would have been dropped
    // instead of B(2) — assert the survivor is C.
    expect(queue[0]![0]).toBe('C'.charCodeAt(0));
  });
});

// --- Bug D5 regression: terminalFocus registry (pane-scoped) ---
describe('terminal focus registry', () => {
  const PANE = 'pane-a';

  beforeEach(() => {
    resetTerminalFocusForTest();
  });

  it('starts unfocused', () => {
    expect(getTerminalFocused(PANE)).toBe(false);
  });

  it('becomes focused on a single notifyTerminalFocus(pane, true)', () => {
    notifyTerminalFocus(PANE, true);
    expect(getTerminalFocused(PANE)).toBe(true);
  });

  it('flips back to false when paired false notification arrives', () => {
    notifyTerminalFocus(PANE, true);
    notifyTerminalFocus(PANE, false);
    expect(getTerminalFocused(PANE)).toBe(false);
  });

  it('stays focused while at least one component holds focus (e.g. remount overlap)', () => {
    notifyTerminalFocus(PANE, true);
    notifyTerminalFocus(PANE, true);
    notifyTerminalFocus(PANE, false);
    // One component is still focused.
    expect(getTerminalFocused(PANE)).toBe(true);
    notifyTerminalFocus(PANE, false);
    expect(getTerminalFocused(PANE)).toBe(false);
  });

  it('never dips below zero (extra unfocus calls are tolerated)', () => {
    notifyTerminalFocus(PANE, false);
    notifyTerminalFocus(PANE, false);
    expect(getTerminalFocused(PANE)).toBe(false);
    notifyTerminalFocus(PANE, true);
    expect(getTerminalFocused(PANE)).toBe(true);
  });

  it('rapid toggling ends up in the expected state', () => {
    for (let i = 0; i < 20; i += 1) {
      notifyTerminalFocus(PANE, true);
      notifyTerminalFocus(PANE, false);
    }
    expect(getTerminalFocused(PANE)).toBe(false);
  });

  // The reason the registry is keyed by pane: focusing one terminal pane must
  // not report focus for another, or it would suppress the other pane's
  // `!terminalFocus` chords. This fails under the old module-global counter.
  it('isolates focus per pane — focusing one pane does not report focus for another', () => {
    notifyTerminalFocus('pane-a', true);
    expect(getTerminalFocused('pane-a')).toBe(true);
    expect(getTerminalFocused('pane-b')).toBe(false);

    // Focusing the second pane is independent; blurring the first leaves the
    // second focused.
    notifyTerminalFocus('pane-b', true);
    notifyTerminalFocus('pane-a', false);
    expect(getTerminalFocused('pane-a')).toBe(false);
    expect(getTerminalFocused('pane-b')).toBe(true);
  });
});

// --- Bug D7 regression: pendingOutput cap ---
describe('pendingOutput cap', () => {
  const cap = PENDING_OUTPUT_LIMITS.bytes;

  function bytesIn(queue: Uint8Array[]): number {
    return queue.reduce((acc, s) => acc + s.length, 0);
  }

  it('appends as-is while total stays below the cap', () => {
    const q = trimPendingOutput([filled(100, 'a')], filled(200, 'b'));
    expect(q.map(dec)).toEqual(['a'.repeat(100), 'b'.repeat(200)]);
    expect(bytesIn(q)).toBe(300);
  });

  it('drops oldest chunks when total would exceed the cap', () => {
    // Build a queue that is already full.
    const queue = [filled(cap / 2, 'x'), filled(cap / 2, 'y')];
    // Add a chunk that would overflow by 100 bytes — oldest 'x' is dropped.
    const next = trimPendingOutput(queue, filled(100, 'z'));
    expect(bytesIn(next)).toBeLessThanOrEqual(cap);
    expect(next[0]![0]).toBe('y'.charCodeAt(0));
    expect(next[next.length - 1]![0]).toBe('z'.charCodeAt(0));
  });

  it('truncates a single oversized chunk to the cap and discards the rest', () => {
    const queue = [enc('pre')];
    const jumbo = filled(cap + 123, 'J');
    const next = trimPendingOutput(queue, jumbo);
    expect(next).toHaveLength(1);
    expect(next[0]!.length).toBe(cap);
    // Kept the tail of the jumbo (most recent bytes).
    expect(next[0]![0]).toBe('J'.charCodeAt(0));
  });

  it('returns existing when chunk is empty (no work done)', () => {
    const queue = [enc('a'), enc('b')];
    expect(trimPendingOutput(queue, new Uint8Array(0))).toBe(queue);
  });

  it('stress: 2 MB of appended output during unmount stays under the cap', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'T' }));
    // Simulate 2 MB arriving in 1000 chunks of 2000 bytes each.
    const chunk = filled(2_000, 'x');
    for (let i = 0; i < 1_000; i += 1) {
      s.appendOutput('T', chunk);
    }
    const total = s.tabs[0]!.pendingOutput.reduce((acc, v) => acc + v.length, 0);
    expect(total).toBeLessThanOrEqual(cap);
    // Oldest chunks (first ~50%) were evicted; a full 2000-byte chunk survives.
    expect(s.tabs[0]!.pendingOutput[0]).toBe(chunk);
  });

  it('eviction preserves the most recent chunk as the tail', () => {
    const s = createThreadTerminalState();
    s.addTab(makeSummary({ terminalID: 'T' }));
    // 3 MB of a byte, then a single distinguishing tail chunk.
    for (let i = 0; i < 3; i += 1) {
      s.appendOutput('T', filled(cap, 'a'));
    }
    s.appendOutput('T', enc('LAST'));
    const queue = s.tabs[0]!.pendingOutput;
    expect(dec(queue[queue.length - 1]!)).toBe('LAST');
  });
});
