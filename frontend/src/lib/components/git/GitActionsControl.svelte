<script lang="ts">
  // Split-button control: primary action on the left, caret opens a menu
  // with the remaining git actions. The dropdown composes the Popover +
  // Menu + MenuItem primitives so it inherits portaling, arrow-key nav,
  // typeahead, and focus management from the shared implementation.
  //
  // Status freshness model: this component does NOT poll. It subscribes
  // to the backend gitwatch stream when the workspace path becomes
  // known, then receives push updates whenever the working tree, .git
  // refs, or anything else under the workspace changes. The backend
  // dedups identical statuses, so the wire stays quiet during heavy fs
  // churn (build outputs, ignored files). The subscription is keyed on
  // `pane.thread?.worktreePath ?? workspacePath`, derived through
  // $derived so the effect short-circuits on value-equality —
  // pane.replaceThread() calls that change unrelated thread metadata
  // (token usage, mode, etc.) no longer thrash the git status pipe.

  import { onMount, onDestroy } from 'svelte';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { GitStatus } from '../../types/git';
  import { errString } from '../../utils/errors';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { wailsEventOn } from '../../stores/events';
  import { getTransportStatus } from '../../stores/transportStatus.svelte';
  import {
    GetGitStatus,
    GitStatusSubscribe,
    GitStatusUnsubscribe,
    type GitStatusSubscriptionResult,
  } from '../../stores/bindings';
  import CommitDialog from './CommitDialog.svelte';
  import ShipChangesDrawer from './ShipChangesDrawer.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import {
    primaryActionFor,
    runCreatePRAction,
    runPullAction,
    runPushAction,
    runRemoveWorktreeAction,
    type GitActionCtx,
  } from './gitActions';

  let { pane }: { pane: ThreadPane } = $props();

  // Wire payload shape for "git:status" events. Wails doesn't generate
  // a TS type for this (event payloads aren't part of the binding
  // surface), so the shape is declared locally and kept in sync with
  // GitStatusEvent in app_gitwatch.go.
  interface GitStatusEvent {
    subscriptionId: string;
    status: GitStatus;
  }

  let status = $state<GitStatus | null>(null);
  let statusError = $state(false);
  let actionLoading = $state(false);

  let showCommit = $state(false);
  let showShip = $state(false);
  let showDropdown = $state(false);
  let showRemoveWorktreeConfirm = $state(false);

  let menuTriggerEl: HTMLButtonElement | undefined = $state(undefined);

  // gitCwd is a derived primitive — Svelte's $derived value-equality
  // means the subscribe effect only re-runs when the actual cwd
  // *value* changes, not on every pane.replaceThread() call (which
  // also fires for token-usage updates, mode changes, hasIncompleteTurn
  // patches, etc.). This is what kills the per-token flicker the
  // previous pane.threadId-tracking $effect had: the underlying
  // signal still fires, but the derived's value-equal recomputation
  // suppresses the downstream re-run.
  let threadId = $derived(pane.threadId);
  let gitCwd = $derived(
    pane.thread?.worktreePath ?? pane.thread?.workspacePath ?? null,
  );

  // transportConnected gates subscription on the WS being live. On
  // reconnect (disconnected → connected) the effect re-runs and
  // re-subscribes; the backend drops subscriptions on raw disconnect
  // via transport.ConnState cleanup, so any old subscriptionId we
  // held is stale.
  let transportConnected = $derived(getTransportStatus().status === 'connected');

  function handleOpenShip(): void {
    if (pane.threadId) showShip = true;
  }

  onMount(() => {
    window.addEventListener('agent-overflow:open-ship-changes', handleOpenShip);
  });

  onDestroy(() => {
    if (typeof window !== 'undefined') {
      window.removeEventListener('agent-overflow:open-ship-changes', handleOpenShip);
    }
  });

  let isWorktree = $derived(!!pane.thread?.worktreePath);
  // Forge-aware labels for the menu item. Falls back to GitHub strings
  // when status.forge is empty (no origin or unsupported host) — the
  // canCreatePR gate keeps the action disabled in that case.
  let labels = $derived(forgeLabels(status?.forge));
  let createPRLabel = $derived(labels.createAction);
  let canCreatePR = $derived(
    status !== null &&
      status.hasUpstream &&
      !status.openPrUrl &&
      !status.isDefaultBranch &&
      // Disable when no recognised forge — we have nothing to dispatch to.
      status.forge !== '' &&
      status.forge !== undefined,
  );

  // Manual one-shot refresh used after git actions (commit/push/pull/PR).
  // The subscribe stream will re-emit ~250ms later via the fs watcher,
  // but explicit fetch removes that perceptible debounce delay between
  // the action completing and the button label catching up.
  async function refreshStatusNow(): Promise<void> {
    const id = threadId;
    if (!id) return;
    try {
      const result = (await GetGitStatus(id)) as GitStatus;
      status = result;
      statusError = false;
    } catch (err) {
      console.error('Failed to refresh git status:', err);
      statusError = true;
      pane.setGeneralError(`Failed to load git status: ${errString(err)}`);
    }
  }

  $effect(() => {
    const id = threadId;
    const cwd = gitCwd;
    const connected = transportConnected;

    if (!id || !cwd || !connected) {
      // Reset on real disqualifiers: thread cleared, workspace gone,
      // or transport down. No subscribe to issue, no listener to
      // attach — but DO NOT touch showDropdown/showCommit/etc., those
      // are independent UI state owned by the user's interactions.
      status = null;
      statusError = false;
      return;
    }

    let cancelled = false;
    let cancelEvent: (() => void) | null = null;
    let activeId: string | null = null;

    void (async () => {
      try {
        const result = (await GitStatusSubscribe(id)) as GitStatusSubscriptionResult;
        if (cancelled) {
          // Effect re-ran before subscribe resolved; release the
          // freshly-created subscription rather than orphaning it.
          // The connection-tied safety net would catch this on
          // disconnect, but releasing eagerly keeps the watcher
          // refcount honest in steady state.
          void GitStatusUnsubscribe(result.id).catch(() => undefined);
          return;
        }
        activeId = result.id;
        status = result.status;
        statusError = false;

        cancelEvent = wailsEventOn<GitStatusEvent>('git:status', (payload) => {
          if (!payload || payload.subscriptionId !== activeId) return;
          status = payload.status;
        });
      } catch (err) {
        if (cancelled) return;
        console.error('GitStatusSubscribe failed:', err);
        status = null;
        statusError = true;
        pane.setGeneralError(`Failed to subscribe to git status: ${errString(err)}`);
      }
    })();

    return () => {
      cancelled = true;
      if (cancelEvent) cancelEvent();
      if (activeId) {
        void GitStatusUnsubscribe(activeId).catch(() => undefined);
      }
    };
  });

  let primaryAction = $derived(primaryActionFor(status));

  function ctx(): GitActionCtx {
    return {
      threadId: pane.threadId!,
      reportError: (msg) => pane.setGeneralError(msg),
      refreshStatus: () => refreshStatusNow(),
      replacePaneThread: (t) => pane.replaceThread(t),
      forge: status?.forge,
    };
  }

  async function executePrimary() {
    if (!pane.threadId) return;
    switch (primaryAction.action) {
      case 'commit':
        showCommit = true;
        break;
      case 'push':
        await guard(() => runPushAction(ctx()));
        break;
      case 'pull':
        await guard(() => runPullAction(ctx()));
        break;
    }
  }

  async function guard(run: () => Promise<void>): Promise<void> {
    if (!pane.threadId || actionLoading) return;
    actionLoading = true;
    try {
      await run();
    } finally {
      actionLoading = false;
    }
  }

  function closeMenu(): void {
    showDropdown = false;
    menuTriggerEl?.focus();
  }

  function handleCommitClose() {
    showCommit = false;
    void refreshStatusNow();
  }
