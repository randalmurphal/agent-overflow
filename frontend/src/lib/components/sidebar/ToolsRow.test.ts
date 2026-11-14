import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { WorkItem } from '../../types/workflow';
import ToolsRow from './ToolsRow.svelte';
import { hydrateWorkflowAttention, resetWorkflowRunsForTest } from '../../stores/workflowRuns.svelte';
import { resetWorkflowsOverlayForTest } from '../../stores/workflowsOverlay.svelte';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import { buttonClasses } from '../primitives/buttonClasses';
import {
  decodeToolDragPayload,
  endToolPaneDrag,
  threadDragPayloadForEvent,
  toolDragPayloadForEvent,
  TOOL_PANE_DRAG_MIME,
} from '../../utils/threadDragPayload';
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

function toolDataTransfer(): DataTransfer {
  const values = new Map<string, string>();
  return {
    effectAllowed: 'none',
    dropEffect: 'none',
    setData: (type: string, value: string) => { values.set(type, value); },
    getData: (type: string) => values.get(type) ?? '',
    setDragImage: () => {},
    get types() { return Array.from(values.keys()); },
  } as unknown as DataTransfer;
}

function resetAll(): void {
  resetBindingMocks();
  resetAppStorageForTest();
  resetWorkflowsOverlayForTest();
  resetWorkflowRunsForTest();
  endToolPaneDrag();
  setCompactLayoutForTest(false);
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
    const row = view.getByTestId('sidebar-tools-row');
    const tools = row.querySelectorAll<HTMLElement>('[role="button"]');
    expect([...tools].map((tool) => tool.dataset.testid)).toEqual([
      'sidebar-tool-pulls',
      'sidebar-tool-workflows',
    ]);
    // No native button and nothing interactive inside: Firefox starts no
    // drag from a <button>, and a nested control would steal the press.
    expect(row.querySelector('button, a, input, [tabindex]:not([role="button"])')).toBeNull();
    for (const tool of tools) {
      expect(tool.tagName).not.toBe('BUTTON');
      expect(tool).toHaveAttribute('tabindex', '0');
      expect(tool.querySelector('[role="button"], [tabindex]')).toBeNull();
    }
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

  it('shows no count on Pull requests, in the badge or the hover text', async () => {
    await hydrate([item('gate', 'needs-human', { reason: 'gate' })]);
    noteToolUsed('workflows');
    const view = render(ToolsRow);
    expect(view.queryByTestId('sidebar-tool-pulls-badge')).not.toBeInTheDocument();
    expect(view.getByTestId('sidebar-tool-pulls')).toHaveAttribute('title', 'Pull requests');
    expect(view.getByTestId('sidebar-tool-pulls')).toHaveAttribute('aria-label', 'Pull requests');
    expect(view.getByTestId('sidebar-tool-pulls').textContent?.trim()).toBe('');
  });

  it('drags a tool as its own payload, which no thread target reads', async () => {
    await hydrate([]);
    const view = render(ToolsRow);
    const button = view.getByTestId('sidebar-tool-workflows');
    expect(button).toHaveAttribute('draggable', 'true');
    const dataTransfer = toolDataTransfer();
    await fireEvent.dragStart(button, { dataTransfer });
    expect(dataTransfer.types).toEqual([TOOL_PANE_DRAG_MIME]);
    expect(decodeToolDragPayload(dataTransfer.getData(TOOL_PANE_DRAG_MIME))).toEqual({ kind: 'workflows' });
    // dragover hands back nothing: the in-flight record answers until dragend.
    const silent = { dataTransfer: { types: [TOOL_PANE_DRAG_MIME], getData: () => '' } } as unknown as DragEvent;
    expect(toolDragPayloadForEvent(silent)).toEqual({ kind: 'workflows' });
    expect(threadDragPayloadForEvent(silent)).toBeNull();
    await fireEvent.dragEnd(button, { dataTransfer });
    expect(toolDragPayloadForEvent(silent)).toBeNull();
    // A drag opens nothing by itself.
    expect(layoutIds()).toEqual(['main']);
  });

  it('starts the drag from the icon and the label as from the control itself', async () => {
    await hydrate([]);
    const view = render(ToolsRow);
    const pulls = view.getByTestId('sidebar-tool-pulls');
    for (const part of [pulls.firstElementChild as HTMLElement, view.getByTestId('sidebar-tool-label')]) {
      expect(part).not.toBe(pulls);
      endToolPaneDrag();
      const dataTransfer = toolDataTransfer();
      await fireEvent.dragStart(part, { dataTransfer });
      expect(decodeToolDragPayload(dataTransfer.getData(TOOL_PANE_DRAG_MIME))).toEqual({ kind: 'pulls' });
    }
  });

  it('opens on Enter keydown and Space keyup like a button, and in a new pane with ctrl or cmd', async () => {
    await hydrate([]);
    const view = render(ToolsRow);
    const workflows = view.getByTestId('sidebar-tool-workflows');
    const pulls = view.getByTestId('sidebar-tool-pulls');
    expect(workflows).toHaveAttribute('aria-pressed', 'false');
    workflows.focus();
    await fireEvent.keyDown(workflows, { key: 'Enter' });
    expect(layoutIds()).toEqual(['tool-workflows']);
    expect(workflows).toHaveAttribute('aria-pressed', 'true');
    expect(workflows.className).toContain('ring-inset');

    pulls.focus();
    await fireEvent.keyDown(pulls, { key: ' ' });
    expect(layoutIds()).toEqual(['tool-workflows']);
    await fireEvent.keyUp(pulls, { key: ' ' });
    expect(layoutIds()).toEqual(['tool-pulls']);
    expect(workflows).toHaveAttribute('aria-pressed', 'false');

    workflows.focus();
    await fireEvent.keyDown(workflows, { key: 'Enter', ctrlKey: true });
    expect(layoutIds()).toEqual(['tool-pulls', 'tool-workflows']);
    // Other keys do nothing.
    await fireEvent.keyDown(pulls, { key: 'a' });
    expect(layoutIds()).toEqual(['tool-pulls', 'tool-workflows']);
  });

  it('opens once for a held Enter or Space, and not at all when focus leaves a held Space', async () => {
    await hydrate([]);
    const view = render(ToolsRow);
    const workflows = view.getByTestId('sidebar-tool-workflows');
    const pulls = view.getByTestId('sidebar-tool-pulls');
    // An open of a tool already open focuses it, so a repeat that opened
    // would move focus off the pane focused after the first.
    workflows.focus();
    await fireEvent.keyDown(workflows, { key: 'Enter', ctrlKey: true });
    expect(layoutIds()).toEqual(['main', 'tool-workflows']);
    focusPane('main');
    await fireEvent.keyDown(workflows, { key: 'Enter', ctrlKey: true, repeat: true });
    await fireEvent.keyDown(workflows, { key: 'Enter', ctrlKey: true, repeat: true });
    // A repeat opened nothing, so focus stayed where it was put.
    expect(getFocusedPaneId()).toBe('main');

    // Space held then Tab away: nothing opens.
    pulls.focus();
    await fireEvent.keyDown(pulls, { key: ' ' });
    await fireEvent.keyDown(pulls, { key: ' ', repeat: true });
    workflows.focus();
    await fireEvent.keyUp(pulls, { key: ' ' });
    expect(isToolOpen('pulls')).toBe(false);

    // Space held with repeats opens once, on release.
    pulls.focus();
    await fireEvent.keyDown(pulls, { key: ' ', ctrlKey: true });
    await fireEvent.keyDown(pulls, { key: ' ', ctrlKey: true, repeat: true });
    await fireEvent.keyDown(pulls, { key: ' ', ctrlKey: true, repeat: true });
    expect(isToolOpen('pulls')).toBe(false);
    // New pane right of the focused main pane.
    await fireEvent.keyUp(pulls, { key: ' ', ctrlKey: true });
    expect(layoutIds()).toEqual(['main', 'tool-pulls', 'tool-workflows']);
    await fireEvent.keyUp(pulls, { key: ' ', ctrlKey: true });
    expect(layoutIds()).toEqual(['main', 'tool-pulls', 'tool-workflows']);
  });

  it('wears the Button primitive\'s ghost look, focus ring and compact hit target', async () => {
    await hydrate([]);
    const view = render(ToolsRow);
    const classes = view.getByTestId('sidebar-tool-pulls').className.split(' ');
    for (const cls of buttonClasses({ variant: 'ghost', size: 'sm' }).split(' ').filter(Boolean)) expect(classes).toContain(cls);
    expect(classes).toEqual(expect.arrayContaining(['compact:h-9', 'focus-visible:ring-2', 'compact:select-none']));
  });

  it('is no drag source on compact', async () => {
    await hydrate([]);
    setCompactLayoutForTest(true);
    const view = render(ToolsRow);
    for (const id of ['sidebar-tool-pulls', 'sidebar-tool-workflows']) {
      expect(view.getByTestId(id)).not.toHaveAttribute('draggable');
    }
    const dataTransfer = toolDataTransfer();
    await fireEvent.dragStart(view.getByTestId('sidebar-tool-pulls'), { dataTransfer });
    expect(dataTransfer.types).toEqual([]);
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
