// What Workflows home lists (UI-SPEC §3.2): the project groups after the
// project filter and the list find's query, and the rows they hold. One
// source for the rows, the Recent counts and the empty state. The query is
// the home list find's (components/workflows/workflowsPaneFind.svelte.ts),
// never persisted: it is '' whenever that find is closed or its pane gone.

import { getProjectLabelText, getProjects } from './projects.svelte';
import { getWorkflowAutomations, getWorkflowCatalog, getWorkflowRuns } from './workflowRuns.svelte';
import { workflowHomeGroups, workflowHomeRowCount, type WorkflowHomeGroup } from './workflowData';
import { getWorkflowProjectFilter } from './workflowsOverlay.svelte';
import { findNeedle } from '../utils/findMatch';

class WorkflowsHomeList {
  query = $state('');
  readonly needle = $derived(findNeedle(this.query));
  readonly projectNames = $derived(
    new Map(getProjects().map((entry) => [entry.project.id, getProjectLabelText(entry.project.id)] as const)),
  );
  readonly groups: readonly WorkflowHomeGroup[] = $derived.by(() =>
    workflowHomeGroups(
      {
        runs: getWorkflowRuns(),
        projectNames: this.projectNames,
        projectFilter: getWorkflowProjectFilter(),
        definitionsOf: (projectId) => getWorkflowCatalog(projectId)?.workflows ?? [],
        automationsOf: (projectId) => getWorkflowAutomations(projectId),
      },
      this.needle,
    ));
  readonly rowCount = $derived(workflowHomeRowCount(this.groups));
}

const home = new WorkflowsHomeList();

/** Reactive: home's groups, filtered by the project filter and the query. */
export function getWorkflowsHomeGroups(): readonly WorkflowHomeGroup[] {
  return home.groups;
}

/** Reactive: the run and definition rows home lists. */
export function getWorkflowsHomeRowCount(): number {
  return home.rowCount;
}

/** Reactive: the query narrowing home; '' when it lists every row. */
export function getWorkflowsHomeQuery(): string {
  return home.query;
}

export function setWorkflowsHomeQuery(query: string): void {
  if (home.query !== query) home.query = query;
}
