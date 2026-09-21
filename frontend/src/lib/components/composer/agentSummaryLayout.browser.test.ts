import { afterEach, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import '../../../app.css';
import { makeItem } from '../../../test/helpers/chat';
import { raf } from '../../../test/helpers/browserFrames';
import { applySubagentProgress, resetForTest } from '../../stores/subagentProgress.svelte';
import type { TrayTask } from '../../utils/backgroundTray';
import type { ThreadPane } from '../../stores/thread.svelte';
import BackgroundTaskTrayRow from './BackgroundTaskTrayRow.svelte';
import SubagentGroupTestHarness from '../chat/SubagentGroupTestHarness.svelte';

const activity = 'Searching for Reuters March coverage and checking the original sources';
afterEach(resetForTest);

function host(): HTMLDivElement {
  const target = document.createElement('div');
  target.style.width = '800px';
  document.body.append(target);
  return target;
}

function expectInside(element: HTMLElement, container: HTMLElement): void {
  const box = element.getBoundingClientRect();
  const outer = container.getBoundingClientRect();
  expect(box.width, element.dataset.testid).toBeGreaterThan(0);
  expect(box.left, element.dataset.testid).toBeGreaterThanOrEqual(outer.left);
  expect(box.right, element.dataset.testid).toBeLessThanOrEqual(outer.right + 1);
}

function agent(provider: 'claude' | 'codex') {
  const launch = makeItem({
    id: 'agent-launch', threadId: 'thread-layout', kind: 'tool_call',
    toolName: provider === 'claude' ? 'Agent' : 'collab_agent', status: 'running',
    payloadMeta: JSON.stringify({ input: { subagent_type: 'Research original financial sources', description: activity } }),
    meta: JSON.stringify({ input: { tool: 'spawn_agent', receiverThreadIds: ['child'], newAgentNickname: 'Research original financial sources', newAgentRole: 'explorer', prompt: activity } }),
  });
  applySubagentProgress({ threadId: launch.threadId, itemId: launch.id, updatedAt: 1,
    progress: { toolUses: 66, totalTokens: 117_500, activity } });
  return launch;
}

for (const provider of ['claude', 'codex'] as const) {
  for (const compact of [false, true]) {
    it(`${provider} tray fits long labels and controls, compact=${compact}`, async () => {
      const launch = agent(provider);
      const task: TrayTask = { rowId: launch.id, anchor: launch, launch, completion: null, status: 'running', elapsedMs: 320_000, depth: 0 };
      const target = host();
      target.classList.toggle('layout-compact', compact);
      const onOpenPane = vi.fn();
      const onToggleExpanded = vi.fn();
      const onStop = vi.fn();
      const pane = { paneId: 'layout' } as unknown as ThreadPane;
      const view = render(BackgroundTaskTrayRow, { target, props: { task, provider, stopTarget: 'stop-agent', isStopping: false, onOpenPane, onStop, pane, onToggleExpanded } });
      try {
        const prefix = provider === 'claude' ? 'agent-row' : 'collab-tool-row';
        const row = view.getByTestId('background-task-tray-row');
        const name = view.getByTestId(`${prefix}-toggle-body-slot`);
        const tools = view.getByTestId('background-task-tray-row-tools');
        const tokens = view.getByTestId('background-task-tray-row-tokens');
        const preview = view.getByTestId('background-task-tray-row-activity');
        const stop = view.getByTestId('background-task-tray-row-stop');
        const open = view.getByTestId('background-task-tray-row-open');
        for (const width of [800, 560, 412, 320]) {
          target.style.width = `${width}px`;
          for (const depth of [0, 2, 6]) {
            await view.rerender({ task: { ...task, depth } });
            for (const isStopping of [false, true]) {
              await view.rerender({ isStopping });
              await raf();
              for (const element of [name, tools, tokens, preview, stop, open]) expectInside(element, row);
              expect(name.getBoundingClientRect().right).toBeLessThanOrEqual(open.getBoundingClientRect().left);
              expect(open.getBoundingClientRect().right).toBeLessThanOrEqual(stop.getBoundingClientRect().left);
              expect(Math.abs(preview.getBoundingClientRect().left - name.getBoundingClientRect().left)).toBeLessThan(1);
              expect(preview.textContent?.trim()).toBe(activity);
              if (row.clientWidth < 576) {
                expect(tools.getBoundingClientRect().top).toBeGreaterThanOrEqual(name.getBoundingClientRect().bottom);
                expect(Math.abs(tools.getBoundingClientRect().left - name.getBoundingClientRect().left)).toBeLessThan(1);
              }
              expect(tools.getBoundingClientRect().right).toBeLessThan(tokens.getBoundingClientRect().left);
              expect(preview.getBoundingClientRect().top).toBeGreaterThanOrEqual(tokens.getBoundingClientRect().bottom);
            }
          }
        }
        await view.rerender({ task, isStopping: false });
        await fireEvent.click(view.getByTestId(`${prefix}-toggle`));
        expect(onToggleExpanded).toHaveBeenCalledExactlyOnceWith(task);
        expect(onOpenPane).not.toHaveBeenCalled();
        await fireEvent.click(open);
        expect(onOpenPane).toHaveBeenCalledExactlyOnceWith(task);
        await fireEvent.click(stop);
        expect(onStop).toHaveBeenCalledExactlyOnceWith(task.rowId, 'stop-agent');
        expect(onOpenPane).toHaveBeenCalledTimes(1);
        await view.rerender({ task: { ...task, status: 'completed' }, stopTarget: null });
        expect(view.queryByTestId('background-task-tray-row-activity')).toBeNull();
        expect(view.queryByTestId('background-task-tray-row-stop')).toBeNull();
      } finally {
        view.unmount();
        target.remove();
      }
    });
  }
}

it('inline agent metrics stay visible and previews align at narrow and wide widths', async () => {
  const parent = agent('claude');
  const target = host();
  const view = render(SubagentGroupTestHarness, { target, props: { group: {
    kind: 'group', parent, anchor: parent, groupKey: parent.id, children: [],
    descendantCount: 180, loadedDescendantCount: 0, latestChildSummary: '',
  } } });
  try {
    for (const compact of [false, true]) {
      target.classList.toggle('layout-compact', compact);
      for (const width of [800, 412, 320]) {
        target.style.width = `${width}px`;
        await raf();
        const card = view.getByTestId('subagent-group');
        const name = view.getByTestId('subagent-group-label');
        const preview = view.getByTestId('subagent-group-preview');
        for (const id of ['label', 'tools', 'tokens', 'count', 'preview', 'duration']) {
          expectInside(view.getByTestId(`subagent-group-${id}`), card);
        }
        expect(Math.abs(name.getBoundingClientRect().left - preview.getBoundingClientRect().left)).toBeLessThan(1);
        if (width < 576) expect(view.getByTestId('subagent-group-tools').getBoundingClientRect().top).toBeGreaterThanOrEqual(name.getBoundingClientRect().bottom);
      }
    }
    await fireEvent.click(view.getByTestId('subagent-group-preview'));
    expect(view.getByTestId('subagent-group-body')).toBeInTheDocument();
  } finally {
    view.unmount();
    target.remove();
  }
});
