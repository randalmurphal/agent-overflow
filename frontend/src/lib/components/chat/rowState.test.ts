import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import type { Item } from '../../types/models';
import { indicatorStateForItem } from './rowState';

// A background launch row's dots turn off once, when the store settles the
// launch (docs/specs/agent-visibility.md#immutable-agent-history). The input
// is the row's own stored `live_background_active` bit, nothing else.
describe('indicatorStateForItem on a background launch row', () => {
  const launch = (meta?: Record<string, unknown>, fields: Partial<Item> = {}): Item => makeItem({
    id: 'bg',
    kind: 'tool_call',
    toolName: 'Agent',
    status: 'running',
    isBackground: true,
    meta: meta ? JSON.stringify(meta) : undefined,
    ...fields,
  });

  it('shows the dots until the bit settles the launch', () => {
    expect(indicatorStateForItem(launch())).toBe('backgrounded');
    expect(indicatorStateForItem(launch({ task_id: 't1' }))).toBe('backgrounded');
    expect(indicatorStateForItem(launch({ live_background_active: true }))).toBe('backgrounded');
    expect(indicatorStateForItem(launch({ live_background_active: false }))).toBeNull();
    expect(indicatorStateForItem(launch({ live_background_active: false }, { status: 'streaming' }))).toBeNull();
  });

  it('reads the row meta, not the payload meta or the options', () => {
    const settledPayload = launch(undefined, { payloadMeta: JSON.stringify({ live_background_active: false }) });
    expect(indicatorStateForItem(settledPayload)).toBe('backgrounded');
    expect(indicatorStateForItem(launch(), { payloadMeta: { live_background_active: false } })).toBe('backgrounded');
  });

  it('keeps the dots through a parked stop, whose sibling leaves the bit set', () => {
    // The store's settle triggers skip a `parked` sibling, so a parked
    // agent's launch still carries the bit; the parked sibling is its own row.
    expect(indicatorStateForItem(launch({ live_background_active: true, task_id: 't1' }))).toBe('backgrounded');
    const parked = makeItem({
      id: 'complete:bg:parked:u1', kind: 'tool_completion', status: 'parked', isBackground: true, completionOf: 'bg',
    });
    expect(indicatorStateForItem(parked)).toBe('parked');
  });

  it('leaves a Codex spawn row as it was, whatever the bit says', () => {
    const spawn = (active: boolean) => makeItem({
      id: 'spawn', kind: 'tool_call', toolName: 'collab_agent', status: 'completed', isBackground: true,
      meta: JSON.stringify({ live_background_active: active, input: { tool: 'spawn_agent' } }),
    });
    expect(indicatorStateForItem(spawn(true))).toBeNull();
    expect(indicatorStateForItem(spawn(false))).toBeNull();
  });

  it('never reads the bit on a foreground row', () => {
    const running = makeItem({ kind: 'tool_call', status: 'running', meta: JSON.stringify({ live_background_active: false }) });
    expect(indicatorStateForItem(running)).toBe('running');
  });
});
