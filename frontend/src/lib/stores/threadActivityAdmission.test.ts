import { beforeEach, expect, it } from 'vitest';
import { buildPane, makeItem, makeThread } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import { pass, registry } from '../../test/helpers/activityRuns';
import { getSettings } from './settings.svelte';
import { groupActivityRuns } from '../utils/activityRunGrouping';
import type { ThreadPane } from './thread.svelte';

beforeEach(installThreadPaneTestEnv);

function runs(pane: ThreadPane) {
  return groupActivityRuns(pane.items.map(item => ({ kind: 'leaf' as const, item })), {
    identity: pane.activityRuns, getItem: id => pane.getItemById(id),
    windowReachesTail: true, withheld: [],
  }).filter(node => node.kind === 'activity_run');
}

it.each(['assistant_text', 'user_text'] as const)('opens a live singleton arriving with %s in one batch', async kind => {
  getSettings().activityRunDefault = 'collapsed';
  const pane = await buildPane(makeThread());
  try {
    pane.applyProviderItemUpserts([
      makeItem({ id: 'tool', itemIndex: 1, kind: 'tool_call', toolName: 'Bash', status: 'completed' }),
      makeItem({ id: 'after', itemIndex: 2, kind, status: 'completed', summary: 'after activity' }),
    ]);
    expect(runs(pane)).toHaveLength(1);
    expect(runs(pane)[0].collapsed).toBe(false);
    const id = runs(pane)[0].runId;
    pane.activityRuns.releaseOpenedLive([id]);
    expect(runs(pane)[0].collapsed).toBe(true);
  } finally { pane.clear(); }
});

it('preserves manual collapse when more live work joins the run', async () => {
  getSettings().activityRunDefault = 'collapsed';
  const pane = await buildPane(makeThread());
  try {
    pane.applyProviderItemUpserts([makeItem({ id: 'one', itemIndex: 1, kind: 'tool_call' })]);
    pane.activityRuns.setCollapsed(runs(pane)[0].runId, true);
    pane.applyProviderItemUpserts([
      makeItem({ id: 'two', itemIndex: 2, kind: 'tool_call' }),
      makeItem({ id: 'answer', itemIndex: 3, kind: 'assistant_text', summary: 'done' }),
    ]);
    expect(runs(pane)[0].collapsed).toBe(true);
  } finally { pane.clear(); }
});

it('keeps loaded history and late historical inserts under the default', async () => {
  getSettings().activityRunDefault = 'collapsed';
  const pane = await buildPane(makeThread(), [
    makeItem({ id: 'old', itemIndex: 1, kind: 'tool_call' }),
    makeItem({ id: 'answer', itemIndex: 4, kind: 'assistant_text', summary: 'done' }),
  ]);
  try {
    expect(runs(pane)[0].collapsed).toBe(true);
    pane.applyProviderItemUpserts([makeItem({ id: 'late', itemIndex: 2, kind: 'tool_call' })]);
    expect(runs(pane)[0].collapsed).toBe(true);
  } finally { pane.clear(); }
});


it.each(['clear', 'replace'] as const)('forgets unrendered live admissions on %s', action => {
  const item = makeItem({ id: 'tool', threadId: 'thread-1', kind: 'tool_call' });
  let items = [item];
  const state = registry({ defaultCollapsed: true, threadId: () => 'thread-1', items: () => items });
  try {
    state.noteLiveAppend([item], undefined);
    if (action === 'clear') state.clear();
    else {
      items = [];
      state.noteWholesaleReplace();
    }
    items = [item];
    expect(pass(state, [['tool']])[0].collapsed).toBe(true);
    state.noteLiveAppend([item], undefined);
    expect(pass(state, [['tool']])[0].collapsed).toBe(false);
  } finally { state.clear(); }
});

it('only holds verified live arrivals and consumes an admission once', () => {
  const item = makeItem({ id: 'tool', threadId: 'thread-1', kind: 'tool_call' });
  let verified = false;
  const state = registry({ defaultCollapsed: true, threadId: () => 'thread-1',
    items: () => [item], windowVerified: () => verified });
  try {
    state.noteLiveAppend([item], undefined);
    verified = true;
    expect(pass(state, [['tool']])[0].collapsed).toBe(true);
    state.noteLiveAppend([item], undefined);
    state.noteLiveAppend([item], undefined);
    const [live] = pass(state, [['tool']]);
    expect(live.collapsed).toBe(false);
    state.releaseOpenedLive([live.runId]);
    expect(pass(state, [['tool']])[0].collapsed).toBe(true);
    verified = false;
    state.noteLiveAppend([item], undefined);
    expect(pass(state, [['tool']])[0].collapsed).toBe(true);
  } finally { state.clear(); }
});
