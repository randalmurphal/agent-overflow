<script lang="ts">
  import type { ProjectWithCounts } from '../../types/models';
  import type { WorkItem, WorkflowQueueStateEvent } from '../../types/workflow';
  import WorkflowQueueGroup from './WorkflowQueueGroup.svelte';

  interface Props {
    items: WorkItem[];
    queue: WorkflowQueueStateEvent;
    projects: readonly ProjectWithCounts[];
    viewOnly: boolean;
    workflowName: (item: WorkItem) => string;
    onOpenRun: (item: WorkItem) => void;
  }

  let { items, queue, projects, viewOnly, workflowName, onOpenRun }: Props = $props();

  let groups = $derived.by(() => {
    const projectRows = new Map(projects.map((entry) => [entry.project.id, entry.project]));
    const queueProjects = new Map((queue.projects ?? []).map((project) => [project.projectId, project]));
    const grouped = new Map<string, WorkItem[]>();
    for (const item of items) {
      const group = grouped.get(item.projectId) ?? [];
      group.push(item);
      grouped.set(item.projectId, group);
    }
    return [...grouped].map(([projectId, projectItems]) => {
      const project = projectRows.get(projectId);
      const state = queueProjects.get(projectId);
      const queued = projectItems
        .filter((item) => item.state === 'queued')
        .sort((left, right) => left.sortPosition - right.sortPosition || left.createdAt - right.createdAt || left.id.localeCompare(right.id));
      return {
        projectId,
        projectName: project?.name ?? projectId,
        projectColor: project?.color ?? 'var(--fg-hint)',
        queued,
        paused: state?.paused ?? project?.workflowQueuePaused ?? false,
        concurrency: state?.concurrency ?? project?.workflowConcurrency ?? 0,
        runningCount: state?.runningCount ?? projectItems.filter((item) => item.state === 'running').length,
      };
    }).sort((left, right) => left.projectName.localeCompare(right.projectName) || left.projectId.localeCompare(right.projectId));
  });
</script>

<section class="space-y-3" data-testid="wf-up-next">
  <h2 class="text-[11px] font-semibold uppercase tracking-wider text-fg-muted">Queues · {groups.length}</h2>
  {#each groups as group (group.projectId)}
    <WorkflowQueueGroup
      {...group}
      globalActive={queue.active}
      globalConcurrency={queue.globalConcurrency}
      {viewOnly}
      {workflowName}
      {onOpenRun}
    />
  {/each}
</section>
