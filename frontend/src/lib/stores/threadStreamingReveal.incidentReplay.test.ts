// Replay of the 2026-08-29 22:58 truncated-final-answer incident
// (thread bc48c862, turn 4): the 1021-char final assistant text arrived
// as 8 fat deltas within 16ms, the completion patch landed 4ms later,
// the turn settled 20ms after that, and the visible row never grew past
// its first reveal tick for 39 seconds while ~150 subagent child rows
// kept streaming into the window. This suite replays the store-level
// event sequence byte-faithfully (wire log provider-events-2026-08-29)
// and asserts the reveal drain finishes.
import { describe, expect, it } from 'vitest';
import {
  __setSmoothingClockForTest,
  createThreadPane,
} from './thread.svelte';
import type { SmoothingClock } from '../markdown/smoothing/PerItemSmoother';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import {
  INCIDENT_TEXT_DELTAS,
  INCIDENT_TEXT_FULL,
  INCIDENT_THINK_DELTAS,
  INCIDENT_THINK_FULL,
} from './__incidentReplayFixture';

class FakeSmoothingClock implements SmoothingClock {
  private current = 0;
  private nextHandle = 1;
  private pending = new Map<number, () => void>();
  now(): number {
    return this.current;
  }
  schedule(cb: () => void): number {
    const h = this.nextHandle++;
    this.pending.set(h, cb);
    return h;
  }
  cancel(h: number): void {
    this.pending.delete(h);
  }
  tickFrame(ms: number): void {
    this.current += ms;
    const toFire = [...this.pending.values()];
    this.pending.clear();
    for (const cb of toFire) cb();
  }
  pendingCount(): number {
    return this.pending.size;
  }
}

const TID = 'thread-1';
const AGENT_1 = 'toolu_01SJkLxjhKvMGzVd2ezoBsyp';
const AGENT_2 = 'toolu_018hzxDEvcpDfH16ZM5f1zVb';

function textRow(overrides: Partial<Item>): Item {
  return makeItem({
    threadId: TID,
    turnIndex: 4,
    role: 'assistant',
    ...overrides,
  });
}

/** Turn-4 window as it stood at 02:58:12 (item indexes from SQLite). */
function seedTurnWindow(pane: ReturnType<typeof createThreadPane>): void {
  pane.upsertItems([
    textRow({ id: 'user:4', kind: 'user_text', role: 'user', itemIndex: 0, summary: 'q' }),
    textRow({ id: 'think:4:0', kind: 'thinking', itemIndex: 1, summary: 't0' }),
    textRow({ id: 'tool:4:0', kind: 'tool_call', toolName: 'Bash', itemIndex: 2, summary: 'ls' }),
    textRow({ id: 'tool:4:1', kind: 'tool_call', toolName: 'Bash', itemIndex: 3, status: 'errored', summary: 'x' }),
    textRow({ id: 'tool:4:2', kind: 'tool_call', toolName: 'Bash', itemIndex: 4, summary: 'ls' }),
    textRow({ id: 'think:4:1', kind: 'thinking', itemIndex: 5, summary: 't1' }),
    textRow({ id: AGENT_1, kind: 'tool_call', toolName: 'Agent', itemIndex: 6, status: 'running', summary: 'survey' }),
    textRow({ id: AGENT_2, kind: 'tool_call', toolName: 'Agent', itemIndex: 17, status: 'running', summary: 'upstream' }),
  ]);
}

interface ChildChurn {
  /** Fire whatever child wire activity is due at fake-time `t`. */
  pump(t: number): void;
}

/**
 * Continuous subagent child stream: each agent runs a repeating
 * upsert(streaming thinking child) → 3 deltas → settle-patch cycle,
 * offset from each other, exactly the shape that kept recomputeReveal
 * and evictSettledChildren firing every few hundred ms through the
 * incident window.
 */
interface ChurnFlags {
  upserts?: boolean;
  deltas?: boolean;
  settles?: boolean;
}

