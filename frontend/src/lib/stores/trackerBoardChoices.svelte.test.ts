// The boards the Issues pane picks from: held while a pane shows them, one
// read per computer that grants tracker:read, pinned to it, started by the
// store on every trigger, the newest read winning, "could not reach" kept
// apart from "answered", and a detached computer's boards gone.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import type { BoardChoice } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import {
  acquireTrackerBoardChoices,
  isBoardChoiceOpenable,
  readTrackerBoardChoices,
  trackerBoardChoiceComputers,
  trackerBoardChoices,
  trackerBoardChoicesAnsweredBy,
  trackerBoardChoicesOf,
  trackerBoardChoicesReadStarted,
} from './trackerBoardChoices.svelte';
import { trackerBoardListsChanged } from './trackerBoardEvents';
import { __setBackendStatusForTest, UNREACHABLE_SETTLE_MS } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { detachBackend, takePinnedBackend } from '../transport/backends';
import { setCarriedSessionScopes } from '../transport/scopes';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { answerAsV0019 } from '../../test/helpers/olderComputer';

function board(id: string, extra: Partial<BoardChoice> = {}): BoardChoice {
  return {
    project: 'p1', connection: 'c1', connection_kind: 'jira', connection_label: 'Work', connection_enabled: true,
    verified_account: 'acc-user1', board: id, name: `Board ${id}`, tracker_project: 'WEB', opens_first: false, ...extra,
  } as BoardChoice;
}

