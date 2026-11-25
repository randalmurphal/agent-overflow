<script lang="ts">
  // Right-aligned action cluster for the chat header, and the home of the
  // pane's git-status subscription.
  //
  // Desktop: [ ticket ] [ PR#123 ] [ +X −Y ] [ git ] [ next thread ]
  // [ take control ] [ browser ] [ menu ]. Compact: [ +X −Y ]
  // [ next thread ] [ menu ], with the rest as rows of the menu
  // (ChatHeaderMenu).
  //
  // This component is the pane's attacher on the shared, workspace-keyed
  // git-status store (stores/gitStatusStore.svelte.ts). GitActionsControl and
  // the two badges read the resulting status through `pane.gitStatus`, which
  // is a view onto the same entry — so two panes on one worktree share one
  // subscription and can never show different Commit/Push state.
  import { untrack } from 'svelte';
  import Globe from '@lucide/svelte/icons/globe';
  import SquarePen from '@lucide/svelte/icons/square-pen';
  import { nextAssistantThreadAction } from '../sidebar/threadRowActions';
  import { assistantsOn, threadComputer } from '../../stores/assistants.svelte';
  import { openThreadFromNavigation } from '../../stores/panes.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { chordHintForCommand } from '../../stores/keybindings.svelte';
  import { threadScopeGrant } from '../../transport/entityScopes';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import { attachGitStatus } from '../../stores/gitStatusStore.svelte';
  import { workspaceKeyForThread } from '../../utils/workspaceKey';
  import GitActionsControl from '../git/GitActionsControl.svelte';
  import PrBadge from '../git/PrBadge.svelte';
  import WorkspaceDiffBadge from '../git/WorkspaceDiffBadge.svelte';
  import ChatHeaderMenu from './ChatHeaderMenu.svelte';
  import ThreadTicketButton from './ThreadTicketButton.svelte';
  import { openPrReview, takeControlState, toggleTakeControl, toggleWorkspaceReview } from './headerPaneActions';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ProviderIcon from '../shared/ProviderIcon.svelte';
  import {
    browserPresentationWanted,
    hydrateBrowserCompanionState,
    reconcileBrowserCompanionForPane,
  } from '../../stores/browserCompanion.svelte';
  import { browserHeaderButton, runBrowserHeaderIntent } from '../../stores/browserActions';

  let { pane }: { pane: ThreadPane } = $props();

  let compact = $derived(isCompactLayout());

  // New Thread, Same Assistant (docs/specs/standing-agents.md, "Next
  // thread"): on an assistant's current thread, desktop and compact, while
  // its computer has assistants on.
  let isAssistant = $derived(Boolean(pane.thread?.agentId) && !pane.thread?.archived && pane.threadId !== null && assistantsOn(threadComputer(pane.threadId)));
  // StartNextAssistantThread rides threads:autonomy.
  let nextThreadGrant = $derived(threadScopeGrant('threads:autonomy', pane.threadId, pane.thread?.projectId));
  function startNextAssistantThread(): void {
    const thread = pane.thread;
    if (!thread) return;
    void nextAssistantThreadAction({
      thread,
      isActive: true,
      clearPane: () => pane.clear(),
      switchPane: async (next) => { await openThreadFromNavigation(next, pane); },
      reportError: (message, err) => pane.setGeneralError(message, err),
    });
  }

  // Take control: a button on the desktop, a row of the menu under compact,
  // both from one definition (takeControlState).
  let takeControl = $derived(takeControlState(pane));

  let reviewToggleChord = $derived(chordHintForCommand('diff.panel.toggle'));

  // Attach deps as $derived primitives so the effect re-runs only when the
  // values actually change — pane.replaceThread() fires for unrelated metadata
  // (token usage, mode) on every streamed token, and value-equality on these
  // derives suppresses the downstream re-subscribe.
  //
  // Deliberately NOT the thread id: the entity is the workspace, and two
  // threads in one worktree are one entity. Tracking the id made a
  // same-workspace thread switch release and re-attach, which bounced the
  // backend refcount through zero — the whole fs watcher torn down and
  // rebuilt, and every badge in the pane blanked, for a switch that changed
  // nothing about the checkout. `hasWorkspace` is a boolean for the same
  // reason, and it is the WORKSPACE question rather than "is there a thread
  // row": a draft placeholder names a checkout too, and refusing it a
  // subscription is what left its git affordances rendering dead.
  let hasWorkspace = $derived(pane.workspace !== null);
  let gitStatusKey = $derived(workspaceKeyForThread(pane.thread ?? null));

  // The pane's reference on its workspace's git-status entry. The store owns
  // the subscription, the retry backoff, the `git:status` routing, the
  // transport-reconnect re-acquire, and the observed-branch persist; this
  // effect owns only the reference's lifetime. Consumers read through
  // `pane.gitStatus` and never attach a second time.
  $effect(() => {
    if (!hasWorkspace || gitStatusKey === null) return;
    const attachment = attachGitStatus(gitStatusKey, {
      // Read at SOURCE time, not attach time. The reference outlives any one
      // thread the pane shows, so a re-source (reconnect, retry) has to run
      // against the checkout the pane points at NOW. The store's source
      // prologue runs untracked, so this read cannot pull the ref back into
      // the effect.
      get workspace() {
        return pane.workspace;
      },
    });
    return () => attachment.release();
  });

  // The browser chip: how it renders and what a press means are one answer
  // (browserHeaderButton), its intent chosen from what it shows. The
  // hydration read fills in what the ephemeral `browser:companion-state`
  // channel cannot replay to a fresh UI, and a state the layout could not
  // present when it arrived (compact) is presented once it can.
  let threadIdForBrowser = $derived(pane.threadId ?? '');
  let browserButton = $derived(browserHeaderButton(threadIdForBrowser));
  let browserWanted = $derived(threadIdForBrowser !== '' && browserPresentationWanted(threadIdForBrowser));

  $effect(() => {
    if (threadIdForBrowser) hydrateBrowserCompanionState(threadIdForBrowser);
  });

  $effect(() => {
    const threadId = threadIdForBrowser;
    if (!browserWanted) return;
    untrack(() => reconcileBrowserCompanionForPane(pane.paneId, threadId));
  });
