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
    SIDEBAR_RAIL_WIDTH,
    getSidebarWidth,
    isSidebarCollapsed,
    persistSidebarWidth,
    setSidebarWidthLive,
  } from '../../stores/sidebarLayout.svelte';
  import SidebarToggleButton from './SidebarToggleButton.svelte';
  import SidebarSearch from './SidebarSearch.svelte';
  import ProjectsSection from './ProjectsSection.svelte';
  import UsageFooter from './UsageFooter.svelte';
  import SystemStatsFooter from './SystemStatsFooter.svelte';
  import WorkflowsFooter from './WorkflowsFooter.svelte';
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

  let collapsed = $derived(isSidebarCollapsed());
</script>

<!--
  Collapsed renders a rail, not nothing. It is the one piece of chrome
  guaranteed to exist in every app state — no thread open, settings
  surface up, workflows overlay covering the pane strip — so the way
  back is never conditional on what happens to be mounted next to it.
  A "show sidebar" button in the chat header would have been the closer
  parallel to how companion panes reopen, but ChatHeader renders only
  `{#if pane.thread}` and once per pane: it would vanish exactly when
  the user has no thread and needs the sidebar most, and triple itself
  in a three-pane layout.

  The branch lives in the template, not around the component, so the
  script's project fetch, jump-hint subscription, and the PR-dialog
  registration the palette owns all survive a collapse.
-->
{#if collapsed}
  <aside
    class="relative shrink-0 border-r border-border-subtle bg-transparent flex flex-col h-full items-center pt-3"
    style="width: {SIDEBAR_RAIL_WIDTH}px"
    data-testid="sidebar-rail"
  >
    <SidebarToggleButton />
  </aside>
{:else}
  <aside
    class="relative shrink-0 border-r border-border-subtle bg-transparent flex flex-col h-full"
    style="width: {getSidebarWidth()}px"
    data-testid="sidebar"
  >
    <div class="flex items-center gap-1 px-3 pt-3 pb-2">
      <SidebarSearch {registerFocusSearch} />
      <SidebarToggleButton />
    </div>
    <ProjectsSection {pane} />
    <UsageFooter />
    <SystemStatsFooter />
    <WorkflowsFooter />
    <SettingsFooter {onOpenSettings} />
    <!--
      The divider is mounted only while expanded. A grabbable edge on a
      36px rail would be a second, ambiguous way to expand — it would
      have to either fight the collapsed flag or leave the store saying
      "collapsed" at 250px. mod+b and the rail button are the only two
      ways out, and both restore the stored width exactly.
    -->
    <SidebarResizer
      width={getSidebarWidth()}
      onResizeLive={setSidebarWidthLive}
      onResizeEnd={persistSidebarWidth}
      pane={pane ?? undefined}
    />
  </aside>
{/if}

<ThreadFromPRDialog
  open={showFromPR}
  pane={pane ?? undefined}
  onClose={() => {
    showFromPR = false;
  }}
/>
