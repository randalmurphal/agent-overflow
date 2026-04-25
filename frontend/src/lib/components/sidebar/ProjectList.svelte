<script lang="ts">
  // ProjectList: iterates sorted projects and renders a ProjectItem per
  // row. Virtualization via VirtualList when scale warrants; flat
  // iteration is fine at ~50 projects / 1000 threads.

  import type { ProjectWithCounts, Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { autoAnimate } from '../../utils/autoAnimate';
  import Button from '../primitives/Button.svelte';
  import ProjectItem from './ProjectItem.svelte';

  interface Props {
    projects: readonly ProjectWithCounts[];
    /** Map of project id -> visible threads for that project. */
    threadsByProject: Map<string, Thread[]>;
    pane: ThreadPane;
    onNewThread?: (projectId: string) => void;
    /** Drag-reorder commit. Wired by ProjectsSection only when manual
     *  sort mode is active; ProjectItem ignores it otherwise. */
    onReorder?: (newOrderedIds: string[]) => void;
    /** Click handler for the empty-state "Add Your First Project"
     *  button. Wired by ProjectsSection so the modal stays in the
     *  parent. */
    onAddProject?: () => void;
  }

  let { projects, threadsByProject, pane, onNewThread, onReorder, onAddProject }: Props = $props();

  let orderedIds = $derived(projects.map((p) => p.project.id));
</script>

{#if projects.length === 0}
  <div
    class="mx-2 my-3 flex flex-col items-center gap-2 rounded-xl border border-dashed border-border/60 bg-surface-0/40 px-4 py-6 text-center"
    data-testid="sidebar-projects-empty"
  >
    <p class="text-xs text-fg-muted">No projects yet.</p>
    <Button
      variant="primary"
      size="sm"
      onclick={onAddProject}
      testId="sidebar-projects-empty-add"
    >
      {#snippet children()}Add Your First Project{/snippet}
    </Button>
  </div>
{:else}
  <div
    class="flex-1 overflow-y-auto px-2 py-1"
    data-testid="sidebar-project-list"
    use:autoAnimate
  >
    {#each projects as project (project.project.id)}
      <ProjectItem
        {project}
        threads={threadsByProject.get(project.project.id) ?? []}
        {pane}
        {onNewThread}
        {orderedIds}
        {onReorder}
      />
    {/each}
  </div>
{/if}
