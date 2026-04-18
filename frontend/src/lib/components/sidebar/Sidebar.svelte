<script lang="ts">
  // Sidebar layout shell. Composes the sub-sections; any domain logic
  // (filtering, project creation, settings launch) lives in the children.
  // Keep this file a layout-only file — if you're tempted to add a
  // handler here, it probably belongs in the relevant child.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import { refreshProjects } from '../../stores/projects.svelte';
  import { expandProjectsForActiveThread } from '../../stores/sidebar.svelte';
  import SidebarSearch from './SidebarSearch.svelte';
  import ProjectsSection from './ProjectsSection.svelte';
  import SettingsFooter from './SettingsFooter.svelte';
  import ThreadFromPRDialog from './ThreadFromPRDialog.svelte';

  interface Props {
    pane: ThreadPane;
    onOpenSettings?: () => void;
    onStartDiscussion?: (thread: Thread) => void;
    /** Palette/command hook: receives a focus callback for the search input. */
    registerFocusSearch?: (focus: () => void) => void;
    /** Palette/command hook: receives a callback that opens the
     * "new thread from PR" dialog. Kept even though no visible button
     * triggers it from the sidebar — the command palette owns the entry
     * point in v13. */
    registerOpenFromPR?: (openFromPR: () => void) => void;
  }

  let {
    pane,
    onOpenSettings,
    onStartDiscussion,
    registerFocusSearch,
    registerOpenFromPR,
  }: Props = $props();

  let showFromPR = $state(false);

  $effect(() => {
    if (registerOpenFromPR) {
      registerOpenFromPR(() => {
        showFromPR = true;
      });
    }
  });

  // Keep the project of the currently-open thread expanded so the user
  // never sees their active thread hidden behind a collapsed chevron.
  $effect(() => {
    expandProjectsForActiveThread(pane.threadId);
  });

  // Kick off the projects fetch on mount. Callers refresh threads from
  // App.svelte, so the sidebar only needs to own project loading.
  $effect(() => {
    void refreshProjects();
  });
</script>

<aside
  class="w-[292px] shrink-0 border-r border-border/70 bg-surface-1/70 backdrop-blur-md flex flex-col h-full"
  data-testid="sidebar"
>
  <SidebarSearch {registerFocusSearch} />
  <ProjectsSection {pane} {onStartDiscussion} />
  <SettingsFooter {onOpenSettings} />
</aside>

<ThreadFromPRDialog
  open={showFromPR}
  {pane}
  onClose={() => {
    showFromPR = false;
  }}
/>