</script>

{#if statusError}
  <Button
    variant="danger-outline"
    size="sm"
    onclick={() => void refreshStatusNow()}
    testId="git-actions-error"
    title="Failed to load git status. Click to retry."
  >
    {#snippet children()}Git: error{/snippet}
  </Button>
{:else if status && status.isRepo}
  <div class="flex">
    <button
      onclick={executePrimary}
      disabled={primaryAction.disabled || actionLoading}
      title={primaryAction.tooltip}
      class="text-xs px-2.5 py-1 rounded-l border border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer disabled:opacity-40 disabled:cursor-not-allowed focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      {actionLoading ? '...' : primaryAction.label}
    </button>
    <button
      bind:this={menuTriggerEl}
      onclick={() => (showDropdown = !showDropdown)}
      aria-label="More git actions"
      aria-expanded={showDropdown}
      aria-haspopup="menu"
      class="text-xs px-1 py-1 rounded-r border border-l-0 border-border text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
    >
      <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-80" />
    </button>
  </div>

  {@const menuStatus = status}
  <Popover
    anchor={menuTriggerEl}
    open={showDropdown}
    onClose={closeMenu}
    placement="bottom-end"
    role="none"
  >
    {#snippet children()}
      <Menu ariaLabel="Git actions" onClose={closeMenu}>
        <MenuItem
          label="Commit"
          disabled={!menuStatus.hasChanges}
          onSelect={() => {
            showDropdown = false;
            showCommit = true;
          }}
        />
        <MenuItem
          label="Push"
          disabled={menuStatus.aheadCount === 0}
          onSelect={() => {
            showDropdown = false;
            void guard(() => runPushAction(ctx()));
          }}
        />
        <MenuItem
          label="Pull"
          disabled={menuStatus.behindCount === 0}
          onSelect={() => {
            showDropdown = false;
            void guard(() => runPullAction(ctx()));
          }}
        />
        <MenuItem
          label={createPRLabel}
          disabled={!canCreatePR}
          onSelect={() => {
            showDropdown = false;
            void guard(() => runCreatePRAction(ctx()));
          }}
        />
        <MenuDivider />
        <MenuItem
          label="Ship Changes…"
          onSelect={() => {
            showDropdown = false;
            showShip = true;
          }}
        />
        {#if isWorktree}
          <MenuDivider />
          <MenuItem
            label="Remove Worktree"
            variant="danger"
            onSelect={() => {
              showDropdown = false;
              showRemoveWorktreeConfirm = true;
            }}
          />
        {/if}
      </Menu>
    {/snippet}
  </Popover>

  <CommitDialog {pane} open={showCommit} onClose={handleCommitClose} />

  <ShipChangesDrawer
    {pane}
    open={showShip}
    onClose={() => {
      showShip = false;
      void refreshStatusNow();
    }}
  />

  <ConfirmDialog
    open={showRemoveWorktreeConfirm}
    title="Remove worktree"
    description="This will remove the git worktree for this thread. The branch will be preserved but the working directory will be deleted."
    confirmLabel="Remove"
    destructive={true}
    onConfirm={() => {
      showRemoveWorktreeConfirm = false;
      void guard(() => runRemoveWorktreeAction(ctx()));
    }}
    onCancel={() => {
      showRemoveWorktreeConfirm = false;
    }}
  />
{/if}
