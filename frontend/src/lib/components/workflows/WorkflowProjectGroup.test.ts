// A project group on Workflows home: Recent is collapsed behind its toggle
// with its count, and shown open while the list find narrows the rows, so
// a matching resolved run is never hidden behind it.

import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import WorkflowProjectGroup from './WorkflowProjectGroup.svelte';
import type { WorkflowHomeGroup } from '../../stores/workflowData';
import type { WorkItem } from '../../types/workflow';

const MERGED = '{"action":"merged","policy":"manual","at":1}';

function group(): WorkflowHomeGroup {
  const run = (id: string, goal: string) => ({ id, goal, workflowId: 'port', state: 'done', disposition: MERGED, createdAt: 1, endedAt: 2, projectId: 'p1' }) as WorkItem;
  return { projectId: 'p1', projectName: 'Widgets', attention: [], running: [], recent: [run('r1', 'Port the parser'), run('r2', 'Port the lexer')], definitions: [] };
}

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
});