</script>

<!-- Visible on placeholders too: the caller's `pane.thread` gate already
     filters out "no thread at all" panes, and the subcomponents self-gate on
     `pane.workspace` (git and review) or `pane.threadId` (the pieces whose
     subject genuinely is a persisted row). The cluster never shrinks: the
     header moves it to a second row when the title is at its floor, and
     only a pane narrower than the cluster itself wraps it. -->
<div class="ml-auto flex max-w-full shrink-0 flex-wrap items-center justify-end gap-1.5 compact:flex-nowrap compact:gap-2">
  {#if !compact && pane.thread?.ticketLink}
    <ThreadTicketButton thread={pane.thread} />
  {/if}
  {#if !compact}
    <!-- Plain click opens the companion review pane on the PR scope;
         mod+click, or a pane with no review subject yet, falls through to
         the external forge link. -->
    <PrBadge status={pane.gitStatus.status} onOpenReview={() => openPrReview(pane)} />
  {/if}
  {#if hasWorkspace}
    <WorkspaceDiffBadge
      status={pane.gitStatus.status}
      pressed={pane.showReviewPane}
      chord={compact ? null : reviewToggleChord}
      onActivate={() => toggleWorkspaceReview(pane)}
    />
  {/if}

  {#if !compact}
    <GitActionsControl {pane} />
  {/if}

  {#if isAssistant}
    <Button
      variant="secondary"
      size="xs"
      disabled={nextThreadGrant !== 'granted'}
      ariaLabel="New Thread, Same Assistant"
      title={grantBlockedTitle(nextThreadGrant) ?? 'New Thread, Same Assistant'}
      onclick={startNextAssistantThread}
      testId="chat-header-next-thread"
      class="shrink-0 w-6 compact:w-9 px-0"
    >
      {#snippet children()}
        <Icon icon={SquarePen} size={12} strokeWidth={2} class="opacity-90" />
      {/snippet}
    </Button>
  {/if}

  {#if !compact && takeControl.offered}
    <!-- The terminal-green Claude glyph ties the button to the TUI
         provider. -->
    <Button
      variant="secondary"
      size="xs"
      pressed={takeControl.open}
      disabled={takeControl.grant !== 'granted'}
      ariaLabel="Toggle take-control terminal"
      title={grantBlockedTitle(takeControl.grant) ?? 'Take control — open the live Claude TUI terminal'}
      onclick={() => toggleTakeControl(pane)}
      testId="take-control-toggle"
      class="shrink-0 w-6 compact:w-9 px-0"
    >
      {#snippet children()}
        <ProviderIcon provider="claude-tui" size={13} />
      {/snippet}
    </Button>
  {/if}

  {#if !compact && browserButton.rendered}
    {@const intent = browserButton.intent}
    <!-- Browser companion toggle, on the desktop only (the compact menu has
         no browser row). Pressed while the pane is in the layout and on
         screen, and a press then hides it; otherwise a press shows the
         tabs, or opens the pane on a new tab when there are none, and
         brings it out from under a maximized pane. Closing the pane never
         closes the pages. -->
    <Button
      variant="secondary"
      size="xs"
      pressed={browserButton.pressed}
      ariaLabel={browserButton.label}
      title={browserButton.label}
      onclick={() => void runBrowserHeaderIntent(intent)}
      testId="browser-companion-toggle"
      class="shrink-0 w-6 compact:w-9 px-0"
    >
      {#snippet children()}
        <Icon icon={Globe} size={12} strokeWidth={2} class="opacity-90" />
      {/snippet}
    </Button>
  {/if}

  <ChatHeaderMenu {pane} />
</div>
