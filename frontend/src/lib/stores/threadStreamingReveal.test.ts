import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Item } from '../types/models';
import type { SmoothingClock } from '../markdown/smoothing/PerItemSmoother';
import { makeItem } from '../../test/helpers/chat';
import { expectCleanTransitions } from '../../test/helpers/transitions';
import { __setSmoothingClockForTest } from './threadPaneShared';
import { createThreadStreamingReveal } from './threadStreamingReveal.svelte';
import type { StreamingAssistantRevealSink } from './streamingAssistantReveal';
import { getSettings, resetSettingsForTest } from './settings.svelte';
import { matchesProvenAppend } from 'svelte-streamdown';

class FakeSmoothingClock implements SmoothingClock {
  private nextHandle = 1;
  private readonly pending = new Map<number, () => void>();
  private current = 0;

  now(): number {
    return this.current;
  }

  schedule(callback: () => void): number {
    const handle = this.nextHandle++;
    this.pending.set(handle, callback);
    return handle;
  }

  cancel(handle: number): void {
    this.pending.delete(handle);
  }

  tick(ms: number): void {
    this.current += ms;
    const pending = [...this.pending.values()];
    this.pending.clear();
    for (const callback of pending) callback();
  }
}

function sink(
  reset: () => void,
  canAppendLiteral: () => boolean = () => true,
): StreamingAssistantRevealSink {
  return {
    canAppendLiteral,
    appendLiteral: () => {},
    restoreLiteral: () => true,
    reset,
  };
}

function makeReveal(initialItems: Item[]) {
  let items = initialItems;
  let nextItemWriteFailure: unknown;
  let itemWriteCount = 0;
  const indexById = new Map(items.map((item, index) => [item.id, index]));
  const reveal = createThreadStreamingReveal({
    getItemById: (itemId) => {
      const index = indexById.get(itemId);
      return index === undefined ? undefined : items[index];
    },
    getItemIndex: (itemId) => indexById.get(itemId),
    getItems: () => items,
    setItemAt: (index, item) => {
      if (nextItemWriteFailure !== undefined) {
        const failure = nextItemWriteFailure;
        nextItemWriteFailure = undefined;
        throw failure;
      }
      if (items[index]?.id !== item.id) throw new Error('test wrote the wrong item');
      const next = items.slice();
      next[index] = item;
      items = next;
      itemWriteCount++;
    },
    appendDirectAssistantLiteral: (
      index,
      itemId,
      append,
      updatedAt,
    ) => {
      const current = items[index];
      if (
        !current
        || current.id !== itemId
        || !matchesProvenAppend(append, current.summary, append.next)
      ) {
        throw new Error('test direct write mismatch');
      }
      current.summary = append.next;
      current.updatedAt = Math.max(current.updatedAt, updatedAt);
    },
    stampLiveContent: () => {},
    armStructuralSpring: () => {},
    appendLivePayloadDeltaForItem: () => {},
  });
  return {
    reveal,
    getItems: () => items,
    getItemWriteCount: () => itemWriteCount,
    failNextItemWrite(failure: unknown) {
      nextItemWriteFailure = failure;
    },
  };
}

afterEach(() => {
  __setSmoothingClockForTest(undefined);
  resetSettingsForTest();
});

describe('streaming reveal live-updates toggle transitions', () => {
  // `streamingEnabled` is the live-updates toggle: off, the smoother
  // hands its whole pending buffer over in one mutation instead of
  // animating (`revealImmediately`). The existing coverage flips it once;
  // these are the laps a toggle has to survive — repeated flips, a flip
  // that lands while a backlog is still draining, and the settled state
  // being identical after every one of them.
  it('returns the reveal to a clean settled state across every flip', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, getItems } = makeReveal([item]);

    const BACKLOG = Array.from({ length: 60 }, (_, i) => `word${i} `).join('');
    let received = '';
    let updatedAt = 1;

    const append = (delta: string): void => {
      reveal.appendStreamingDelta(item.id, getItems()[0].summary, delta, ++updatedAt);
      received += delta;
    };
    const drain = (): void => {
      for (let frame = 0; frame < 2_000; frame++) {
        if (getItems()[0].summary === received) return;
        clock.tick(16);
      }
      throw new Error('reveal did not drain');
    };

    append('seed words ');
    drain();

    expectCleanTransitions('streaming reveal live-updates toggle', {
      on: () => { getSettings().streamingEnabled = false; },
      // Disengaging restores the animated cadence and lets whatever is
      // pending settle — a toggle is only clean once the reveal it
      // governs has finished.
      off: () => {
        getSettings().streamingEnabled = true;
        drain();
      },
      whileOn: () => {
        // Live updates off means the whole backlog lands in one tick.
        append(BACKLOG);
        clock.tick(16);
        expect(getItems()[0].summary).toBe(received);
      },
      inFlight: () => { append(BACKLOG); },
      read: () => ({
        smoothers: reveal.smootherCount(),
        boundary: reveal.revealBoundary,
        caughtUp: getItems()[0].summary === received,
        tails: reveal.debugStats().liveThinkingTails,
      }),
    });

    expect(getItems()[0].summary).toBe(received);
  });
});

