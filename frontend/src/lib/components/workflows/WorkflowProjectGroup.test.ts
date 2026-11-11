// A project group on Workflows home: Recent is collapsed behind its toggle
// with its count, and shown open while the list find narrows the rows, so
// a matching resolved run is never hidden behind it. The reader's expansion
// belongs to the project, not the mounted group: a group the filter removes
// and restores is still expanded.

import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, describe, expect, it } from 'vitest';
import WorkflowProjectGroup from './WorkflowProjectGroup.svelte';
import type { WorkflowHomeGroup } from '../../stores/workflowData';
import type { WorkItem } from '../../types/workflow';
import { resetWorkflowsHomeForTest } from '../../stores/workflowsHome.svelte';

const MERGED = '{"action":"merged","policy":"manual","at":1}';

function group(projectId = 'p1'): WorkflowHomeGroup {
  const run = (id: string, goal: string) => ({ id, goal, workflowId: 'port', state: 'done', disposition: MERGED, createdAt: 1, endedAt: 2, projectId }) as WorkItem;
  return { projectId, projectName: 'Widgets', attention: [], running: [], recent: [run('r1', 'Port the parser'), run('r2', 'Port the lexer')], definitions: [] };
}

afterEach(() => resetWorkflowsHomeForTest());

describe('<WorkflowProjectGroup> Recent', () => {
  it('is collapsed with its count and opens on its toggle', async () => {
    const view = render(WorkflowProjectGroup, { group: group() });
    const toggle = view.getByTestId('workflow-recent-toggle');
    expect(toggle).toHaveTextContent('Recent · 2');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(view.queryByTestId('workflow-recent-list')).toBeNull();
    await fireEvent.click(toggle);
    expect(view.getAllByTestId('workflow-run-row')).toHaveLength(2);
  });

  it('shows its rows open while the list find filters', () => {
    const view = render(WorkflowProjectGroup, { group: group(), filtering: true });
    const toggle = view.getByTestId('workflow-recent-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(toggle).toBeDisabled();
    expect(view.getAllByTestId('workflow-run-row')).toHaveLength(2);
  });

  it('stays expanded when its group is removed and restored, and closes on a second toggle', async () => {
    const first = render(WorkflowProjectGroup, { group: group() });
    await fireEvent.click(first.getByTestId('workflow-recent-toggle'));
    expect(first.getByTestId('workflow-recent-list')).toBeInTheDocument();
    first.unmount();

    const other = render(WorkflowProjectGroup, { group: group('p2') });
    expect(other.queryByTestId('workflow-recent-list')).toBeNull();
    other.unmount();

    const restored = render(WorkflowProjectGroup, { group: group() });
    const toggle = restored.getByTestId('workflow-recent-toggle');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    expect(restored.getAllByTestId('workflow-run-row')).toHaveLength(2);
    await fireEvent.click(toggle);
    expect(restored.queryByTestId('workflow-recent-list')).toBeNull();
  });
});
