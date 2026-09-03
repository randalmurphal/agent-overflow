<script lang="ts">
  // ProjectList: iterates sorted projects and renders a ProjectItem per
  // row. Virtualization via VirtualList when scale warrants; flat
  // iteration is fine at ~50 projects / 1000 threads.

  import type { ProjectWithCounts, Thread, ThreadGroup } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { NO_GROUPS } from '../../stores/threadGroups.svelte';
  import { sidebarFlip, sidebarEnter, sidebarExit } from '../../utils/sidebarAnimate';
  import ProjectItem from './ProjectItem.svelte';
  import type { ProjectNewThreadHandler, ProjectNewTerminalHandler } from './projectNewThread';

  interface Props {
    projects: readonly ProjectWithCounts[];
    /** Map of project id -> visible threads for that project. */
    threadsByProject: Map<string, Thread[]>;
    /** Map of project id -> visible thread groups for that project. */
    groupsByProject?: Map<string, ThreadGroup[]>;
    pane: ThreadPane | null;
    onNewThread?: ProjectNewThreadHandler;
    onNewTerminal?: ProjectNewTerminalHandler;
    /** Drag-reorder commit. Wired by ProjectsSection only when manual
     *  sort mode is active; ProjectItem ignores it otherwise. */
    onReorder?: (newOrderedIds: string[]) => void;
  }

  let {
    projects,
    threadsByProject,
    groupsByProject,
    pane,
    onNewThread,
    onNewTerminal,
    onReorder,
  }: Props = $props();

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
    class="px-2 py-1"
    data-testid="sidebar-project-list"
  >
    {#each projects as project, index (project.project.id)}
      <!-- animate:/transition: need an element as the each's immediate
           child; ProjectItem is a component, so a layout-neutral block
           wrapper carries them. -->
      <div animate:sidebarFlip in:sidebarEnter out:sidebarExit>
      <ProjectItem
        {project}
        threads={threadsByProject.get(project.project.id) ?? []}
        groups={groupsByProject?.get(project.project.id) ?? NO_GROUPS}
        {pane}
        {onNewThread}
        {onNewTerminal}
        {orderedIds}
        {onReorder}
        separatedFromPrevious={index > 0}
      />
      </div>
    {/each}
  </div>
{/if}
