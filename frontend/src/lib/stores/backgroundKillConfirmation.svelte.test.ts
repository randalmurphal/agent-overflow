import { afterEach, describe, expect, it } from 'vitest';
import {
  cancelBackgroundKillConfirmationForThread,
  confirmBackgroundKill,
  pendingBackgroundKillConfirmation,
  resetForTest,
  resolveBackgroundKillConfirmation,
} from './backgroundKillConfirmation.svelte';

const agent = { launchItemId: 'tu-a', description: 'gate watcher', runState: 'parked' as const, transcriptRootId: 'tu-a' };

describe('backgroundKillConfirmation', () => {
  afterEach(() => resetForTest());

  it('holds one question and answers it', async () => {
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    const answer = confirmBackgroundKill('t1', [agent]);
    expect(pendingBackgroundKillConfirmation()).toMatchObject({ threadId: 't1', agents: [agent] });
    resolveBackgroundKillConfirmation(true);
    expect(pendingBackgroundKillConfirmation()).toBeNull();
    await expect(answer).resolves.toBe(true);
    resolveBackgroundKillConfirmation(true);
  });

  it('settles an older question as "keep them" when a newer one replaces it', async () => {
    const first = confirmBackgroundKill('t1', [agent]);
    const second = confirmBackgroundKill('t2', []);
    await expect(first).resolves.toBe(false);
    expect(pendingBackgroundKillConfirmation()?.threadId).toBe('t2');
    resolveBackgroundKillConfirmation(false);
    await expect(second).resolves.toBe(false);
  });

  it('settles a torn-down thread’s question as "keep them" and leaves another thread’s alone', async () => {
    const ask = confirmBackgroundKill('t1', [agent]);
    cancelBackgroundKillConfirmationForThread('t2');
    expect(pendingBackgroundKillConfirmation()?.threadId).toBe('t1');
    cancelBackgroundKillConfirmationForThread('t1');
    await expect(ask).resolves.toBe(false);
    expect(pendingBackgroundKillConfirmation()).toBeNull();
  });
});