function createChildChurn(
  pane: ReturnType<typeof createThreadPane>,
  agents: readonly string[],
  flags: ChurnFlags = { upserts: true, deltas: true, settles: true },
): ChildChurn {
  const state = agents.map((anchor, i) => ({
    anchor,
    phase: 0,
    nextAt: 300 + i * 500,
    seq: 0,
    childIndex: 7 + i * 11,
  }));
  return {
    pump(t: number): void {
      for (const s of state) {
        while (t >= s.nextAt) {
          const id = `child:${s.anchor.slice(-4)}:${s.seq}`;
          if (s.phase === 0) {
            if (flags.upserts) {
              pane.applyProviderItemUpserts([
                textRow({
                  id,
                  kind: 'thinking',
                  itemIndex: s.childIndex,
                  parentId: s.anchor,
                  status: 'streaming',
                  summary: '',
                }),
              ]);
            }
          } else if (s.phase <= 3) {
            if (flags.upserts && flags.deltas) {
              pane.applyItemDelta({
                threadId: TID,
                itemId: id,
                kind: 'thinking',
                delta: `child reasoning burst ${s.seq}-${s.phase} `.repeat(6),
                updatedAt: t,
              });
            }
          } else {
            if (flags.upserts && flags.settles) {
              pane.applyItemPatch({
                threadId: TID,
                itemId: id,
                kind: 'thinking',
                patch: { status: 'completed', updatedAt: t },
              });
            }
            s.seq += 1;
          }
          s.phase = (s.phase + 1) % 5;
          s.nextAt += 220;
        }
      }
    },
  };
}

interface ReplayEvent {
  at: number;
  run: () => void;
}

/** Pump fake frames at 60Hz to `untilMs`, firing due events in order. */
function pumpTo(
  clock: FakeSmoothingClock,
  events: ReplayEvent[],
  churn: ChildChurn | null,
  untilMs: number,
): void {
  let queue = [...events].sort((a, b) => a.at - b.at);
  while (clock.now() < untilMs) {
    const next = clock.now() + 16;
    while (queue.length > 0 && queue[0].at <= next) {
      queue = queue.slice(0);
      const evt = queue.shift()!;
      evt.run();
    }
    churn?.pump(next);
    clock.tickFrame(16);
  }
}

function textDeltaEvents(
  pane: ReturnType<typeof createThreadPane>,
  base: number,
): ReplayEvent[] {
  // Real wire offsets (ms) of the 8 text deltas after content_block_start.
  const offsets = [0, 1, 1, 2, 2, 9, 12, 16];
  return INCIDENT_TEXT_DELTAS.map((delta, i) => ({
    at: base + offsets[i],
    run: () =>
      pane.applyItemDelta({
        threadId: TID,
        itemId: 'text:4:0',
        kind: 'assistant_text',
        delta,
        updatedAt: base + offsets[i],
      }),
  }));
}

