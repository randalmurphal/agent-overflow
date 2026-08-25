import { beforeEach, describe, expect, it, vi } from 'vitest';
import { createRevealGateTripwire } from './revealGateTripwire';

// The tripwire pairs every `reportFrontendDiagnostic` call with a
// console.warn carrying the same detail (a non-loopback session cannot
// persist the record, so the console line is the only surviving
// evidence there). Asserting on the warn keeps this suite off
// `vi.mock` of a shared module — see frontend/AGENTS.md § Testing.
let warned: string[] = [];

function flushMicrotasks(): Promise<void> {
  return Promise.resolve();
}

describe('revealGateTripwire', () => {
  beforeEach(() => {
    warned = [];
    vi.spyOn(console, 'warn').mockImplementation((...args: unknown[]) => {
      warned.push(args.map(String).join(' '));
    });
  });

  function make(engaged = true) {
    return createRevealGateTripwire({
      getThreadId: () => 'thread-1',
      isRevealGateEngaged: () => engaged,
    });
  }

  it('reports a commit that never got a reveal recompute', async () => {
    const tripwire = make();
    tripwire.noteItemsCommitted('commitUpsertResult');
    await flushMicrotasks();
    expect(warned).toHaveLength(1);
    expect(warned[0]).toContain('items committed with no reveal recompute');
    expect(warned[0]).toContain('site=commitUpsertResult');
    expect(warned[0]).toContain('thread=thread-1');
  });

  it('stays silent when the recompute follows in the same task', async () => {
    const tripwire = make();
    tripwire.noteItemsCommitted('commitTimelineItems');
    tripwire.noteRevealSynced();
    await flushMicrotasks();
    expect(warned).toEqual([]);
  });

  it('stays silent while the gate is not engaged', async () => {
    const tripwire = make(false);
    tripwire.noteItemsCommitted('commitTimelineItems');
    await flushMicrotasks();
    expect(warned).toEqual([]);
  });

  // A structurally missing recompute repeats on every batch of a
  // streaming turn; the first few carry the whole signal.
  it('caps its reports per pane', async () => {
    const tripwire = make();
    for (let i = 0; i < 20; i += 1) {
      tripwire.noteItemsCommitted('commitUpsertResult');
      await flushMicrotasks();
    }
    expect(warned).toHaveLength(5);
  });

  // Observation only: the tripwire has no way to move the gate, which is
  // what keeps it clear of the reveal queue's readable drain.
  it('exposes nothing that can recompute the gate', () => {
    expect(Object.keys(make()).sort()).toEqual([
      'noteItemsCommitted',
      'noteRevealSynced',
    ]);
  });
});
