<script lang="ts">
  // Workspace mode tabs that live above the projects list. These are
  // navigation, not filters — clicking Design switches the main view to
  // the most-recent design thread (or the design empty-state if none
  // exists in scope), and Chat does the same for chat/plan threads. The
  // sidebar's project + thread list stays mixed regardless of which tab
  // is active; tabs only choose which mode the main pane is in.
  //
  // Visual rules: pill-style segmented control, sized to fit the sidebar
  // chrome density. Active pill carries a subtle accent fill so the
  // current mode is glanceable; inactive pill is text-only with a
  // soft hover. The whole control is centered in a thin row above
  // SidebarSearch — no border above or below, so it reads as one quiet
  // step in the sidebar's vertical rhythm.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreads } from '../../stores/threads.svelte';
  import { openThreadInPane } from '../../stores/panes.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  type ModeTab = 'chat' | 'design';

  function tabForMode(mode: string | undefined): ModeTab | null {
    if (mode === 'design') return 'design';
    if (mode === 'chat' || mode === 'plan') return 'chat';
    // Discussion threads bypass the top tab — they own the full surface
    // via DiscussionView. Leave activeTab unchanged when one is loaded.
    return null;
  }

  // Visual state. When the active thread carries a tab-eligible mode we
  // show that; otherwise fall back to the pane's last user intent.
  let currentTab = $derived<ModeTab>(
    tabForMode(pane.thread?.mode) ?? pane.activeTab,
  );

  async function selectTab(tab: ModeTab): Promise<void> {
    // Always sync the visible pill, even when nothing else changes.
    pane.setActiveTab(tab);

    // If the active thread already matches the target tab, the click is
    // just a tab confirmation — nothing to navigate to.
    if (pane.thread && tabForMode(pane.thread.mode) === tab) return;

    // No project context (no thread loaded) → tab click is a no-op for
    // navigation. We don't reach into "any project" as a fallback;
    // that would silently jump the user into someone else's project.
    // Creating a fresh thread is the project's "+ New" responsibility,
    // not the tabs'.
    const projectId = pane.thread?.projectId ?? null;
    if (!projectId) return;

    // Find the most-recent thread of the target type IN THE SAME
    // PROJECT. If there is one, switch to it; otherwise leave the pane
    // alone — ChatView detects the (thread.mode, activeTab) mismatch
    // and renders the empty state for the target tab with project
    // context derived from the still-loaded thread.
    const candidates = getThreads().filter(
      (t) => tabForMode(t.mode) === tab && t.projectId === projectId,
    );
    candidates.sort(
      (a, b) =>
        (b.latestTurnCompletedAt ?? b.updatedAt) - (a.latestTurnCompletedAt ?? a.updatedAt),
    );
    const found = candidates[0];
    if (found) {
      await openThreadInPane(found, pane);
    }
  }

  const TABS: ReadonlyArray<{ value: ModeTab; label: string }> = [
    { value: 'chat', label: 'Chat' },
    { value: 'design', label: 'Design' },
  ];
</script>

<div
  class="flex justify-center px-3 pt-0.5 pb-1 shrink-0"
  data-testid="sidebar-mode-tabs"
>
  <div
    role="radiogroup"
    aria-label="Workspace Mode"
    class="inline-flex items-center gap-0.5 rounded-full bg-surface-2/40 p-0.5"
  >
    {#each TABS as tab (tab.value)}
      {@const active = currentTab === tab.value}
      <button
        type="button"
        role="radio"
        aria-checked={active}
        onclick={() => void selectTab(tab.value)}
        data-testid="sidebar-mode-tab-{tab.value}"
        class={[
          'rounded-full px-2.5 py-0.5 text-[11px] font-medium',
          'cursor-pointer transition-colors select-none',
          'focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
          active
            ? 'bg-surface-0 text-fg shadow-[var(--shadow-sheet)]'
            : 'text-fg-muted hover:text-fg',
        ].join(' ')}
      >
        {tab.label}
      </button>
    {/each}
  </div>
</div>