async function settle(): Promise<void> {
  flushSync();
  for (let i = 0; i < 5; i += 1) await Promise.resolve();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

let release: (() => void) | null = null;

function hold(): void {
  release = acquireTrackerBoardChoices();
}

afterEach(() => {
  release?.();
  release = null;
  resetStagedBackends();
});

describe('the boards the pane picks from', () => {
  it('reads nothing until held, then every computer, pinned to it, listed in attach order', async () => {
    stageBackend();
    setCarriedSessionScopes('laptop', ['tracker:read']);
    const pins: string[] = [];
    const read = setBindingMock('TrackerBoardChoices', async () => {
      const pin = String(takePinnedBackend() ?? '');
      pins.push(pin);
      return pin === 'laptop' ? [board('2')] : [board('1')];
    });
    readTrackerBoardChoices();
    await settle();
    expect(read).not.toHaveBeenCalled();
    expect(trackerBoardChoiceComputers().map((c) => c.state.kind)).toEqual(['reading', 'reading']);

    hold();
    await settle();
    expect(pins.sort()).toEqual([HOME_BACKEND, 'laptop'].sort());
    expect(trackerBoardChoices().map((c) => [c.backend, c.choice.board])).toEqual([[HOME_BACKEND, '1'], ['laptop', '2']]);
    expect(trackerBoardChoiceComputers().map((c) => c.state.kind)).toEqual(['answered', 'answered']);

    detachBackend('laptop');
    expect(trackerBoardChoices().map((c) => c.choice.board)).toEqual(['1']);

    // The last release drops what was read.
    release!();
    release = null;
    expect(trackerBoardChoices()).toEqual([]);
  });

  it('keeps the newest read of a computer when an older one answers after it', async () => {
    const answers: Array<(rows: BoardChoice[]) => void> = [];
    setBindingMock('TrackerBoardChoices', () => new Promise<BoardChoice[]>((resolve) => answers.push(resolve)));
    hold();
    await settle();
    readTrackerBoardChoices();
    answers[1]([board('new')]);
    answers[0]([board('old')]);
    await settle();
    expect(trackerBoardChoices().map((c) => c.choice.board)).toEqual(['new']);
    expect(trackerBoardChoicesAnsweredBy(HOME_BACKEND)).toBe(trackerBoardChoicesReadStarted(HOME_BACKEND));
  });

  it('keeps the last answer beside a failure, and tells a computer it never reached from one that answered', async () => {
    const read = setBindingMock('TrackerBoardChoices', async () => {
      throw new DisconnectedError('offline');
    });
    hold();
    await settle();
    expect(trackerBoardChoicesOf(HOME_BACKEND)).toEqual({ kind: 'offline', rows: null });

    read.mockImplementation(async () => [board('1')]);
    readTrackerBoardChoices();
    await settle();
    read.mockImplementation(async () => {
      throw new TransportError('method_error', 'database is locked');
    });
    readTrackerBoardChoices();
    await settle();
    const failed = trackerBoardChoicesOf(HOME_BACKEND);
    expect(failed?.kind).toBe('failed');
    expect(failed?.rows?.map((b) => b.board)).toEqual(['1']);
    expect(failed?.kind === 'failed' && failed.error.report.reason).toMatch(/database is locked/i);

    // Not reached after it answered: the answer stands.
    read.mockImplementation(async () => [board('2')]);
    readTrackerBoardChoices();
    await settle();
    read.mockImplementation(async () => {
      throw new DisconnectedError('offline');
    });
    readTrackerBoardChoices();
    await settle();
    expect(trackerBoardChoicesOf(HOME_BACKEND)).toEqual({ kind: 'answered', rows: [board('2')] });
  });

  it('reads a computer again when it reconnects and when its boards may have changed, only while held', async () => {
    const read = setBindingMock('TrackerBoardChoices', async () => [board('1')]);
    hold();
    await settle();
    expect(read).toHaveBeenCalledTimes(1);
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await settle();
    expect(read).toHaveBeenCalledTimes(2);
    trackerBoardListsChanged(HOME_BACKEND);
    await settle();
    expect(read).toHaveBeenCalledTimes(3);
    trackerBoardListsChanged('elsewhere');
    await settle();
    expect(read).toHaveBeenCalledTimes(3);

    release!();
    release = null;
    trackerBoardListsChanged(HOME_BACKEND);
    __setBackendStatusForTest(HOME_BACKEND, { status: 'disconnected', nextAttemptAt: null });
    __setBackendStatusForTest(HOME_BACKEND, { status: 'connected', nextAttemptAt: null });
    await settle();
    expect(read).toHaveBeenCalledTimes(3);
  });

  it('reads a computer once its grant arrives, and counts one that never connected as offline', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      stageBackend({ status: 'reconnecting' });
      const read = setBindingMock('TrackerBoardChoices', async () => (takePinnedBackend() === 'laptop' ? [board('7')] : []));
      hold();
      flushSync();
      await vi.advanceTimersByTimeAsync(0);
      expect(trackerBoardChoicesOf('laptop')).toEqual({ kind: 'reading', rows: null });
      await vi.advanceTimersByTimeAsync(UNREACHABLE_SETTLE_MS);
      expect(trackerBoardChoicesOf('laptop')).toEqual({ kind: 'offline', rows: null });
      expect(read).toHaveBeenCalledTimes(1);

      setCarriedSessionScopes('laptop', ['tracker:read']);
      flushSync();
      await vi.advanceTimersByTimeAsync(0);
      expect(read).toHaveBeenCalledTimes(2);
      expect(trackerBoardChoices().map((c) => [c.backend, c.choice.board])).toEqual([['laptop', '7']]);
    } finally {
      vi.useRealTimers();
    }
  });

  it('lists no board of a computer on a release without the method', async () => {
    answerAsV0019();
    hold();
    await settle();
    expect(trackerBoardChoices()).toEqual([]);
    expect(trackerBoardChoicesOf(HOME_BACKEND)).toEqual({ kind: 'answered', rows: [] });
  });

  it('opens no board whose connection is turned off', () => {
    expect(isBoardChoiceOpenable(board('1'))).toBe(true);
    expect(isBoardChoiceOpenable(board('1', { connection_enabled: false }))).toBe(false);
  });
});
