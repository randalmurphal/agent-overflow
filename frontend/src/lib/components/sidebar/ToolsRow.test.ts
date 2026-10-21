import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { WorkItem } from '../../types/workflow';
import ToolsRow from './ToolsRow.svelte';
import { hydrateWorkflowAttention, resetWorkflowRunsForTest } from '../../stores/workflowRuns.svelte';
import { resetWorkflowsOverlayForTest } from '../../stores/workflowsOverlay.svelte';
import { resetPullsAttentionForTest, setPullsNeedsYouCount } from '../../stores/pullsAttention.svelte';
import {
  isToolOpen,
  noteToolUsed,
  resetToolPanesForTest,
} from '../../stores/toolPanes.svelte';
import { createPane, focusPane, getFocusedPaneId, resetPanesForTest } from '../../stores/panes.svelte';
import {
  getPaneLayoutItems,
  resetPaneLayoutForTest,
  setPaneLayoutItemsForTest,
} from '../../stores/paneLayout.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

function item(id: string, state: string, over: Partial<WorkItem> = {}): WorkItem {
  return { id, projectId: 'p', state, createdAt: 1, ...over } as WorkItem;
}

const MERGED = '{"action":"merged","policy":"manual","at":1}';

async function hydrate(items: WorkItem[]): Promise<void> {
  setBindingMock('WorkflowListUnresolvedItems', async () => items);
  await hydrateWorkflowAttention();
}

function layoutIds(): string[] {
  return getPaneLayoutItems().map((entry) => entry.paneId);
}

function resetAll(): void {
  resetBindingMocks();
  resetAppStorageForTest();
  resetWorkflowsOverlayForTest();
  resetWorkflowRunsForTest();
  resetPullsAttentionForTest();
  resetToolPanesForTest();
  resetPanesForTest();
  resetPaneLayoutForTest();
}

describe('ToolsRow', () => {
  beforeEach(() => {
    resetAll();
    setPaneLayoutItemsForTest([{ id: 'main', paneId: 'main', kind: 'thread', widthPx: 560 }]);
    createPane('main');
    focusPane('main');
  });

  afterEach(resetAll);

  it('lists Pull requests then Workflows, labelling only the active tool', async () => {
    await hydrate([]);
    const view = render(ToolsRow);
    const buttons = view.getByTestId('sidebar-tools-row').querySelectorAll('button');
    expect([...buttons].map((button) => button.dataset.testid)).toEqual([
      'sidebar-tool-pulls',
      'sidebar-tool-workflows',
    ]);
    // Nothing used yet: the first tool is the labelled one.
    expect(view.getByTestId('sidebar-tool-pulls')).toHaveTextContent('Pull requests');
    expect(view.getByTestId('sidebar-tool-workflows')).not.toHaveTextContent('Workflows');
    expect(view.getByTestId('sidebar-tool-workflows')).toHaveAttribute('title', 'Workflows');
    expect(view.getByTestId('sidebar-tool-workflows')).toHaveAttribute('aria-label', 'Workflows');
  });

  it('labels the tool used last, and follows a focused tool pane over it', async () => {
    await hydrate([]);
    noteToolUsed('workflows');
    const view = render(ToolsRow);
    expect(view.getByTestId('sidebar-tool-workflows')).toHaveTextContent('Workflows');
    expect(view.getByTestId('sidebar-tool-pulls')).not.toHaveTextContent('Pull requests');

    await fireEvent.click(view.getByTestId('sidebar-tool-pulls'));
    expect(getFocusedPaneId()).toBe('tool-pulls');
    expect(view.getByTestId('sidebar-tool-pulls')).toHaveTextContent('Pull requests');
    expect(view.getAllByTestId('sidebar-tool-label')).toHaveLength(1);
  });

  it('shows no badge at all when nothing needs you', async () => {
    await hydrate([item('a', 'running'), item('b', 'done')]);
    const view = render(ToolsRow);
    expect(view.queryByTestId('sidebar-tool-workflows-badge')).not.toBeInTheDocument();
    expect(view.queryByTestId('sidebar-tool-pulls-badge')).not.toBeInTheDocument();
  });

  it('counts workflow runs a human must unblock in amber, with the count in the hover text', async () => {
    await hydrate([
      item('gate', 'needs-human', { reason: 'gate' }),
      item('failed', 'failed'),
      item('awaiting-disposition', 'done'),
      item('resolved', 'needs-human', { disposition: MERGED }),
      item('child', 'needs-human', { parentItemId: 'gate' }),
    ]);
    const view = render(ToolsRow);
    const badge = view.getByTestId('sidebar-tool-workflows-badge');
    expect(badge).toHaveTextContent('2');
    expect(badge.className).toContain('text-warning');
    expect(view.getByTestId('sidebar-tool-workflows')).toHaveAttribute('title', 'Workflows · 2 need you');
  });

  it('shows the pulls count in accent, and hides it again at zero', async () => {
    await hydrate([]);
    setPullsNeedsYouCount(1);
    const view = render(ToolsRow);
    const badge = view.getByTestId('sidebar-tool-pulls-badge');
    expect(badge).toHaveTextContent('1');
    expect(badge.className).toContain('text-accent');
    expect(view.getByTestId('sidebar-tool-pulls')).toHaveAttribute('title', 'Pull requests · 1 needs you');

    setPullsNeedsYouCount(0);
    await Promise.resolve();
    expect(view.queryByTestId('sidebar-tool-pulls-badge')).not.toBeInTheDocument();
  });

  it('opens a tool in place of the focused pane on a plain click', async () => {
    await hydrate([]);
    const view = render(ToolsRow);
    await fireEvent.click(view.getByTestId('sidebar-tool-workflows'));
    expect(layoutIds()).toEqual(['tool-workflows']);
    expect(getFocusedPaneId()).toBe('tool-workflows');
    expect(view.getByTestId('sidebar-tool-workflows')).toHaveAttribute('aria-pressed', 'true');
  });

  it('opens a tool in a new pane beside the focused one on ctrl or cmd click', async () => {
    await hydrate([]);
    const view = render(ToolsRow);
    await fireEvent.click(view.getByTestId('sidebar-tool-workflows'), { ctrlKey: true });
    expect(layoutIds()).toEqual(['main', 'tool-workflows']);
    await fireEvent.click(view.getByTestId('sidebar-tool-pulls'), { metaKey: true });
    expect(layoutIds()).toEqual(['main', 'tool-workflows', 'tool-pulls']);
    expect(isToolOpen('pulls')).toBe(true);
  });
});
