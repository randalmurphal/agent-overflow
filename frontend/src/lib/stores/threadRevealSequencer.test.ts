// stores/threadRevealSequencer.test.ts
//
// The reveal ORDERING half of threadStreamingReveal (threadRevealGate):
// `revealBoundary` releases rows in wire order, and nothing may skip, rush
// or pop the readable drain — the reveal-queue doctrine in stores/AGENTS.md.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { __setSmoothingClockForTest } from './thread.svelte';
import {
  MAX_ADAPTIVE_CHARS_PER_SEC,
  MAX_ADVANCE_PER_TICK_CHARS,
} from '../markdown/smoothing/PerItemSmoother';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { FakeSmoothingClock, installThreadPaneTestEnv } from '../../test/helpers/threadPane';

describe('threadRevealGate', () => {
  beforeEach(installThreadPaneTestEnv);

  describe('reveal sequencer (revealBoundary)', () => {
    function streamingThinking(
      id: string,
      itemIndex: number,
      threadId: string,
    ) {
      return makeItem({
        id,
        threadId,
        kind: 'thinking',
        role: 'assistant',
        status: 'streaming',
        turnIndex: 0,
        itemIndex,
        summary: '',
        payloadId: `thinking:${id}`,
        updatedAt: 1,
      });
    }

    it('starts with no gate', async () => {
      const pane = await buildPane(makeThread({ id: 't' }));
      expect(pane.revealBoundary).toBeNull();
    });

    it('synchronizes the gate after an upsert bookkeeping failure', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        const streaming = streamingThinking('think:0:0', 0, 't');
        pane.upsertItem(streaming);
        pane.applyItemDelta({
          threadId: 't',
          itemId: streaming.id,
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        expect(pane.__itemSmootherCountForTest()).toBe(1);

        const bookkeepingFailure = new Error('activity summary bookkeeping failed');
        vi.spyOn(pane.activityRuns, 'noteMemberContentChanged').mockImplementation(() => {
          throw bookkeepingFailure;
        });

        expect(() =>
          pane.upsertItem({
            ...streaming,
            status: 'killed',
            updatedAt: 3,
          }),
        ).toThrowError(
          expect.objectContaining({
            message: 'timeline item upsert finalization failed',
            errors: expect.arrayContaining([bookkeepingFailure]),
          }),
        );

        // The row commit cannot be rolled back, so every post-commit domain
        // leg still runs before the error escapes. In particular, terminal
        // reconciliation disposes the smoother and derives the boundary from
        // the installed window rather than leaving a stale gate behind.
        expect(pane.items[0].status).toBe('killed');
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('repairs the old-window gate when replacement preparation fails', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        const streaming = makeItem({
          id: 'text:0:0',
          threadId: 't',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          turnIndex: 0,
          itemIndex: 0,
          summary: '',
          updatedAt: 1,
        });
        pane.upsertItem(streaming);
        const reset = vi.fn();
        const unregister = pane.registerAssistantRevealSink(streaming.id, {
          canAppendLiteral: () => true,
          appendLiteral: () => {},
          restoreLiteral: () => true,
          reset,
        });
        try {
          pane.applyItemDelta({
            threadId: 't',
            itemId: streaming.id,
            kind: streaming.kind,
            delta: 'word '.repeat(40),
            updatedAt: 2,
          });
          expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
          expect(pane.__itemSmootherCountForTest()).toBe(1);

          reset.mockImplementationOnce(() => {
            throw new Error('replacement sink reset failed');
          });

          expect(() =>
            pane.upsertItem({
              ...streaming,
              status: 'killed',
              updatedAt: 3,
            }),
          ).toThrow('streaming reveal smoother disposal failed');

          // Preparation runs before the row commit, so the terminal echo is
          // rejected. Its failed reset still disposed the smoother. The gate
          // must describe that old streaming window rather than the rejected
          // incoming row and therefore clears with its removed owner.
          expect(pane.items[0].status).toBe('streaming');
          expect(pane.__itemSmootherCountForTest()).toBe(0);
          expect(pane.revealBoundary).toBeNull();
          expect(reset).toHaveBeenCalledTimes(1);
        } finally {
          unregister();
        }
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps the row and row signal paired after in-place bookkeeping fails', async () => {
      const pane = await buildPane(makeThread({ id: 't' }));
      const item = makeItem({
        id: 'text:0:0',
        threadId: 't',
        kind: 'assistant_text',
        role: 'assistant',
        status: 'streaming',
        summary: 'before',
      });
      pane.upsertItem(item);
      const bookkeepingFailure = new Error('activity summary bookkeeping failed');
      vi.spyOn(pane.activityRuns, 'noteMemberContentChanged').mockImplementation(() => {
        throw bookkeepingFailure;
      });

      expect(() => pane.applyItemPatch({
        threadId: 't',
        itemId: item.id,
        kind: item.kind,
        patch: {
          status: 'completed',
          summary: 'after',
          updatedAt: item.updatedAt + 1,
        },
      })).toThrowError(expect.objectContaining({
        message: `timeline item write finalization failed for ${item.id}`,
        errors: expect.arrayContaining([bookkeepingFailure]),
      }));

      expect(pane.items[0].summary).toBe('after');
      expect(pane.getItemById(item.id)?.summary).toBe('after');
    });

    it('a solo streaming row gates at itself but withholds nothing at the tail', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        // Frontier is the only/last node → boundary points at it but the
        // slice helper (covered in subagentGrouping.test.ts) withholds nothing.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // It drains at the ordinary reveal cadence and the gate drops.
        for (let i = 0; i < 200 && pane.revealBoundary !== null; i++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('withholds the next top-level row until the streaming item drains', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        // Wire moves on: a tool call appears while the thinking still lags.
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );
        // Gate stays at the thinking row — the tool call is withheld.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // The thinking row finishes at the ordinary cadence — no rush —
        // and only then does the gate drop.
        for (let i = 0; i < 200 && pane.revealBoundary !== null; i++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it.each(['assistant_text', 'thinking'] as const)(
      'never retracts a released row when earlier %s resumes, but still gates new rows',
      async (kind) => {
        const clock = new FakeSmoothingClock();
        __setSmoothingClockForTest(clock);
        try {
          const pane = await buildPane(makeThread({ id: 't' }));
          pane.upsertItem({ ...streamingThinking('text', 0, 't'), kind });
          const append = () => pane.applyItemDelta({
            threadId: 't', itemId: 'text', kind, delta: 'word '.repeat(40), updatedAt: 2,
          });
          const command = (id: string, itemIndex: number) => makeItem({
            id, itemIndex, turnIndex: 0, threadId: 't', kind: 'tool_call',
            toolName: 'Bash', status: 'running', summary: 'Bash: git status',
          });
          append();
          pane.upsertItem(command('released', 1));
          expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
          for (let i = 0; i < 300 && pane.revealBoundary !== null; i++) clock.tickFrame(16);
          expect(pane.revealBoundary).toBeNull();

          append();
          pane.upsertItem(command('withheld', 2));
          expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
          // Removing the released row also retires its visibility floor.
          pane.removeItemById('released', 't');
          pane.upsertItem(command('replacement', 1));
          expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
          for (let i = 0; i < 300 && pane.revealBoundary !== null; i++) clock.tickFrame(16);
          expect(pane.revealBoundary).toBeNull();

          // A fresh thread reuses positions, never the previous visibility.
          await pane.switchThread(makeThread({ id: 'other' }));
          pane.upsertItem({ ...streamingThinking('new-text', 0, 'other'), kind });
          pane.applyItemDelta({ threadId: 'other', itemId: 'new-text', kind,
            delta: 'word '.repeat(40), updatedAt: 2 });
          pane.upsertItem({ ...command('new-command', 1), threadId: 'other' });
          expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        } finally {
          __setSmoothingClockForTest(undefined);
        }
      },
    );

    it('a waiting successor does not speed the frontier up', async () => {
      // The successor-waiting fast-drain is gone: a queued row changes
      // WHAT renders (it is withheld), never how fast the frontier
      // animates. Both panes get the same backlog; the one with a
      // successor must not finish sooner.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const solo = await buildPane(makeThread({ id: 'solo' }));
        solo.upsertItem(streamingThinking('think:0:0', 0, 'solo'));
        solo.applyItemDelta({
          threadId: 'solo',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });

        const gated = await buildPane(makeThread({ id: 'gated' }));
        gated.upsertItem(streamingThinking('think:0:0', 0, 'gated'));
        gated.applyItemDelta({
          threadId: 'gated',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        gated.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 'gated',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );

        // Both panes share the clock, so one loop drives both.
        let soloFrames = 0;
        let gatedFrames = 0;
        for (let i = 1; i <= 300; i++) {
          clock.tickFrame(16);
          if (soloFrames === 0 && solo.revealBoundary === null) soloFrames = i;
          if (gatedFrames === 0 && gated.revealBoundary === null) gatedFrames = i;
        }
        expect(soloFrames).toBeGreaterThan(0);
        expect(gatedFrames).toBe(soloFrames);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('pauses a withheld smoothed successor so it animates from the start', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        // A streaming assistant_text successor arrives and gets its deltas
        // while still withheld behind the thinking row.
        pane.upsertItem(
          makeItem({
            id: 'text:0:1',
            threadId: 't',
            kind: 'assistant_text',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 1,
            summary: '',
            updatedAt: 3,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:1',
          kind: 'assistant_text',
          delta: 'Hello world this is the answer',
          updatedAt: 4,
        });
        const textIdx = pane.items.findIndex((i) => i.id === 'text:0:1');

        // While withheld, the successor's reveal is paused at its seed.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        for (let i = 0; i < 5; i++) clock.tickFrame(16);
        expect(pane.items[textIdx].summary).toBe('');

        // Thinking drains → gate advances to the text row, which now
        // reveals from the start.
        for (
          let i = 0;
          i < 200 && pane.revealBoundary?.itemIndex === 0;
          i++
        ) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
        for (let i = 0; i < 200 && pane.revealBoundary !== null; i++) {
          clock.tickFrame(16);
        }
        expect(pane.items[textIdx].summary).toBe(
          'Hello world this is the answer',
        );
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('never lets a subagent child become the frontier (no cross-branch gating)', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        // Agent launch (top-level, non-smoothed) + a streaming child thinking.
        pane.upsertItem(
          makeItem({
            id: 'agent:0:0',
            threadId: 't',
            kind: 'tool_call',
            toolName: 'Agent',
            status: 'running',
            turnIndex: 0,
            itemIndex: 0,
            summary: 'Agent',
            updatedAt: 1,
          }),
        );
        pane.upsertItem(
          makeItem({
            id: 'child:0:1',
            threadId: 't',
            kind: 'thinking',
            parentId: 'agent:0:0',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 1,
            summary: '',
            payloadId: 'thinking:child',
            updatedAt: 2,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'child:0:1',
          kind: 'thinking',
          delta: 'subagent reasoning '.repeat(5),
          updatedAt: 3,
        });
        // A subagent descendant must not gate the timeline.
        expect(pane.revealBoundary).toBeNull();

        // A later top-level text becomes the frontier; the child is ignored.
        pane.upsertItem(
          makeItem({
            id: 'text:0:2',
            threadId: 't',
            kind: 'assistant_text',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 2,
            summary: '',
            updatedAt: 4,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:2',
          kind: 'assistant_text',
          delta: 'top level answer',
          updatedAt: 5,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 2 });
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('drops the gate when the streaming item is interrupted', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // Interrupt kills the thinking row → snap + dispose → gate drops.
        pane.applyItemPatch({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          patch: { status: 'killed', updatedAt: 4 },
        });
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('holds the gate while a completion patch extends the frontier, then drops it once the suffix drains', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 0,
            summary: '',
            updatedAt: 1,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: 'hello ',
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // Turn-completion patch carries the final text, extending what streamed.
        pane.applyItemPatch({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: {
            status: 'completed',
            summary: 'hello world done',
            updatedAt: 4,
          },
        });
        // Gate still held — the appended suffix hasn't revealed yet.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        for (let i = 0; i < 80; i++) clock.tickFrame(16);
        expect(pane.revealBoundary).toBeNull();
        const text = pane.items.find((i) => i.id === 'text:0:0');
        expect(text?.summary).toBe('hello world done');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('reveals a thinking → text → tool_call chain in order', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'text:0:1',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 1,
            summary: '',
            updatedAt: 3,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:1',
          kind: 'assistant_text',
          delta: 'the answer here',
          updatedAt: 4,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:2',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 2,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 5,
          }),
        );
        // Gate at thinking; text AND tool both withheld.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // thinking drains → gate steps to the text row (not straight to null).
        for (let i = 0; i < 200 && pane.revealBoundary?.itemIndex === 0; i++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
        // text drains → gate drops (tool has no smoother, reveals immediately).
        for (let i = 0; i < 200 && pane.revealBoundary !== null; i++) {
          clock.tickFrame(16);
        }
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('resumes a paused successor when the frontier row is removed', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'text:0:1',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 1,
            summary: '',
            updatedAt: 3,
          }),
        );
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:1',
          kind: 'assistant_text',
          delta: 'the answer',
          updatedAt: 4,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        // Optimistic revert removes the streaming frontier row.
        pane.removeItemById('think:0:0', 't');
        // The withheld successor becomes the frontier and resumes from its start.
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 1 });
        for (let i = 0; i < 60; i++) clock.tickFrame(16);
        expect(pane.items.find((i) => i.id === 'text:0:1')?.summary).toBe(
          'the answer',
        );
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('resets the gate to null on thread switch', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: 'word '.repeat(40),
          updatedAt: 2,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 3,
          }),
        );
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
        await pane.switchThread(makeThread({ id: 'other-thread' }));
        expect(pane.revealBoundary).toBeNull();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('leaves the gate null for a settled thread (no streaming)', async () => {
      const pane = await buildPane(makeThread({ id: 't' }), [
        makeItem({
          id: 'u:0',
          threadId: 't',
          kind: 'user_text',
          role: 'user',
          summary: 'hi',
          turnIndex: 0,
          itemIndex: 0,
        }),
        makeItem({
          id: 'a:1',
          threadId: 't',
          kind: 'assistant_text',
          summary: 'done',
          turnIndex: 0,
          itemIndex: 1,
        }),
      ]);
      expect(pane.revealBoundary).toBeNull();
    });

    it('holds a successor behind a multi-KB reasoning frontier for the whole readable drain', async () => {
      // The contract that replaced BOTH removed shortcuts (the
      // successor-waiting fast-drain, then the bounded-backlog skip): a
      // queued row waits. It waits for every character of the frontier to
      // animate, at no more than the reveal ceiling, and is released only
      // when the frontier is genuinely caught up.
      //
      // This is affordable because the wire is bursty — the drain below
      // runs with NO further appends, which is exactly what a tool call or
      // an API round-trip looks like. Do not "fix" a long wait here by
      // skipping, rushing, or popping the frontier.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(streamingThinking('think:0:0', 0, 't'));
        const text = 'word '.repeat(1200); // 6000 chars
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'think:0:0',
          kind: 'thinking',
          delta: text,
          updatedAt: 3,
        });
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 4,
          }),
        );
        expect(pane.liveThinkingTailForItem('think:0:0')).toBeNull();
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        let revealedLength = 0;
        let frames = 0;
        // The wire gap: frames tick, nothing new arrives.
        while (pane.revealBoundary !== null && frames < 3000) {
          clock.tickFrame(16);
          frames++;
          const length = pane.liveThinkingTailForItem('think:0:0')?.length ?? 0;
          // Every frame is ordinary bounded work — no frame hands over a
          // skipped middle.
          expect(length - revealedLength).toBeLessThanOrEqual(
            MAX_ADVANCE_PER_TICK_CHARS,
          );
          revealedLength = length;
          // The gate may only release on the frame the frontier finishes.
          // Releasing on any earlier frame would mean it popped a still
          // -revealing row.
          if (pane.revealBoundary === null) {
            expect(length).toBe(text.length);
          }
        }
        // Released exactly at catch-up, with every character animated.
        expect(pane.revealBoundary).toBeNull();
        expect(pane.liveThinkingTailForItem('think:0:0')).toBe(text);
        // And it took the ceiling-implied time — the wait was paid, not
        // shortened. ~19s of frames for 6000 chars at 320cps.
        expect(frames * 16).toBeGreaterThanOrEqual(
          (text.length / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000,
        );
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps direct rows quiet while imperative readers and parser checkpoints stay current', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        const renderContext = {
          streaming: true,
          volatileTailVisible: true,
          pathLinksInert: false,
          workspacePath: '/workspace',
          previewKey: '',
        } as const;
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 0,
            summary: '',
            updatedAt: 2,
          }),
        );
        const appended: string[] = [];
        const unregister = pane.registerAssistantRevealSink('text:0:0', {
          canAppendLiteral: () => true,
          appendLiteral: (_nextSource, delta) => appended.push(delta),
          restoreLiteral: () => true,
          reset: () => {},
        });
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: 'first ',
          updatedAt: 3,
        });
        for (let frames = 0; clock.pendingCount() > 0 && frames < 100; frames++) {
          clock.tickFrame(16);
        }
        const checkpoint = pane.items[0];

        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: 'second third ',
          updatedAt: 4,
        });
        for (let frames = 0; clock.pendingCount() > 0 && frames < 100; frames++) {
          clock.tickFrame(16);
        }

        expect(pane.items[0]).toBe(checkpoint);
        expect(pane.getItemById('text:0:0')).toBe(checkpoint);
        expect(pane.items[0].summary).toBe('first second third ');
        expect(pane.assistantMarkdownParserSource(
          'text:0:0',
          pane.items[0].summary,
          renderContext,
        )).toBe('first ');
        expect(appended.join('')).toBe('second third ');
        expect(pane.assistantMarkdownSourceAppend(
          'text:0:0',
          pane.items[0].summary,
        )).toBeUndefined();

        pane.applyItemMeta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          meta: '{"pathRefs":[]}',
          updatedAt: 4,
        });
        const metadataRewrite = pane.items[0];
        expect(metadataRewrite).not.toBe(checkpoint);
        expect(pane.assistantMarkdownParserSource(
          'text:0:0',
          pane.items[0].summary,
          renderContext,
        )).toBe('first second third ');
        const publishedDirectAppend = pane.assistantMarkdownSourceAppend(
          'text:0:0',
          pane.items[0].summary,
        );
        expect(publishedDirectAppend?.delta).toBe('second third ');

        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: '**',
          updatedAt: 5,
        });
        for (let frames = 0; clock.pendingCount() > 0 && frames < 100; frames++) {
          clock.tickFrame(16);
        }
        expect(pane.items[0]).not.toBe(metadataRewrite);
        expect(pane.items[0].summary).toBe('first second third **');
        expect(pane.assistantMarkdownParserSource(
          'text:0:0',
          pane.items[0].summary,
          renderContext,
        )).toBe('first second third **');
        expect(pane.assistantMarkdownSourceAppend(
          'text:0:0',
          pane.items[0].summary,
        )?.delta).toBe('**');
        await Promise.resolve();
        await Promise.resolve();
        expect(pane.assistantMarkdownSourceAppend(
          'text:0:0',
          pane.items[0].summary,
        )).toBeUndefined();
        unregister();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps the thread and window paired when reveal disposal aborts clear', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        const item = makeItem({
          id: 'text:0:0',
          threadId: 't',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: '',
        });
        pane.upsertItem(item);
        const reset = vi.fn();
        pane.registerAssistantRevealSink(item.id, {
          canAppendLiteral: () => true,
          appendLiteral: () => {},
          restoreLiteral: () => true,
          reset,
        });
        pane.applyItemDelta({
          threadId: 't',
          itemId: item.id,
          kind: item.kind,
          delta: 'first second ',
          updatedAt: 2,
        });
        while (clock.pendingCount() > 0) clock.tickFrame(16);
        reset.mockImplementationOnce(() => {
          throw new Error('sink disposal failed');
        });

        expect(() => pane.clear()).toThrow('streaming reveal disposal failed');
        expect(pane.thread?.id).toBe('t');
        expect(pane.items.map((row) => row.id)).toEqual([item.id]);
        expect(pane.__itemSmootherCountForTest()).toBe(0);
        expect(pane.revealBoundary).toBeNull();

        expect(() => pane.clear()).not.toThrow();
        expect(pane.thread).toBeNull();
        expect(pane.items).toEqual([]);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('aborts a switch before incoming state mutates when reveal disposal fails', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 'outgoing' }));
        const item = makeItem({
          id: 'text:0:0',
          threadId: 'outgoing',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: '',
        });
        pane.upsertItem(item);
        const reset = vi.fn();
        pane.registerAssistantRevealSink(item.id, {
          canAppendLiteral: () => true,
          appendLiteral: () => {},
          restoreLiteral: () => true,
          reset,
        });
        pane.applyItemDelta({
          threadId: 'outgoing',
          itemId: item.id,
          kind: item.kind,
          delta: 'first second ',
          updatedAt: 2,
        });
        while (clock.pendingCount() > 0) clock.tickFrame(16);
        reset.mockImplementationOnce(() => {
          throw new Error('switch disposal failed');
        });

        await expect(pane.switchThread(makeThread({ id: 'incoming' }))).rejects.toThrow(
          'streaming reveal disposal failed',
        );
        expect(pane.thread?.id).toBe('outgoing');
        expect(pane.items.map((row) => row.id)).toEqual([item.id]);
        expect(pane.loading).toBe(false);

        await expect(pane.switchThread(makeThread({ id: 'incoming' }))).resolves.toBeUndefined();
        expect(pane.thread?.id).toBe('incoming');
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('reconciles a streaming upsert that jumps ahead of the revealed cursor', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        const initial = makeItem({
          id: 'text:0:0',
          threadId: 't',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          updatedAt: 1,
        });
        pane.upsertItem(initial);
        const unregister = pane.registerAssistantRevealSink(initial.id, {
          canAppendLiteral: () => true,
          appendLiteral: () => {},
          restoreLiteral: () => true,
          reset: () => {},
        });

        pane.applyItemDelta({
          threadId: 't',
          itemId: initial.id,
          kind: 'assistant_text',
          delta: 'first ',
          updatedAt: 2,
        });
        while (clock.pendingCount() > 0) clock.tickFrame(100);

        pane.applyItemDelta({
          threadId: 't',
          itemId: initial.id,
          kind: 'assistant_text',
          delta: 'second third fourth fifth sixth seventh ',
          updatedAt: 3,
        });
        pane.upsertItem({
          ...pane.items[0],
          summary: 'first second third fourth fifth sixth seventh ',
          updatedAt: 4,
        });
        expect(pane.items[0].summary).toBe('first ');
        expect(pane.items[0].updatedAt).toBe(4);
        let previousLength = pane.items[0].summary.length;
        while (clock.pendingCount() > 0) {
          clock.tickFrame(100);
          expect(pane.items[0].summary.length).toBeGreaterThanOrEqual(previousLength);
          previousLength = pane.items[0].summary.length;
        }

        expect(pane.items[0].summary).toBe('first second third fourth fifth sixth seventh ');
        expect(pane.items[0].updatedAt).toBe(4);
        unregister();
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('does not let a lagging backend snapshot rewind an active reveal', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const thread = makeThread({ id: 'snapshot-during-reveal' });
        const initial = makeItem({
          id: 'text:0:0',
          threadId: thread.id,
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          summary: '',
          updatedAt: 1,
        });
        const pane = await buildPane(thread, [initial]);
        const full = 'first second third fourth fifth sixth seventh ';
        pane.applyItemDelta({
          threadId: thread.id,
          itemId: initial.id,
          kind: initial.kind,
          delta: full,
          updatedAt: 5,
        });
        clock.tickFrame(100);
        const beforeSnapshot = pane.items[0].summary;
        expect(beforeSnapshot.length).toBeGreaterThan(0);
        expect(beforeSnapshot.length).toBeLessThan(full.length);

        setBindingMock('ListThreadSliceAround', async () => ({
          items: [{ ...initial, summary: '', updatedAt: 2 }],
          oldestTurnIndex: 0,
          newestTurnIndex: 0,
          hasMore: false,
          hasMoreOlder: false,
          hasMoreNewer: false,
        }));
        await pane.refreshFromBackend();

        expect(pane.items[0].summary).toBe(beforeSnapshot);
        expect(pane.items[0].updatedAt).toBe(5);
        expect(pane.__itemSmootherCountForTest()).toBe(1);
        let previousLength = pane.items[0].summary.length;
        while (clock.pendingCount() > 0) {
          clock.tickFrame(100);
          expect(pane.items[0].summary.length).toBeGreaterThanOrEqual(previousLength);
          previousLength = pane.items[0].summary.length;
        }
        expect(pane.items[0].summary).toBe(full);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('never skips any frontier — it animates every char at the adaptive cap', async () => {
      // The prose counterpart of the test above; the guarantee is
      // kind-independent, so both are asserted. A queued successor waits
      // out the whole reveal, and the reveal stays inside the rate ceiling
      // throughout.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 0,
            summary: '',
            updatedAt: 2,
          }),
        );
        const text = 'word '.repeat(400); // 2000 chars
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: text,
          updatedAt: 3,
        });
        pane.upsertItem(
          makeItem({
            id: 'tool:0:1',
            threadId: 't',
            kind: 'tool_call',
            status: 'running',
            turnIndex: 0,
            itemIndex: 1,
            toolName: 'Bash',
            summary: 'Bash',
            updatedAt: 4,
          }),
        );
        expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });

        const summaryOf = () =>
          pane.items.find((i) => i.id === 'text:0:0')?.summary ?? '';
        let previousLength = summaryOf().length;
        expect(previousLength).toBe(0);
        let frames = 0;
        while (pane.revealBoundary !== null && frames < 900) {
          clock.tickFrame(16);
          frames++;
          const length = summaryOf().length;
          // Per-frame WORK bound: no frame ever dumps a chunk, and no
          // frame ever jumps a skipped middle.
          expect(length - previousLength).toBeLessThanOrEqual(
            MAX_ADVANCE_PER_TICK_CHARS,
          );
          previousLength = length;
        }
        expect(pane.revealBoundary).toBeNull();
        expect(summaryOf()).toBe(text);
        // Average rate over the whole drain stayed under the ceiling —
        // which also proves nothing was skipped.
        expect(frames * 16).toBeGreaterThanOrEqual(
          (text.length / MAX_ADAPTIVE_CHARS_PER_SEC) * 1000,
        );
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps isItemSmoothing true through the post-completion drain, false once caught up', async () => {
      // The wire settles status to 'completed' while the smoother is
      // still revealing. Render code derives its streaming mode from
      // `status === 'streaming' || isItemSmoothing`, so this signal must
      // hold through the drain tail and clear exactly at catch-up —
      // otherwise ChatMarkdown drops its volatile-tail markdown guards
      // while the text is still visibly growing.
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 0,
            summary: '',
            updatedAt: 2,
          }),
        );
        const text = 'word '.repeat(60); // 300 chars
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: text,
          updatedAt: 3,
        });
        pane.applyItemPatch({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          patch: { status: 'completed', summary: text, updatedAt: 4 },
        });

        const item = () => pane.items.find((i) => i.id === 'text:0:0');
        // Status settles immediately; the reveal (and the smoothing
        // signal) keeps draining.
        expect(item()?.status).toBe('completed');
        expect(pane.isItemSmoothing('text:0:0')).toBe(true);
        expect((item()?.summary ?? text).length).toBeLessThan(text.length);

        let frames = 0;
        while (pane.isItemSmoothing('text:0:0') && frames < 500) {
          clock.tickFrame(16);
          frames++;
        }
        expect(frames).toBeGreaterThan(1);
        expect(item()?.summary).toBe(text);
        expect(pane.isItemSmoothing('text:0:0')).toBe(false);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('keeps a terminal full-row upsert on the post-completion drain', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        const initial = makeItem({
          id: 'text:0:0',
          threadId: 't',
          kind: 'assistant_text',
          role: 'assistant',
          status: 'streaming',
          turnIndex: 0,
          itemIndex: 0,
          summary: '',
          updatedAt: 1,
        });
        pane.upsertItem(initial);
        const streamed = 'word '.repeat(120);
        const finalSuffix = 'terminal suffix remains readable ';
        pane.applyItemDelta({
          threadId: 't',
          itemId: initial.id,
          kind: initial.kind,
          delta: streamed,
          updatedAt: 2,
        });
        for (let frame = 0; frame < 4; frame++) clock.tickFrame(16);
        const visibleAtCompletion = pane.items[0].summary;
        expect(visibleAtCompletion.length).toBeGreaterThan(0);
        expect(visibleAtCompletion.length).toBeLessThan(streamed.length);

        pane.upsertItem({
          ...pane.items[0],
          status: 'completed',
          summary: streamed + finalSuffix,
          updatedAt: 3,
        });

        expect(pane.items[0].status).toBe('completed');
        expect(pane.items[0].summary).toBe(visibleAtCompletion);
        expect(pane.isItemSmoothing(initial.id)).toBe(true);

        let previousLength = visibleAtCompletion.length;
        let frames = 0;
        while (pane.isItemSmoothing(initial.id) && frames++ < 1_000) {
          clock.tickFrame(16);
          const length = pane.items[0].summary.length;
          expect(length).toBeGreaterThanOrEqual(previousLength);
          expect(length - previousLength).toBeLessThanOrEqual(
            MAX_ADVANCE_PER_TICK_CHARS,
          );
          previousLength = length;
        }
        expect(frames).toBeLessThan(1_000);
        expect(pane.items[0].summary).toBe(streamed + finalSuffix);
        expect(pane.isItemSmoothing(initial.id)).toBe(false);
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });

    it('settleTurn keeps the normal reveal cadence — no end-of-turn rush', async () => {
      const clock = new FakeSmoothingClock();
      __setSmoothingClockForTest(clock);
      try {
        const pane = await buildPane(makeThread({ id: 't' }));
        pane.upsertItem(
          makeItem({
            id: 'text:0:0',
            threadId: 't',
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            turnIndex: 0,
            itemIndex: 0,
            summary: '',
            updatedAt: 2,
          }),
        );
        // 2000-char backlog on a solo tail row. The historical
        // end-of-turn fast-drain rushed this at an elevated per-tick cap
        // (~3360 cps) — rushed motion the user read as jank.
        // Deliberately removed: the backlog drains at the same steady
        // cadence as live streaming (adaptive catch-up, ≤
        // MAX_ADAPTIVE_CHARS_PER_SEC).
        const text = 'word '.repeat(400);
        pane.applyItemDelta({
          threadId: 't',
          itemId: 'text:0:0',
          kind: 'assistant_text',
          delta: text,
          updatedAt: 3,
        });
        pane.settleTurn({
          turnId: 'turn-1',
          turnIndex: 0,
          startedAt: 1,
          completedAt: 2,
          stopReason: 'end_turn',
          assistantMessageId: 'text:0:0',
          tokenUsage: null,
          aborted: false,
          errorMessage: '',
        });
        // Inside the historical 800ms drain window the backlog must
        // still be mid-reveal (the rush would have finished it) —
        // advancing steadily, not snapped and not stalled.
        for (let i = 0; i < 60; i++) clock.tickFrame(16);
        const midSummary =
          pane.items.find((i) => i.id === 'text:0:0')?.summary ?? '';
        expect(midSummary.length).toBeGreaterThan(0);
        expect(midSummary.length).toBeLessThan(text.length);
        // At the steady cadence (~5 word-aligned chars/frame while the
        // lag is large, tapering below) the full 2000-char backlog
        // completes within a few hundred frames.
        for (let i = 0; i < 600; i++) clock.tickFrame(16);
        expect(pane.items.find((i) => i.id === 'text:0:0')?.summary).toBe(
          text,
        );
      } finally {
        __setSmoothingClockForTest(undefined);
      }
    });
  });
});
