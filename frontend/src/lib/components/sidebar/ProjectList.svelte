<script lang="ts">
  // ProjectList: iterates sorted projects and renders a ProjectItem per
  // row. Virtualization via VirtualList when scale warrants; flat
  // iteration is fine at ~50 projects / 1000 threads.

  import type { ProjectWithCounts, Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { autoAnimate } from '../../utils/autoAnimate';
  import ProjectItem from './ProjectItem.svelte';

  interface Props {
    projects: readonly ProjectWithCounts[];
    /** Map of project id -> visible threads for that project. */
    threadsByProject: Map<string, Thread[]>;
    pane: ThreadPane | null;
    onNewThread?: (projectId: string) => void;
    /** Drag-reorder commit. Wired by ProjectsSection only when manual
     *  sort mode is active; ProjectItem ignores it otherwise. */
    onReorder?: (newOrderedIds: string[]) => void;
  }

  let { projects, threadsByProject, pane, onNewThread, onReorder }: Props = $props();

  let orderedIds = $derived(projects.map((p) => p.project.id));
</script>

{#if projects.length === 0}
  <div
    class="px-3 pt-4 text-center text-xs text-fg-muted"
    data-testid="sidebar-projects-empty"
  >
    No projects yet. Click + to add one.
  </div>
{:else}
  <div
    class="flex-1 overflow-y-auto px-2 py-1"
    data-testid="sidebar-project-list"
    use:autoAnimate
  >
    {#each projects as project, index (project.project.id)}
      <ProjectItem
        {project}
        threads={threadsByProject.get(project.project.id) ?? []}
        {pane}
        {onNewThread}
        {orderedIds}
        {onReorder}
        separatedFromPrevious={index > 0}
      />
    {/each}
  </div>
{/if}
