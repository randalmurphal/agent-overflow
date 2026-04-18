<script lang="ts">
  import { onMount, onDestroy } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import MessageTimeline from './MessageTimeline.svelte';
  import ApprovalPrompt from '../composer/ApprovalPrompt.svelte';
  import BackgroundTray from '../shared/BackgroundTray.svelte';
  import Composer from '../composer/Composer.svelte';
  import StatusBar from '../shared/StatusBar.svelte';
  import ProviderStatusBanner from './ProviderStatusBanner.svelte';
  import InteractionModeBadge from './InteractionModeBadge.svelte';
  import BranchToolbar from '../git/BranchToolbar.svelte';
  import GitActionsControl from '../git/GitActionsControl.svelte';
  import ContextWindowMeter from './ContextWindowMeter.svelte';
  import RateLimitsMeter from './RateLimitsMeter.svelte';
  import ModelPicker from '../composer/ModelPicker.svelte';
  import RuntimeModePicker from '../composer/RuntimeModePicker.svelte';
  import ThreadTerminalDrawer from '../terminal/ThreadTerminalDrawer.svelte';
  import DiscussionView from '../discussion/DiscussionView.svelte';
  import DesignView from '../design/DesignView.svelte';
  import DiffPanelDrawer from './DiffPanelDrawer.svelte';
  import PlanSidebar from './PlanSidebar.svelte';
  import PlanFollowUpBanner from './PlanFollowUpBanner.svelte';
  import CompactHeaderMenu from './CompactHeaderMenu.svelte';
  import { createComposerDraftStore } from '../../stores/composerDraft.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  const draft = createComposerDraftStore();
  let lastHydratedThreadId: string | null = null;

  // Threshold in CSS pixels of the header row below which the optional
  // chrome (model/runtime/branch/git) collapses into a dropdown. Chosen
  // to match a typical phone/tablet split where the side panels still
  // leave enough room for the meters + toggles but not for four
  // inline pickers. Intentionally not user-configurable for v1.
  const COMPACT_BREAKPOINT = 640;

  let headerEl: HTMLDivElement | undefined = $state(undefined);
  let headerCompact = $state(false);

  // Observe the header element's content-box width. We can't rely on
  // window width — sidebars and the diff panel eat real estate on the
  // same viewport, so the header shrinks before the viewport does.
  $effect(() => {
    if (!headerEl) return;
    const observer = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const width = entry.contentRect.width;
        headerCompact = width > 0 && width < COMPACT_BREAKPOINT;
      }
    });
    observer.observe(headerEl);
    return () => observer.disconnect();
  });

  $effect(() => {
    const current = pane.thread?.id ?? null;
    if (current === lastHydratedThreadId) return;
    lastHydratedThreadId = current;
    void draft.setThread(current);
  });

  let inDiscussionMode = $derived(
    !!pane.thread && pane.thread.interactionMode === 'discussion' && !!pane.thread.discussionId,
  );
  let inDesignMode = $derived(
    !!pane.thread && pane.thread.interactionMode === 'design',
  );

  // Exposed so the terminal drawer can "send to composer".
  export function addTerminalChipToDraft(chip: {
    id: string;
    label: string;
    preview: string;
    content: string;
    createdAt: number;
  }) {
    draft.addTerminalChip(chip);
  }

  function handleKeydown(e: KeyboardEvent) {
    const isToggleShortcut = (e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey && e.key.toLowerCase() === 'j';
    if (!isToggleShortcut) return;
    if (!pane.thread) return;
    e.preventDefault();
    pane.toggleTerminal();
  }

  onMount(() => {
    window.addEventListener('keydown', handleKeydown);
  });

  onDestroy(() => {
    window.removeEventListener('keydown', handleKeydown);
  });
</script>

{#if pane.thread && inDiscussionMode}
  <DiscussionView {pane} />
{:else if pane.thread}
  <div class="flex h-full min-h-0">
    <div class="flex flex-col {inDesignMode ? 'flex-1 min-w-0 border-r border-border' : 'flex-1 min-w-0'}">
      <div
        bind:this={headerEl}
        data-testid="chat-header"
        data-compact={headerCompact ? 'true' : undefined}
        class="border-b border-border bg-surface-1 px-4 py-2.5 flex items-center gap-x-2 shrink-0 flex-nowrap min-w-0"
      >
        <span class="text-xs font-medium px-1.5 py-0.5 rounded bg-accent/20 text-accent shrink-0">
          {pane.thread.provider === 'claude' ? 'C' : 'X'}
        </span>
        <h2 class="text-sm font-medium text-text-primary truncate min-w-0">{pane.thread.title}</h2>
        <div class="shrink-0"><InteractionModeBadge {pane} /></div>
        {#if !headerCompact}
          <ModelPicker {pane} />
          <RuntimeModePicker {pane} />
          <BranchToolbar {pane} />
          <GitActionsControl {pane} />
        {/if}
        {#if pane.contextWindow}
          <div class="shrink-0"><ContextWindowMeter data={pane.contextWindow} /></div>
        {/if}
        {#if pane.rateLimits.length > 0}
          <div class="shrink-0"><RateLimitsMeter limits={pane.rateLimits} /></div>
        {/if}
        <button
          type="button"
          class="rounded border border-border px-2 py-0.5 text-xs text-text-secondary hover:bg-surface-2/60 cursor-pointer shrink-0"
          data-testid="diff-panel-toggle"
          aria-pressed={pane.diffPanel.open}
          aria-label="Toggle diff panel"
          title="Toggle diff panel (⇧⌘G)"
          onclick={() => pane.toggleDiffPanel()}
        >
          Diffs
        </button>
        <button
          type="button"
          class="rounded border border-border px-2 py-0.5 text-xs text-text-secondary hover:bg-surface-2/60 cursor-pointer shrink-0"
          data-testid="plan-sidebar-toggle"
          aria-pressed={pane.showPlanSidebar}
          aria-label="Toggle plan sidebar"
          title="Toggle plan sidebar"
          onclick={() => pane.togglePlanSidebar()}
        >
          Plans
        </button>
        {#if headerCompact}
          <div data-testid="chat-header-compact" class="shrink-0">
            <CompactHeaderMenu label="More">
              {#snippet children()}
                <!--
                  The pickers own their own open/close flows, so we do
                  not wire the passed-in onClose here — selecting a
                  model/mode/branch leaves the outer More menu open
                  and the user can click the backdrop to dismiss it.
                -->
                <ModelPicker {pane} />
                <RuntimeModePicker {pane} />
                <BranchToolbar {pane} />
                <GitActionsControl {pane} />
              {/snippet}
            </CompactHeaderMenu>
          </div>
        {/if}
        <span class="ml-auto text-xs text-text-secondary truncate min-w-0 shrink max-w-[200px]" title={pane.thread.workspacePath}>{pane.thread.workspacePath}</span>
      </div>

      <ProviderStatusBanner {pane} />

      <MessageTimeline {pane} />
      <ApprovalPrompt {pane} />
      <BackgroundTray {pane} />
      <PlanFollowUpBanner {pane} {draft} />
      <Composer {pane} {draft} />
      <StatusBar {pane} />
      {#if pane.diffPanel.open && pane.thread}
        {#key pane.thread.id}
          <DiffPanelDrawer {pane} />
        {/key}
      {/if}
      {#if pane.showTerminal && pane.thread}
        {#key pane.thread.id}
          <ThreadTerminalDrawer {pane} onSendToComposer={addTerminalChipToDraft} />
        {/key}
      {/if}
    </div>
    <PlanSidebar {pane} />
    {#if inDesignMode}
      <div class="flex-1 min-w-0">
        <DesignView {pane} />
      </div>
    {/if}
  </div>
{:else}
  <div class="flex items-center justify-center h-full">
    <div class="text-center">
      <svg class="w-10 h-10 text-text-secondary/30 mx-auto mb-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
      </svg>
      <p class="text-text-secondary text-lg">Select or create a thread</p>
      <p class="text-text-secondary/60 text-sm mt-1">Use the sidebar to get started</p>
    </div>
  </div>
{/if}