describe('incident 2026-08-29 replay: fat-burst final text + instant settle', () => {
  function withClock(run: (clock: FakeSmoothingClock) => void): void {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    try {
      run(clock);
    } finally {
      __setSmoothingClockForTest(undefined);
    }
  }

  const summaryOf = (pane: ReturnType<typeof createThreadPane>, id: string) =>
    pane.items.find((item) => item.id === id)?.summary ?? '';

  it('stage 1: burst deltas + completion patch drain to the full text', () => {
    withClock((clock) => {
      const pane = createThreadPane();
      pane.applyProviderItemUpserts([
        textRow({ id: 'text:4:0', kind: 'assistant_text', itemIndex: 28, status: 'streaming', summary: '' }),
      ]);
      const events: ReplayEvent[] = [
        ...textDeltaEvents(pane, 100),
        {
          at: 120,
          run: () =>
            pane.applyItemPatch({
              threadId: TID,
              itemId: 'text:4:0',
              kind: 'assistant_text',
              patch: { status: 'completed', summary: INCIDENT_TEXT_FULL, updatedAt: 120 },
            }),
        },
      ];
      pumpTo(clock, events, null, 8_000);

      expect(summaryOf(pane, 'text:4:0')).toBe(INCIDENT_TEXT_FULL);
      expect(pane.isItemSmoothing('text:4:0')).toBe(false);
      expect(pane.revealBoundary).toBeNull();
    });
  });

  it('stage 2: preceding thinking block settling 1ms before the text', () => {
    withClock((clock) => {
      const pane = createThreadPane();
      seedTurnWindow(pane);
      const events: ReplayEvent[] = [
        {
          at: 3205,
          run: () => {
            pane.applyProviderItemUpserts([
              textRow({ id: 'think:4:2', kind: 'thinking', itemIndex: 24, status: 'streaming', summary: '' }),
            ]);
            pane.applyItemDelta({
              threadId: TID,
              itemId: 'think:4:2',
              kind: 'thinking',
              delta: INCIDENT_THINK_DELTAS[0],
              updatedAt: 3205,
            });
          },
        },
        {
          at: 3209,
          run: () =>
            pane.applyItemDelta({
              threadId: TID,
              itemId: 'think:4:2',
              kind: 'thinking',
              delta: INCIDENT_THINK_DELTAS[1],
              updatedAt: 3209,
            }),
        },
        {
          at: 3615,
          run: () =>
            pane.applyItemDelta({
              threadId: TID,
              itemId: 'think:4:2',
              kind: 'thinking',
              delta: INCIDENT_THINK_DELTAS[2],
              updatedAt: 3615,
            }),
        },
        {
          at: 6760,
          run: () =>
            pane.applyItemPatch({
              threadId: TID,
              itemId: 'think:4:2',
              kind: 'thinking',
              patch: { status: 'completed', summary: INCIDENT_THINK_FULL, updatedAt: 6760 },
            }),
        },
        {
          at: 6761,
          run: () =>
            pane.applyProviderItemUpserts([
              textRow({ id: 'text:4:0', kind: 'assistant_text', itemIndex: 28, status: 'streaming', summary: '' }),
            ]),
        },
        ...textDeltaEvents(pane, 6761),
        {
          at: 6781,
          run: () =>
            pane.applyItemPatch({
              threadId: TID,
              itemId: 'text:4:0',
              kind: 'assistant_text',
              patch: { status: 'completed', summary: INCIDENT_TEXT_FULL, updatedAt: 6781 },
            }),
        },
        {
          at: 6798,
          run: () =>
            pane.settleTurn({
              turnId: 'turn-4',
              turnIndex: 4,
              startedAt: 0,
              completedAt: 6798,
              stopReason: 'end_turn',
              assistantMessageId: 'msg_011CeYBawRKyYWALkGJ3AuDV',
              tokenUsage: null,
              aborted: false,
              errorMessage: '',
            }),
        },
      ];
      pumpTo(clock, events, null, 20_000);

      expect(summaryOf(pane, 'think:4:2')).toBe(INCIDENT_THINK_FULL);
      expect(summaryOf(pane, 'text:4:0')).toBe(INCIDENT_TEXT_FULL);
      expect(pane.isItemSmoothing('text:4:0')).toBe(false);
      expect(pane.revealBoundary).toBeNull();
    });
  });

  it('stage 3: full replay with continuous subagent child churn', () => {
    withClock((clock) => {
      const pane = createThreadPane();
      seedTurnWindow(pane);
      const churn = createChildChurn(pane, [AGENT_1, AGENT_2]);
      const events: ReplayEvent[] = [
        {
          at: 3205,
          run: () => {
            pane.applyProviderItemUpserts([
              textRow({ id: 'think:4:2', kind: 'thinking', itemIndex: 24, status: 'streaming', summary: '' }),
            ]);
            pane.applyItemDelta({
              threadId: TID,
              itemId: 'think:4:2',
              kind: 'thinking',
              delta: INCIDENT_THINK_DELTAS[0],
              updatedAt: 3205,
            });
          },
        },
        {
          at: 3209,
          run: () =>
            pane.applyItemDelta({
              threadId: TID,
              itemId: 'think:4:2',
              kind: 'thinking',
              delta: INCIDENT_THINK_DELTAS[1],
              updatedAt: 3209,
            }),
        },
        {
          at: 3615,
          run: () =>
            pane.applyItemDelta({
              threadId: TID,
              itemId: 'think:4:2',
              kind: 'thinking',
              delta: INCIDENT_THINK_DELTAS[2],
              updatedAt: 3615,
            }),
        },
        {
          at: 6760,
          run: () =>
            pane.applyItemPatch({
              threadId: TID,
              itemId: 'think:4:2',
              kind: 'thinking',
              patch: { status: 'completed', summary: INCIDENT_THINK_FULL, updatedAt: 6760 },
            }),
        },
        {
          at: 6761,
          run: () =>
            pane.applyProviderItemUpserts([
              textRow({ id: 'text:4:0', kind: 'assistant_text', itemIndex: 28, status: 'streaming', summary: '' }),
            ]),
        },
        ...textDeltaEvents(pane, 6761),
        {
          at: 6781,
          run: () =>
            pane.applyItemPatch({
              threadId: TID,
              itemId: 'text:4:0',
              kind: 'assistant_text',
              patch: { status: 'completed', summary: INCIDENT_TEXT_FULL, updatedAt: 6781 },
            }),
        },
        {
          at: 6798,
          run: () =>
            pane.settleTurn({
              turnId: 'turn-4',
              turnIndex: 4,
              startedAt: 0,
              completedAt: 6798,
              stopReason: 'end_turn',
              assistantMessageId: 'msg_011CeYBawRKyYWALkGJ3AuDV',
              tokenUsage: null,
              aborted: false,
              errorMessage: '',
            }),
        },
      ];
      pumpTo(clock, events, churn, 30_000);

      expect(summaryOf(pane, 'text:4:0')).toBe(INCIDENT_TEXT_FULL);
      expect(pane.isItemSmoothing('text:4:0')).toBe(false);
      expect(pane.revealBoundary).toBeNull();
    });
  });

  // The distilled root cause: a subagent child settling during the
  // post-terminal drain evicts its row into the fold, and the eviction's
  // wholesale commit passes every KEPT row back through
  // prepareItemReplacements — including the draining text row, whose
  // status is already terminal while its summary is still the smoother's
  // partial prefix. Disposing there strands the row at the partial text
  // forever (the completion patch's summary write was already skipped in
  // favor of the smoother). Delta-free churn isolates the eviction as
  // the killer: with child deltas the same replay also fails, but this
  // variant proves no child smoother is required.
  it('a fold eviction during the post-terminal drain keeps the drain alive', () => {
    withClock((clock) => {
      const pane = createThreadPane();
      seedTurnWindow(pane);
      const churn = createChildChurn(pane, [AGENT_1, AGENT_2], {
        upserts: true,
        deltas: false,
        settles: true,
      });
      const events: ReplayEvent[] = [
        {
          at: 100,
          run: () =>
            pane.applyProviderItemUpserts([
              textRow({ id: 'text:4:0', kind: 'assistant_text', itemIndex: 28, status: 'streaming', summary: '' }),
            ]),
        },
        ...textDeltaEvents(pane, 100),
        {
          at: 130,
          run: () =>
            pane.applyItemPatch({
              threadId: TID,
              itemId: 'text:4:0',
              kind: 'assistant_text',
              patch: { status: 'completed', summary: INCIDENT_TEXT_FULL, updatedAt: 130 },
            }),
        },
      ];
      pumpTo(clock, events, churn, 20_000);

      expect(summaryOf(pane, 'text:4:0')).toBe(INCIDENT_TEXT_FULL);
      expect(pane.isItemSmoothing('text:4:0')).toBe(false);
    });
  });

  it('stage 4: terminal upsert echo instead of a patch, mid-drain', () => {
    withClock((clock) => {
      const pane = createThreadPane();
      seedTurnWindow(pane);
      const events: ReplayEvent[] = [
        {
          at: 100,
          run: () =>
            pane.applyProviderItemUpserts([
              textRow({ id: 'text:4:0', kind: 'assistant_text', itemIndex: 28, status: 'streaming', summary: '' }),
            ]),
        },
        ...textDeltaEvents(pane, 100),
        {
          at: 130,
          run: () =>
            pane.applyProviderItemUpserts([
              textRow({
                id: 'text:4:0',
                kind: 'assistant_text',
                itemIndex: 28,
                status: 'completed',
                summary: INCIDENT_TEXT_FULL,
                updatedAt: 130,
              }),
            ]),
        },
      ];
      pumpTo(clock, events, null, 10_000);

      expect(summaryOf(pane, 'text:4:0')).toBe(INCIDENT_TEXT_FULL);
      expect(pane.isItemSmoothing('text:4:0')).toBe(false);
      expect(pane.revealBoundary).toBeNull();
    });
  });
});
