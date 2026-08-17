// Contract tests for the thread-title generation pending registry: the
// optimistic claim around the async ack, the completion event as the only
// ordinary release, the joined-click guard, and the "surface only what a
// user awaited" rule (auto-generation failures stay silent; a clicked run's
// failure lands on the pane showing the thread, or a toast when none is).

import { beforeEach, describe, expect, it } from 'vitest';
import {
  applyThreadTitleGeneration,
  regenerateThreadTitle,
  resetThreadTitleGenerationForTest,
  titleGenerationPending,
} from './threadTitleGeneration.svelte';
import { resetPanesForTest } from './panes.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { getToasts, removeToast } from './toast.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

function clearToasts(): void {
  for (const toast of [...getToasts()]) removeToast(toast.id);
}

describe('threadTitleGeneration', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetPanesForTest();
    resetThreadTitleGenerationForTest();
    clearToasts();
  });

  it('claims pending before the ack and holds it until the completion event', async () => {
    let resolveAck!: () => void;
    setBindingMock(
      'RegenerateThreadTitle',
      () => new Promise<void>((resolve) => (resolveAck = () => resolve())),
    );

    const call = regenerateThreadTitle('t1');
    expect(titleGenerationPending('t1')).toBe(true);

    resolveAck();
    await call;
    // The ack only means the run started; the event is the release.
    expect(titleGenerationPending('t1')).toBe(true);

    applyThreadTitleGeneration({ threadId: 't1', error: '' });
    expect(titleGenerationPending('t1')).toBe(false);
  });

  it('joins an in-flight generation instead of dispatching a second RPC', async () => {
    let calls = 0;
    setBindingMock('RegenerateThreadTitle', async () => {
      calls += 1;
    });

    await regenerateThreadTitle('t1');
    await regenerateThreadTitle('t1');
    expect(calls).toBe(1);

    // The one running generation's completion clears the joined click too.
    applyThreadTitleGeneration({ threadId: 't1', error: '' });
    expect(titleGenerationPending('t1')).toBe(false);
  });

  it('releases pending and surfaces a toast when the ack rejects with no pane mounted', async () => {
    setBindingMock('RegenerateThreadTitle', async () => {
      throw new Error('unknown thread');
    });

    await regenerateThreadTitle('t1');
    expect(titleGenerationPending('t1')).toBe(false);
    expect(getToasts().map((t) => t.message)).toContain(
      'Failed to regenerate title: unknown thread',
    );
  });

  it('keeps an unawaited failed run silent (auto-generation failures)', () => {
    applyThreadTitleGeneration({ threadId: 't1', error: 'provider CLI failed' });
    expect(titleGenerationPending('t1')).toBe(false);
    expect(getToasts()).toEqual([]);
  });

  it('releases orphaned pending flags on the reconnect edge', async () => {
    setBindingMock('RegenerateThreadTitle', async () => undefined);
    await regenerateThreadTitle('t1');
    expect(titleGenerationPending('t1')).toBe(true);

    // The run's completion frame is lost while the socket is down; the
    // reconnect edge must release the flag or the affordance spins forever.
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    expect(titleGenerationPending('t1')).toBe(true);
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    expect(titleGenerationPending('t1')).toBe(false);
    // Releasing is not surfacing: nothing failed, nothing to report.
    expect(getToasts()).toEqual([]);
  });

  it('ignores a null or thread-less completion frame', () => {
    applyThreadTitleGeneration(null);
    applyThreadTitleGeneration({ threadId: '', error: 'x' });
    expect(getToasts()).toEqual([]);
  });
});