describe('thread streaming reveal cleanup', () => {
  it('keeps a terminal same-stream replacement on the bounded reveal cursor', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, getItems } = makeReveal([item]);
    const received = Array.from(
      { length: 120 },
      (_, index) => `word${String(index).padStart(3, '0')} `,
    ).join('');
    reveal.appendStreamingDelta(item.id, '', received, 2);
    clock.tick(100);
    const revealedAtCompletion = getItems()[0].summary;
    expect(revealedAtCompletion.length).toBeGreaterThan(0);
    expect(revealedAtCompletion.length).toBeLessThan(received.length);

    const [prepared] = reveal.prepareItemReplacements([{
      ...getItems()[0],
      status: 'completed',
      summary: received,
      updatedAt: 3,
    }]);

    expect(prepared.status).toBe('completed');
    expect(prepared.summary).toBe(revealedAtCompletion);
    expect(reveal.smootherCount()).toBe(1);
    getItems()[0] = prepared;

    let previousLength = prepared.summary.length;
    let frames = 0;
    while (reveal.smootherCount() > 0 && frames++ < 1_000) {
      clock.tick(16);
      const length = getItems()[0].summary.length;
      expect(length).toBeGreaterThanOrEqual(previousLength);
      expect(length - previousLength).toBeLessThanOrEqual(128);
      previousLength = length;
    }
    expect(frames).toBeLessThan(1_000);
    expect(getItems()[0].summary).toBe(received);
    expect(reveal.revealBoundary).toBeNull();
  });

  it('snaps a killed full-row replacement instead of leaving a drain alive', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, getItems } = makeReveal([item]);
    const received = 'word '.repeat(120);
    reveal.appendStreamingDelta(item.id, '', received, 2);
    clock.tick(100);
    expect(getItems()[0].summary.length).toBeLessThan(received.length);

    const incoming = {
      ...getItems()[0],
      status: 'killed' as const,
      summary: `${received}— stopped`,
      updatedAt: 3,
    };
    const [prepared] = reveal.prepareItemReplacements([incoming]);

    expect(prepared).toBe(incoming);
    expect(reveal.smootherCount()).toBe(0);
    expect(reveal.revealBoundary).toBeNull();
  });

  it('drops a smoother whose advanced cursor cannot commit its row', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, failNextItemWrite } = makeReveal([item]);
    reveal.appendStreamingDelta(item.id, '', 'pending words ', 1);
    expect(reveal.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
    failNextItemWrite(new Error('row commit failed'));

    expect(() => clock.tick(100)).toThrow('row commit failed');
    expect(reveal.smootherCount()).toBe(0);
    expect(reveal.revealBoundary).toBeNull();
  });

  it('does not claim append lineage across an equal-length summary rewrite', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, getItems } = makeReveal([item]);
    reveal.appendStreamingDelta(item.id, '', 'alpha ', 1);
    clock.tick(100);

    const previous = getItems()[0];
    const rewritten = { ...previous, summary: 'bravo ' };
    reveal.reconcileItemWrite(previous, rewritten);
    getItems()[0] = rewritten;
    reveal.appendStreamingDelta(item.id, rewritten.summary, 'tail ', 2);
    clock.tick(100);

    const finalSource = getItems()[0].summary;
    expect(finalSource).toBe('alpha tail ');
    expect(reveal.assistantSourceAppend(item.id, finalSource)).toBeUndefined();
  });

  it('keeps the smoother source and gate coherent after a sink preflight fails', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, getItems } = makeReveal([item]);
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    reveal.registerAssistantRevealSink(item.id, sink(
      () => {},
      () => { throw new Error('preflight failed'); },
    ));

    reveal.appendStreamingDelta(item.id, '', 'first ', 1);
    clock.tick(100);
    reveal.appendStreamingDelta(item.id, 'first ', 'second ', 2);
    expect(() => clock.tick(100)).not.toThrow();

    expect(getItems()[0].summary).toBe('first second ');
    expect(reveal.revealBoundary).toBeNull();
    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining('fell back after a sink failure'),
      expect.stringContaining('phase=preflight'),
    );
    warn.mockRestore();
  });

  it('drops a smoother even when its mounted sink cannot reset', () => {
    __setSmoothingClockForTest(new FakeSmoothingClock());
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal } = makeReveal([item]);
    const reset = vi.fn()
      .mockImplementationOnce(() => { throw new Error('reset failed'); })
      .mockImplementation(() => {});
    reveal.registerAssistantRevealSink(item.id, sink(reset));
    reveal.appendStreamingDelta(item.id, '', 'pending words ', 1);

    expect(() => reveal.disposeSmootherFor(item.id)).toThrow(
      /smoother disposal failed/,
    );
    expect(reveal.smootherCount()).toBe(0);
    expect(reveal.revealBoundary).toBeNull();
    expect(reveal.debugStats().liveThinkingTails).toBe(0);
    expect(() => reveal.disposeSmootherFor(item.id)).not.toThrow();
  });

  it('retains a content-consistent thinking tail when settle cleanup reports an error', () => {
    __setSmoothingClockForTest(new FakeSmoothingClock());
    const item = makeItem({
      id: 'thinking',
      kind: 'thinking',
      status: 'streaming',
      summary: '',
    });
    const { reveal, getItems } = makeReveal([item]);
    reveal.registerAssistantRevealSink(item.id, sink(() => {
      throw new Error('settle reset failed');
    }));
    reveal.appendStreamingDelta(item.id, '', 'reasoning words ', 1);

    expect(() => reveal.__flushForTest()).toThrow(/smoother settle failed/);
    expect(reveal.smootherCount()).toBe(0);
    expect(reveal.revealBoundary).toBeNull();
    expect(getItems()[0].summary).toBe('reasoning words ');
    expect(reveal.liveThinkingTailFor(item.id)).toBe('reasoning words ');
  });

  it('drops the gate when terminal onReveal cleanup reports an error', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({
      id: 'thinking',
      kind: 'thinking',
      status: 'streaming',
      summary: '',
    });
    const { reveal, getItems } = makeReveal([item]);
    const reset = vi.fn(() => { throw new Error('settle reset failed'); });
    reveal.registerAssistantRevealSink(item.id, sink(reset));
    reveal.appendStreamingDelta(item.id, '', 'reasoning words ', 1);
    expect(reveal.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
    getItems()[0].status = 'completed';

    expect(() => clock.tick(100)).toThrow(/smoother settle failed/);
    expect(reveal.smootherCount()).toBe(0);
    expect(reveal.revealBoundary).toBeNull();
  });

  it('reactively republishes a direct suffix when terminal cleanup retires its DOM', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, getItems, getItemWriteCount } = makeReveal([item]);
    let writesAtReset = -1;
    const reset = vi.fn(() => {
      writesAtReset = getItemWriteCount();
    });
    reveal.registerAssistantRevealSink(item.id, sink(reset));

    reveal.appendStreamingDelta(item.id, '', 'first ', 1);
    clock.tick(100);
    expect(getItemWriteCount()).toBe(1);

    reveal.appendStreamingDelta(item.id, 'first ', 'second ', 2);
    getItems()[0] = { ...getItems()[0], status: 'completed' };
    clock.tick(100);

    expect(getItems()[0].summary).toBe('first second ');
    expect(writesAtReset).toBeGreaterThanOrEqual(1);
    expect(getItemWriteCount()).toBeGreaterThan(writesAtReset);
    expect(reset).toHaveBeenCalledOnce();
    expect(reveal.smootherCount()).toBe(0);
    expect(reveal.revealBoundary).toBeNull();
  });

  it('drops the gate when patch disposal reports an error', () => {
    __setSmoothingClockForTest(new FakeSmoothingClock());
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal } = makeReveal([item]);
    const reset = vi.fn(() => { throw new Error('patch reset failed'); });
    reveal.registerAssistantRevealSink(item.id, sink(reset));
    reveal.appendStreamingDelta(item.id, '', 'pending words ', 1);
    expect(reveal.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

    expect(() => reveal.applyPatch(item.id, {
      status: 'completed',
      summary: 'replacement ',
    })).toThrow(/smoother disposal failed/);
    expect(reveal.smootherCount()).toBe(0);
    expect(reveal.revealBoundary).toBeNull();
  });

  it('disposes a snapped patch smoother even when sink cleanup fails', () => {
    __setSmoothingClockForTest(new FakeSmoothingClock());
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal } = makeReveal([item]);
    reveal.registerAssistantRevealSink(item.id, sink(() => {
      throw new Error('snap reset failed');
    }));
    reveal.appendStreamingDelta(item.id, '', 'pending words ', 1);

    expect(() => reveal.applyPatch(item.id, {
      status: 'killed',
      summary: 'interrupted',
    })).toThrow(/smoother disposal failed/);
    expect(reveal.smootherCount()).toBe(0);
    expect(reveal.revealBoundary).toBeNull();
  });

  it('snaps every row and recomputes after one mounted sink fails', () => {
    __setSmoothingClockForTest(new FakeSmoothingClock());
    const first = makeItem({
      id: 'first',
      status: 'streaming',
      summary: '',
      itemIndex: 0,
    });
    const second = makeItem({
      id: 'second',
      status: 'streaming',
      summary: '',
      itemIndex: 1,
    });
    const { reveal, getItems } = makeReveal([first, second]);
    reveal.registerAssistantRevealSink(first.id, sink(() => {
      throw new Error('visibility reset failed');
    }));
    reveal.appendStreamingDelta(first.id, '', 'first pending ', 1);
    reveal.appendStreamingDelta(second.id, '', 'second pending ', 1);
    expect(reveal.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
    getItems()[0].status = 'completed';

    expect(() => reveal.snapAllToReceived()).toThrow(/smoother settle failed/);
    expect(getItems()[1].summary).toBe('second pending ');
    expect(reveal.revealBoundary).toBeNull();
  });

  it('prepares every terminal replacement when one sink reset fails', () => {
    __setSmoothingClockForTest(new FakeSmoothingClock());
    const first = makeItem({
      id: 'first',
      status: 'streaming',
      summary: '',
      itemIndex: 0,
    });
    const second = makeItem({
      id: 'second',
      status: 'streaming',
      summary: '',
      itemIndex: 1,
    });
    const { reveal, getItems } = makeReveal([first, second]);
    const secondReset = vi.fn();
    reveal.registerAssistantRevealSink(first.id, sink(() => {
      throw new Error('first upsert reset failed');
    }));
    reveal.registerAssistantRevealSink(second.id, sink(secondReset));
    reveal.appendStreamingDelta(first.id, '', 'first pending ', 1);
    reveal.appendStreamingDelta(second.id, '', 'second pending ', 1);
    // A DIVERGENT terminal summary is what forces disposal. A terminal
    // replacement whose summary is still a prefix of `received` keeps the
    // smoother draining instead (incident 2026-08-29: disposing on the
    // drain's own partial row stranded the final text mid-reveal).
    const terminalItems = getItems().map((current) => ({
      ...current,
      status: 'completed' as const,
      summary: `rewritten ${current.id}`,
    }));
    expect(() => reveal.prepareItemReplacements(terminalItems)).toThrow(
      /smoother disposal failed/,
    );
    expect(secondReset).toHaveBeenCalledOnce();
    expect(reveal.smootherCount()).toBe(0);
    reveal.recomputeReveal();
    expect(reveal.revealBoundary).toBeNull();
  });

  it('disposes every smoother and clears the gate when one sink reset fails', () => {
    __setSmoothingClockForTest(new FakeSmoothingClock());
    const first = makeItem({
      id: 'first',
      status: 'streaming',
      summary: '',
      itemIndex: 0,
    });
    const second = makeItem({
      id: 'second',
      status: 'streaming',
      summary: '',
      itemIndex: 1,
    });
    const { reveal } = makeReveal([first, second]);
    const secondReset = vi.fn();
    reveal.registerAssistantRevealSink(first.id, sink(() => {
      throw new Error('first reset failed');
    }));
    reveal.registerAssistantRevealSink(second.id, sink(secondReset));
    reveal.appendStreamingDelta(first.id, '', 'first words ', 1);
    reveal.appendStreamingDelta(second.id, '', 'second words ', 1);
    expect(reveal.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
    const generationBeforeDisposal = reveal.assistantRevealRegistrationGeneration;

    expect(() => reveal.disposeAll()).toThrow(/streaming reveal disposal failed/);
    expect(reveal.assistantRevealRegistrationGeneration).toBe(
      generationBeforeDisposal + 1,
    );
    expect(secondReset).toHaveBeenCalledOnce();
    expect(reveal.smootherCount()).toBe(0);
    expect(reveal.revealBoundary).toBeNull();
    expect(reveal.debugStats()).toEqual({
      itemSmoothers: 0,
      liveThinkingTails: 0,
      liveThinkingTailChars: 0,
    });

    const replacementReset = vi.fn();
    expect(() => reveal.registerAssistantRevealSink(
      first.id,
      sink(replacementReset),
    )).not.toThrow();
  });
});
