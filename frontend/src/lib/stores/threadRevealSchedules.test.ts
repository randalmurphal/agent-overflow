import { afterEach, beforeEach, expect, it } from 'vitest';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { FakeSmoothingClock, installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { __setSmoothingClockForTest } from './threadPaneShared';
import { applyItemStreamEvent, flushItemEventQueue, resetItemEventQueue } from './eventsItemStream';
import { resetPanesForTest } from './panes.svelte';
import { getSettings } from './settings.svelte';

beforeEach(installThreadPaneTestEnv);
afterEach(() => {
  resetItemEventQueue();
  resetPanesForTest();
  __setSmoothingClockForTest(undefined);
  getSettings().lowPowerMode = false;
  getSettings().streamingEnabled = true;
});

// Replay the same valid stream under different chunk, flush, and frame
// schedules. The oracle describes observable behavior, not smoother internals.
it.each(Array.from({ length: 24 }, (_, seed) => seed + 1))(
  'preserves text, ordering and resource lifetime under delivery schedule %i',
  async (seed) => {
    let randomState = seed;
    const random = (bound: number) => {
      randomState = (Math.imul(randomState, 1664525) + 1013904223) >>> 0;
      return randomState % bound;
    };
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    getSettings().lowPowerMode = seed % 4 === 0;
    getSettings().streamingEnabled = seed % 6 !== 0;
    const thread = makeThread({ id: 'schedule', provider: seed % 2 ? 'codex' : 'claude' });
    const pane = await buildPane(thread);
    const prose = makeItem({ id: 'prose', threadId: thread.id, status: 'streaming', summary: '', itemIndex: 0 });
    const text = 'Checking the result 👩‍💻 and café.\n\n'.repeat(8);
    let received = '';
    let previous = '';
    let time = 1;
    const inspect = () => {
      const displayed = pane.getItemById(prose.id)?.summary ?? '';
      expect(displayed.startsWith(previous)).toBe(true);
      expect(received.startsWith(displayed)).toBe(true);
      previous = displayed;
      if (pane.__itemSmootherCountForTest() > 0) {
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
      }
    };
    applyItemStreamEvent({ action: 'upsert', threadId: thread.id, item: prose });
    for (let offset = 0; offset < text.length;) {
      const delta = text.slice(offset, offset + 1 + random(29));
      offset += delta.length;
      received += delta;
      applyItemStreamEvent({ action: 'delta', threadId: thread.id, itemId: prose.id,
        kind: prose.kind, delta, updatedAt: ++time });
      if (offset === delta.length) {
        applyItemStreamEvent({ action: 'upsert', threadId: thread.id,
          item: makeItem({ id: 'command', threadId: thread.id, itemIndex: 1,
            kind: 'tool_call', toolName: 'command_execution', status: 'completed', summary: 'git status' }) });
      }
      if (random(3) === 0) {
        // A cached prefix is allowed to lag the wire; it must not rewind the
        // reader or throw away the pending suffix, even across batch splits.
        applyItemStreamEvent({ action: 'upsert', threadId: thread.id,
          item: { ...prose, summary: received.slice(0, random(received.length + 1)), updatedAt: ++time } });
      }
      if (random(2) === 0) flushItemEventQueue();
      if (seed % 5 === 0 && random(4) === 0) pane.snapSmoothersToReceived();
      const revision = pane.timelineRevision;
      for (let frame = random(20); frame > 0; frame--) {
        clock.tickFrame(8 + random(43));
        inspect();
      }
      // Text animation cannot rebuild the structural item window.
      expect(pane.timelineRevision).toBe(revision);
    }
    if (seed % 2) {
      applyItemStreamEvent({ action: 'patch', threadId: thread.id, itemId: prose.id,
        kind: prose.kind, patch: { status: 'completed', summary: text, updatedAt: ++time } });
    } else {
      applyItemStreamEvent({ action: 'upsert', threadId: thread.id,
        item: { ...prose, status: 'completed', summary: text, updatedAt: ++time } });
    }
    flushItemEventQueue();
    for (let frame = 0; frame < 1500 && pane.__itemSmootherCountForTest() > 0; frame++) {
      clock.tickFrame(16);
      inspect();
    }
    expect(pane.getItemById(prose.id)?.summary).toBe(text);
    expect(pane.revealBoundary).toBeNull();
    expect(pane.__itemSmootherCountForTest()).toBe(0);
    expect(clock.pendingCount()).toBe(0);

    // Leave another stream in flight, then tear down its owner. Advancing the
    // old clock must not resurrect rows or retain a reveal callback.
    pane.upsertItem({ ...prose, id: 'cleanup', itemIndex: 2 });
    pane.applyItemDelta({ threadId: thread.id, itemId: 'cleanup', kind: prose.kind,
      delta: ' pending cleanup '.repeat(20), updatedAt: ++time });
    pane.clear();
    clock.tickFrame(1000);
    expect(pane.items).toHaveLength(0);
    expect(pane.revealBoundary).toBeNull();
    expect(clock.pendingCount()).toBe(0);
  },
);
