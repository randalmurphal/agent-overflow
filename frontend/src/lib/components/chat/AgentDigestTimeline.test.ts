// The digest regroups its scope only when the scope's structure changes:
// settled content and patches reach the mounted rows through their boxes.
import { cleanup, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import AgentDigestTimeline from './AgentDigestTimeline.svelte';
import { groupItemsBySubagent } from '../../utils/subagentGrouping';
import { installTimelineScopeCapability, installPaneMocks, makeItem, makeThread } from '../../../test/helpers/chat';
import { createThreadPane } from '../../stores/thread.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../stores/panes.svelte';
import { applyItemStreamEvent, flushItemEventQueue } from '../../stores/eventsItemStream';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { makeSettings } from '../../../test/helpers/settings';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import type { Item } from '../../types/models';

vi.mock('../../utils/subagentGrouping', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../utils/subagentGrouping')>();
  return { ...actual, groupItemsBySubagent: vi.fn(actual.groupItemsBySubagent) };
});

const threadId = 'digest-thread';
const launch = makeItem({ id: 'launch', threadId, itemIndex: 0, kind: 'tool_call', toolName: 'Agent', status: 'running', isBackground: true });
const tool = (id: string, itemIndex: number, extra: Partial<Item> = {}) => makeItem({
  id, threadId, parentId: launch.id, itemIndex, kind: 'tool_call', toolName: 'Bash', status: 'running', summary: `${id} running`, ...extra,
});
const push = (item: Item) => { applyItemStreamEvent({ action: 'upsert', threadId, item }); flushItemEventQueue(); };

beforeEach(async () => {
  installTimelineScopeCapability();
  resetBindingMocks();
  resetPanesForTest();
  setBindingMock('GetSettings', async () => makeSettings({ activityRunDefault: 'expanded' }));
  await loadSettings();
});
afterEach(() => { cleanup(); resetPanesForTest(); });

it('regroups on structural changes only', async () => {
  const rows = [tool('t1', 1), tool('t2', 2), tool('t3', 3)];
  installPaneMocks([launch, ...rows]);
  const pane = createThreadPane({ paneId: 'main' });
  registerPaneForTest('main', pane);
  await pane.switchThread(makeThread({ id: threadId }));
  const view = render(AgentDigestTimeline, { props: { pane, scopeId: launch.id, id: 'digest', viewKey: 'tray', live: true } });
  const body = view.getByTestId('subagent-group-body');
  await waitFor(() => expect(body.querySelectorAll('[data-item-id]')).toHaveLength(3));
  const grouping = vi.mocked(groupItemsBySubagent);
  grouping.mockClear();

  for (const [index, row] of rows.entries()) {
    push({ ...row, status: 'completed', summary: `${row.id} done`, updatedAt: row.updatedAt + 1 });
    applyItemStreamEvent({ action: 'patch', threadId, itemId: row.id, kind: row.kind, patch: { summary: `${row.id} patched`, rev: index + 10 } });
    flushItemEventQueue();
  }
  await tick();
  expect(grouping).not.toHaveBeenCalled();
  await waitFor(() => expect(body.querySelector('[data-item-id="t3"]')?.textContent).toContain('t3 patched'));

  push(tool('t4', 4));
  await waitFor(() => expect(body.querySelectorAll('[data-item-id]')).toHaveLength(4));
  expect(grouping).toHaveBeenCalled();
});
