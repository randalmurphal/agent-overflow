// Pure arrow-key reducer for the diff panel's turn list. Tests every
// guard path + the core left/right movement math, including the
// "selected turn is outside the checkpoint list" edge case where
// findIndex returns -1.

import { describe, expect, it } from 'vitest';
import { selectTurnForKey, type DiffPanelKeyArgs } from './diffPanelKeyboard';
import type { Checkpoint } from '../../../types/checkpoint';

function makeCheckpoint(turnIndex: number): Checkpoint {
  return {
    id: `cp-${turnIndex}`,
    threadId: 'thread-a',
    turnIndex,
    refName: `refs/ao/thread-a/${turnIndex}`,
    capturedAt: Date.UTC(2026, 0, 1, 0, turnIndex),
    workspacePath: '/tmp/workspace',
  };
}

function baseArgs(overrides: Partial<DiffPanelKeyArgs> = {}): DiffPanelKeyArgs {
  return {
    key: 'ArrowRight',
    metaKey: false,
    ctrlKey: false,
    altKey: false,
    targetTag: 'DIV',
    source: 'turn',
    panelOpen: true,
    checkpoints: [makeCheckpoint(0), makeCheckpoint(1), makeCheckpoint(2)],
    selectedTurnIndex: 0,
    ...overrides,
  };
}

describe('selectTurnForKey', () => {
  it('returns null when the panel is closed', () => {
    expect(selectTurnForKey(baseArgs({ panelOpen: false }))).toBeNull();
  });

  it('returns null when source is worktree', () => {
    expect(selectTurnForKey(baseArgs({ source: 'worktree' }))).toBeNull();
  });

  it('returns null when source is cumulative', () => {
    expect(selectTurnForKey(baseArgs({ source: 'cumulative' }))).toBeNull();
  });

  it.each(['INPUT', 'TEXTAREA', 'SELECT'])(
    'returns null when focus is inside %s so typing arrows stays with the form field',
    (tag) => {
      expect(selectTurnForKey(baseArgs({ targetTag: tag }))).toBeNull();
    },
  );

  it('allows keystrokes landing on non-form elements (DIV, BUTTON, etc)', () => {
    expect(selectTurnForKey(baseArgs({ targetTag: 'BUTTON', key: 'ArrowRight' }))).toBe(1);
  });

  it.each(['meta', 'ctrl', 'alt'])('returns null when %s modifier is pressed', (mod) => {
    const overrides: Partial<DiffPanelKeyArgs> = {};
    if (mod === 'meta') overrides.metaKey = true;
    if (mod === 'ctrl') overrides.ctrlKey = true;
    if (mod === 'alt') overrides.altKey = true;
    expect(selectTurnForKey(baseArgs(overrides))).toBeNull();
  });

  it.each(['ArrowUp', 'ArrowDown', 'Enter', ' ', 'Escape', 'a'])(
    'returns null for unrelated key %s',
    (key) => {
      expect(selectTurnForKey(baseArgs({ key }))).toBeNull();
    },
  );

  it('returns null when the checkpoint list is empty', () => {
    expect(selectTurnForKey(baseArgs({ checkpoints: [] }))).toBeNull();
  });

  it('ArrowRight moves one step forward', () => {
    expect(selectTurnForKey(baseArgs({ key: 'ArrowRight', selectedTurnIndex: 0 }))).toBe(1);
    expect(selectTurnForKey(baseArgs({ key: 'ArrowRight', selectedTurnIndex: 1 }))).toBe(2);
  });

  it('ArrowLeft moves one step back', () => {
    expect(selectTurnForKey(baseArgs({ key: 'ArrowLeft', selectedTurnIndex: 2 }))).toBe(1);
    expect(selectTurnForKey(baseArgs({ key: 'ArrowLeft', selectedTurnIndex: 1 }))).toBe(0);
  });

  it('ArrowRight at the tail returns null (no-op, not wrap)', () => {
    expect(selectTurnForKey(baseArgs({ key: 'ArrowRight', selectedTurnIndex: 2 }))).toBeNull();
  });

  it('ArrowLeft at the head returns null (no-op, not wrap)', () => {
    expect(selectTurnForKey(baseArgs({ key: 'ArrowLeft', selectedTurnIndex: 0 }))).toBeNull();
  });

  it('ArrowRight with selectedTurnIndex=null lands on the first checkpoint', () => {
    // findIndex returns -1 when selection is out of the list; the
    // branch "idx === -1 ? 0" takes over and we pick the first entry.
    expect(
      selectTurnForKey(baseArgs({ key: 'ArrowRight', selectedTurnIndex: null })),
    ).toBe(0);
  });

  it('ArrowLeft with selectedTurnIndex=null clamps to the first checkpoint', () => {
    // idx is -1; the "idx <= 0 ? 0" branch clamps to the start.
    // Since the selectedTurnIndex is already "not 0", we return 0.
    expect(
      selectTurnForKey(baseArgs({ key: 'ArrowLeft', selectedTurnIndex: null })),
    ).toBe(0);
  });

  it('returns null when stepping would land on the already-selected turn', () => {
    // ArrowLeft from a turn that is NOT in the checkpoint list returns
    // turnIndex=0. If selected was also 0, the guard on
    // `target.turnIndex === selectedTurnIndex` returns null.
    expect(
      selectTurnForKey(
        baseArgs({ key: 'ArrowLeft', selectedTurnIndex: 0 }),
      ),
    ).toBeNull();
  });

  it('respects a sparse checkpoint list (indices do not start at 0)', () => {
    const cps = [makeCheckpoint(5), makeCheckpoint(12), makeCheckpoint(21)];
    expect(
      selectTurnForKey(baseArgs({ checkpoints: cps, key: 'ArrowRight', selectedTurnIndex: 5 })),
    ).toBe(12);
    expect(
      selectTurnForKey(baseArgs({ checkpoints: cps, key: 'ArrowRight', selectedTurnIndex: 12 })),
    ).toBe(21);
    expect(
      selectTurnForKey(baseArgs({ checkpoints: cps, key: 'ArrowRight', selectedTurnIndex: 21 })),
    ).toBeNull();
  });
});
