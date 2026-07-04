<script lang="ts">
  // Sidebar layout shell. Composes the sub-sections; any domain logic
  // (filtering, project creation, settings launch) lives in the children.
  // Keep this file a layout-only file — if you're tempted to add a
  // handler here, it probably belongs in the relevant child.

  import { onDestroy } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { refreshProjects } from '../../stores/projects.svelte';
  import { subscribeJumpHints } from '../../stores/keyboardModifiers.svelte';
  import {
    getSidebarWidth,
    persistSidebarWidth,
    setSidebarWidthLive,
  } from '../../stores/sidebarLayout.svelte';
  import SidebarSearch from './SidebarSearch.svelte';
  import ProjectsSection from './ProjectsSection.svelte';
  import UsageFooter from './UsageFooter.svelte';
  import SystemStatsFooter from './SystemStatsFooter.svelte';
  import SettingsFooter from './SettingsFooter.svelte';
  import SidebarResizer from './SidebarResizer.svelte';
  import ThreadFromPRDialog from './ThreadFromPRDialog.svelte';

  interface Props {
    pane: ThreadPane | null;
    onOpenSettings?: () => void;
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

  // Kick off the projects fetch on mount. Callers refresh threads from
  // App.svelte, so the sidebar only needs to own project loading.
  $effect(() => {
    void refreshProjects();
  });

  // Install the global Cmd/Ctrl modifier listener for the Cmd+1..9 jump
  // hints. The store refcounts subscribers so multiple sidebar mounts
  // (HMR, tests) don't duplicate listeners.
  const releaseJumpHints = subscribeJumpHints();
  onDestroy(releaseJumpHints);
</script>

<aside
  class="relative shrink-0 border-r border-border-subtle bg-transparent flex flex-col h-full"
  style="width: {getSidebarWidth()}px"
  data-testid="sidebar"
>
  <SidebarSearch {registerFocusSearch} />
  <ProjectsSection {pane} />
  <UsageFooter />
  <SystemStatsFooter />
  <SettingsFooter {onOpenSettings} />
  <SidebarResizer
    width={getSidebarWidth()}
    onResizeLive={setSidebarWidthLive}
    onResizeEnd={persistSidebarWidth}
    pane={pane ?? undefined}
  />
</aside>

<ThreadFromPRDialog
  open={showFromPR}
  pane={pane ?? undefined}
  onClose={() => {
    showFromPR = false;
  }}
/>
