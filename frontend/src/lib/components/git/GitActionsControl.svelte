<script lang="ts">
  // Split-button control: primary action on the left, caret opens a menu
  // with the remaining git actions. The dropdown composes the Popover +
  // Menu + MenuItem primitives so it inherits portaling, arrow-key nav,
  // typeahead, and focus management from the shared implementation.
  //
  // Status freshness model: this control does NOT own a subscription. It reads
  // `pane.gitStatus`, a view onto the shared workspace-keyed git-status store
  // — subscribed/retried/pushed once per workspace, shared with the header's
  // diff/PR badges and every other pane on the same worktree. After a git
  // action completes it asks for a one-shot `refreshNow()` so the button label
  // catches up immediately instead of waiting on the ~250ms fs-watcher
  // debounce; the backend pushes that same refresh to the other panes.

  import { onMount, onDestroy } from 'svelte';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { forgeLabels } from '../../utils/forgeLabels';
  import { handleExternalURL, safeExternalURL } from '../../utils/externalLinks';
  import { OPEN_SHIP_CHANGES_EVENT } from '../../stores/eventNames';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import LazyOverlay from '../primitives/LazyOverlay.svelte';
  import Popover from '../primitives/Popover.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import Icon from '../primitives/Icon.svelte';
  import Button from '../primitives/Button.svelte';
  import { SPLIT_BTN_BASE } from '../primitives/splitButton';
  import { createWorkspaceChangeLockState } from '../../stores/workspaceChangeLock.svelte';
  import { hasScope } from '../../transport/scopes';
  import {
    primaryActionFor,
    runCreatePRAction,
    runPullAction,
    runPushAction,
    runRemoveWorktreeAction,
    type GitActionCtx,
  } from './gitActions';

  let { pane }: { pane: ThreadPane } = $props();

  // Removing this thread's own worktree reattaches it to the project root —
  // the same self-move the EnvPicker blocks while the thread is busy. Gate
  // the menu item on the same lock state so the affordance matches instead
  // of letting the click through to a backend refusal.
  const workspaceLock = createWorkspaceChangeLockState(() => pane);

  // Split-button chrome (both segments, 24px height, header-cluster font +
  // focus ring) lives in ../primitives/splitButton so the Open-in-editor
  // control shares the exact same base — see SPLIT_BTN_BASE there for why the
  // height is load-bearing. Per-segment padding / rounded corners / middle
  // border are appended at each use site below.

  // Live status for this pane's workspace. Reading through $derived keeps
  // this control reactive to the shared entry without owning it.
  let status = $derived(pane.gitStatus.status);
  // Null when healthy; the message itself when the workspace's stream is
  // failing, so the retry button can say what went wrong.
  let statusError = $derived(pane.gitStatus.statusError);

  // Every action here rides `git:operate` — commit, push, pull, PR, ship,
  // remove-worktree. A session without it never gets a git status either
  // (the store's own guard), so this control is normally absent rather than
  // inert; the gate is what keeps every action honest if a status ever
  // arrives without the grant to act on it.
  let gitUngranted = $derived(!hasScope('git:operate'));

  let actionLoading = $state(false);

  let showCommit = $state(false);
  let showShip = $state(false);
  let showDropdown = $state(false);
  let showRemoveWorktreeConfirm = $state(false);

  let menuTriggerEl: HTMLButtonElement | undefined = $state(undefined);

  function handleOpenShip(event: Event): void {
    const detail = (event as CustomEvent<{ paneId?: string }>).detail;
    if (detail?.paneId && detail.paneId !== pane.paneId) return;
    if (pane.threadId) showShip = true;
  }

  onMount(() => {
    window.addEventListener(OPEN_SHIP_CHANGES_EVENT, handleOpenShip);
  });

  onDestroy(() => {
    if (typeof window !== 'undefined') {
      window.removeEventListener(OPEN_SHIP_CHANGES_EVENT, handleOpenShip);
    }
  });

  let isWorktree = $derived(!!pane.thread?.worktreePath);
  // Forge-aware labels for the menu item. Falls back to GitHub strings
  // when status.forge is empty (no origin or unsupported host) — the
  // canCreatePR gate keeps the action disabled in that case.
  let labels = $derived(forgeLabels(status?.forge));
  let hasOpenPR = $derived(!!status?.openPrUrl);
  let openPRURL = $derived(safeExternalURL(status?.openPrUrl));
  let prMenuLabel = $derived(hasOpenPR ? labels.openAction : labels.createAction);
  let prLookupError = $derived(status?.openPrLookupError?.trim() ?? '');
  let canCreatePR = $derived(
    status !== null &&
      status.hasUpstream &&
      !hasOpenPR &&
      prLookupError === '' &&
      !status.isDefaultBranch &&
      // Disable when no recognised forge — we have nothing to dispatch to.
      status.forge !== '' &&
      status.forge !== undefined,
  );

  let primaryAction = $derived(primaryActionFor(status));

  function ctx(): GitActionCtx {
    return {
      threadId: pane.threadId!,
      reportError: (msg) => pane.setGeneralError(msg),
      refreshStatus: () => pane.gitStatus.refreshNow(),
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

  function closeMenu(reason?: PopoverCloseReason): void {
    showDropdown = false;
    restorePickerFocus(reason, { triggerEl: menuTriggerEl });
  }

  function handleCommitClose() {
    showCommit = false;
    void pane.gitStatus.refreshNow();
  }
</script>

{#if statusError}
  <Button
    variant="danger-outline"
    size="xs"
    onclick={() => void pane.gitStatus.refreshNow()}
    testId="git-actions-error"
    title={`Failed to load git status: ${statusError}. Click to retry.`}
  >
    {#snippet children()}Git: error{/snippet}
  </Button>
{:else if status && status.isRepo}
  <div class="flex">
    <button
      onclick={executePrimary}
      disabled={primaryAction.disabled || actionLoading || gitUngranted}
      title={gitUngranted ? 'Not granted to this device' : primaryAction.tooltip}
      class="{SPLIT_BTN_BASE} px-2.5 rounded-l disabled:opacity-40 disabled:cursor-not-allowed"
    >
      {actionLoading ? '...' : primaryAction.label}
    </button>
    <button
      bind:this={menuTriggerEl}
      onclick={() => (showDropdown = !showDropdown)}
      aria-label="More git actions"
      aria-expanded={showDropdown}
      aria-haspopup="menu"
      class="{SPLIT_BTN_BASE} px-1 rounded-r border-l-0"
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
          disabled={!menuStatus.hasChanges || gitUngranted}
          onSelect={() => {
            showDropdown = false;
            showCommit = true;
          }}
        />
        <MenuItem
          label="Push"
          disabled={menuStatus.aheadCount === 0 || gitUngranted}
          onSelect={() => {
            showDropdown = false;
            void guard(() => runPushAction(ctx()));
          }}
        />
        <MenuItem
          label="Pull"
          disabled={menuStatus.behindCount === 0 || gitUngranted}
          onSelect={() => {
            showDropdown = false;
            void guard(() => runPullAction(ctx()));
          }}
        />
        <MenuItem
          label={prMenuLabel}
          description={prLookupError && !hasOpenPR ? `Could not check existing ${labels.noun}: ${prLookupError}` : undefined}
          title={
            hasOpenPR && !openPRURL
              ? `Invalid ${labels.longSingular} URL`
              : prLookupError
                ? `Could not check existing ${labels.longSingular}: ${prLookupError}`
                : undefined
          }
          disabled={gitUngranted ? !hasOpenPR : (hasOpenPR ? openPRURL === null : !canCreatePR)}
          onSelect={() => {
            showDropdown = false;
            if (openPRURL) {
              void handleExternalURL(openPRURL);
            } else {
              void guard(() => runCreatePRAction(ctx()));
            }
          }}
        />
        <MenuDivider />
        <MenuItem
          label="Ship Changes…"
          disabled={gitUngranted}
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
            disabled={workspaceLock.locked || gitUngranted}
            title={gitUngranted ? 'Not granted to this device' : workspaceLock.locked ? workspaceLock.reason : undefined}
            onSelect={() => {
              showDropdown = false;
              showRemoveWorktreeConfirm = true;
            }}
          />
        {/if}
      </Menu>
    {/snippet}
  </Popover>

  <LazyOverlay
    load={() => import('./CommitDialog.svelte')}
    active={showCommit}
    props={{ pane, open: showCommit, onClose: handleCommitClose }}
  />

  <LazyOverlay
    load={() => import('./ShipChangesDrawer.svelte')}
    active={showShip}
    props={{
      pane,
      open: showShip,
      onClose: () => {
        showShip = false;
        void pane.gitStatus.refreshNow();
      },
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
