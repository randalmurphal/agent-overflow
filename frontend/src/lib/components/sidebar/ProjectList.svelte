<script lang="ts">
  // ProjectList: iterates the sorted entries and renders a ProjectItem per
  // row. Virtualization via VirtualList when scale warrants; flat
  // iteration is fine at ~50 projects / 1000 threads.
  //
  // What a row shows is the entry (./sidebarEntries.ts). This file reads no
  // project row and derives nothing from one.

  import type { Thread, ThreadGroup } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { NO_GROUPS } from '../../stores/threadGroups.svelte';
  import { disambiguatedPathLabels } from '../../utils/pathDisplay';
  import { sidebarFlip, sidebarEnter, sidebarExit } from '../../utils/sidebarAnimate';
  import ProjectItem from './ProjectItem.svelte';
  import type { SidebarEntry } from './sidebarEntries';
  import type { ProjectNewThreadHandler, ProjectNewTerminalHandler } from './projectNewThread';

  interface Props {
    entries: readonly SidebarEntry[];
    emptyMessage?: string;
    /** Map of entry id -> visible threads for that entry. */
    threadsByProject: Map<string, Thread[]>;
    /** Map of entry id -> visible thread groups for that entry. */
    groupsByProject?: Map<string, ThreadGroup[]>;
    pane: ThreadPane | null;
    onNewThread?: ProjectNewThreadHandler;
    onNewTerminal?: ProjectNewTerminalHandler;
    /** Commit a new ordering. Wired by ProjectsSection only when manual
     *  sort mode is active; ProjectItem ignores it otherwise. */
    onReorder?: (newOrderedIds: string[]) => void;
    /** Fold or unfold one entry, by its key. */
    onSetExpanded?: (key: string, expanded: boolean) => void;
  }

  let {
    entries,
    emptyMessage = 'No projects yet. Click + to add one.',
    threadsByProject,
    groupsByProject,
    pane,
    onNewThread,
    onNewTerminal,
    onReorder,
    onSetExpanded,
  }: Props = $props();

  // One array for every project with no threads, so its list is not
  // handed a new empty one on each render.
  const NO_THREADS: Thread[] = [];

  let orderedIds = $derived(entries.map((entry) => entry.id));
  // Duplicate names are legal (paths are the unique key), so a row's label is
  // disambiguated against the rows BESIDE it: a name alone while it is the
  // only one on screen, a dim parent-dir prefix while its twin is too. Over
  // the entries, which is what the sidebar renders; two members of one
  // repository share a name by definition and are not a collision.
  let labels = $derived(disambiguatedPathLabels(entries));
</script>

{#if entries.length === 0}
  <div
    class="px-3 pt-4 text-center text-xs text-fg-muted"
    data-testid="sidebar-projects-empty"
  >
    {emptyMessage}
  </div>
{:else}
  <div
    class="px-2 py-1"
    data-testid="sidebar-project-list"
  >
    {#each entries as entry, index (entry.key)}
      <!-- animate:/transition: need an element as the each's immediate
           child; ProjectItem is a component, so a layout-neutral block
           wrapper carries them. -->
      <div animate:sidebarFlip in:sidebarEnter out:sidebarExit>
      <ProjectItem
        {entry}
        displayLabel={labels.get(entry.id) ?? { prefix: '', name: entry.name }}
        threads={threadsByProject.get(entry.id) ?? NO_THREADS}
        groups={groupsByProject?.get(entry.id) ?? NO_GROUPS}
        {pane}
        {onNewThread}
        {onNewTerminal}
        {orderedIds}
        {onReorder}
        {onSetExpanded}
        separatedFromPrevious={index > 0}
      />
      </div>
    {/each}
  </div>
{/if}
