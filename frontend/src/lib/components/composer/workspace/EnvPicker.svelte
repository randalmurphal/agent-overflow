<script lang="ts">
  // Workspace trigger in the below-composer bar. Lists the project root,
  // staged new-worktree intent, and registered worktrees so the user can
  // choose where the next provider turn runs without leaving the chat.
  //
  // Choosing a worktree is durable draft state. A placeholder materializes
  // as soon as the user selects New Worktree or an existing worktree, so the
  // checkout has a thread owner and every workspace-keyed surface can follow
  // it before the first message. Worktree cleanup happens inline: a trash
  // icon on each row morphs that row into a confirmation strip.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { Thread } from '../../../types/models';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Folder from '@lucide/svelte/icons/folder';
  import FolderGit2 from '@lucide/svelte/icons/folder-git-2';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import {
    GitListWorktrees,
    GitWorktreeStatus,
    RemoveOtherWorktree,
    UpdateThreadWorkspace,
    WorktreeStatus,
    type WorktreeListItem,
  } from '../../../stores/bindings';
  import { syncThreadReply } from '../../../stores/panes.svelte';
  import { threadRowFrameMark } from '../../../stores/threadRowFrames';
  import { placeholderWorkspaceOf } from '../../../stores/draftWorkspaceSync';
  import {
    syncRemovedWorktreeThreads,
    terminalsClosingNote,
    type WorktreeRemovalResult,
  } from '../../../stores/worktreeRemoval.svelte';
  import { addErrorToast, addToast } from '../../../stores/toast.svelte';
  import { userFacingError } from '../../../utils/userFacingError';
  import { sameNormalizedPath } from '../../../utils/path';
  import { workspaceRefForProject } from '../../../utils/workspaceKey';
  import { disambiguatedPathLabels, formatPathLabel, pathBasename } from '../../../utils/pathDisplay';
  import {
    clearWorktreeIntent,
    setThreadEnvMode,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';
  import type { WorkspaceChangeLockState } from '../../../stores/workspaceChangeLock.svelte';
  import { wailsEventOn } from '../../../stores/wailsEvents';
  import {
    createRefreshScheduler,
    type RefreshScheduler,
    type RefreshToken,
  } from '../../../utils/refreshScheduler';
  import Popover from '../../primitives/Popover.svelte';
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import Button from '../../primitives/Button.svelte';
  import { checkoutDisplay, checkoutIcon, type CheckoutDisplay } from '../../git/checkoutDisplay';

  interface Props {
    pane: ThreadPane;
    workspaceLock: WorkspaceChangeLockState;
    anchor?: HTMLElement;
    hideTrigger?: boolean;
  }

  interface ConfirmState {
    path: string;
    label: string;
    ref: CheckoutDisplay | null;
    status: WorktreeStatus | null;
    loading: boolean;
    pending: boolean;
    error: string | null;
  }

  let { pane, workspaceLock, anchor, hideTrigger = false }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let worktrees: WorktreeListItem[] = $state([]);
  let loading = $state(false);
  // Why the last list read failed, until a read succeeds.
  let listError: string | null = $state(null);
  let applying = $state(false);
  let confirm: ConfirmState | null = $state(null);

  // The checkout this picker lists worktrees of and removes them from.
  let workspace = $derived(pane.workspace);
  let projectPath = $derived(pane.thread?.projectPath ?? '');
  let currentWorkspace = $derived(pane.thread?.workspacePath ?? '');
  let isAtProjectRoot = $derived(sameNormalizedPath(currentWorkspace, projectPath));
  let intent = $derived(worktreeIntentForThread(pane.thread));

  // Trigger reflects *where you are*, not the picker's mode. "Base"
  // sits at the project root; the worktree's basename otherwise. Staged
  // new-worktree intent overrides both so the user sees that the next
  // send will run somewhere new.
  let stagingNewWorktree = $derived(intent.mode === 'new-worktree');
  let triggerLabel = $derived.by(() => {
    if (stagingNewWorktree) return 'New Worktree';
    if (isAtProjectRoot) return 'Base';
    return pathBasename(currentWorkspace) || 'Worktree';
  });
  let triggerIcon = $derived.by(() => {
    if (stagingNewWorktree) return FolderGit2;
    return isAtProjectRoot ? Folder : FolderGit2;
  });
  let triggerIconName = $derived.by(() => {
    if (stagingNewWorktree) return 'folder-git-2';
    return isAtProjectRoot ? 'folder' : 'folder-git-2';
  });

  // Every row here MOVES this thread; none mutates the directory it leaves.
  // So the gate is the thread view of the lock, not the directory view: a
  // sibling thread responding at the project root must not pin this one
  // there. The per-row trash is directory-destructive and is gated by the
  // backend's own per-row `deleteBlocked` instead.
  let disabledReason = $derived(workspaceLock.threadReason);
  let workspaceChangingDisabled = $derived(workspaceLock.threadLocked);

  function handleTrigger(): void {
    open = !open;
    if (!open) {
      confirm = null;
      return;
    }
    // The placeholder only shows for the open's first fetch; the live
    // refreshes below swap the list in place. The fetch itself is the
    // scheduler's opening request, in the effect below.
    loading = true;
  }

  export function openPicker(): void {
    if (!open && pane.thread && !applying) handleTrigger();
  }

  export function label(): string { return triggerLabel; }
  export function atBase(): boolean { return isAtProjectRoot && !stagingNewWorktree; }

  // Every refresh — the open's own load, the event-driven re-checks, and the
  // one after a removal — goes through this, so only one list call is ever in
  // flight and a late answer is inert. The scheduler's token replaced a
  // hand-rolled sequence number that could not see the popover close.
  let refresh: RefreshScheduler | null = null;
  // While a removal is in flight this pane's own workspace may be the path
  // being deleted, and its row only moves when the removal answers. Refreshes
  // wait for that answer rather than list from a path that is going away.
  let removing = false;
  let refreshHeld = false;

  async function refreshWorktreeList(token: RefreshToken): Promise<void> {
    // The list is the project's, so it is read from the project root. The
    // pane's own workspace can name a worktree a removal just deleted until
    // its row catches up, and the backend refuses a path that is gone.
    const ws = workspaceRefForProject(workspace?.projectId, '');
    if (!ws) return;
    if (removing) {
      refreshHeld = true;
      return;
    }
    try {
      const res = (await GitListWorktrees(ws)) as WorktreeListItem[] | null;
      if (!token.isCurrent()) return;
      if (removing) {
        refreshHeld = true;
        return;
      }
      worktrees = Array.isArray(res) ? res : [];
      listError = null;
    } catch (err) {
      console.error('GitListWorktrees failed:', err);
      if (!token.isCurrent()) return;
      if (removing) {
        refreshHeld = true;
        return;
      }
      // Delete gating stays conservative on a failure: no rows means no
      // trash affordance to click, rather than rows carrying a deleteBlocked
      // flag nothing has verified. The menu says why the list is empty.
      worktrees = [];
      listError = userFacingError(err);
    } finally {
      if (token.isCurrent()) loading = false;
    }
  }

  // deleteBlocked is a point-in-time flag: a turn starting or ending
  // anywhere in the project flips it, and this popover can sit open
  // across that. Re-fetch on the live activity signals so the rows never
  // go stale in either direction. 250ms coalesces the per-wire-round turn
  // events; the 1s deadline is what a trailing debounce could not give —
  // under a pane that streams without pausing, those events never stop
  // arriving and the rows never refreshed at all.
  $effect(() => {
    if (!open) return;
    const scheduler = createRefreshScheduler({
      name: 'EnvPicker',
      delayMs: 250,
      maxWaitMs: 1_000,
      run: refreshWorktreeList,
    });
    refresh = scheduler;
    const request = (): void => scheduler.request();
    const cancels = [
      wailsEventOn('provider:turn_started', request),
      wailsEventOn('provider:turn_completed', request),
      wailsEventOn('provider:background_tasks_changed', request),
      wailsEventOn('provider:background_task_state', request),
    ];
    // The open's first fetch, through the same scheduler as everything else.
    scheduler.request({ immediate: true });
    return () => {
      scheduler.dispose();
      if (refresh === scheduler) refresh = null;
      for (const cancel of cancels) cancel();
    };
  });

  function closeMenu(reason?: PopoverCloseReason): void {
    open = false;
    confirm = null;
    restorePickerFocus(reason, { triggerEl: anchor ?? triggerEl });
  }

  async function selectPath(path: string): Promise<void> {
    if (!pane.thread || applying) return;
    if (workspaceChangingDisabled) return;
    const threadId = pane.thread.id;
    const selectingProjectRoot = sameNormalizedPath(path, projectPath);
    if (selectingProjectRoot || pane.hasDraftPlaceholder) {
      setThreadEnvMode(pane.thread, 'local');
    } else {
      clearWorktreeIntent(threadId);
    }
    if (
      sameNormalizedPath(path, currentWorkspace) &&
      (!pane.hasDraftPlaceholder || selectingProjectRoot)
    ) {
      closeMenu();
      return;
    }
    if (pane.hasDraftPlaceholder) {
      const worktree = worktrees.find((candidate) => sameNormalizedPath(candidate.path, path));
      const placeholderId = pane.draftPlaceholder?.id ?? '';
      const previous = {
        workspacePath: pane.thread.workspacePath ?? projectPath,
        worktreePath: pane.thread.worktreePath ?? '',
        branch: pane.thread.branch ?? '',
      };
      pane.applyDraftPlaceholderWorkspace({
        workspacePath: path,
        worktreePath: sameNormalizedPath(path, projectPath) ? '' : path,
        branch: worktree?.branch,
      });
      applying = true;
      try {
        const createdId = await pane.ensureMaterializedThread();
        if (!createdId && pane.draftPlaceholder?.id === placeholderId) {
          pane.applyDraftPlaceholderWorkspace(previous);
        } else if (createdId) {
          clearWorktreeIntent(createdId);
        }
      } finally {
        applying = false;
        closeMenu();
      }
      return;
    }
    applying = true;
    try {
      const mark = threadRowFrameMark();
      const updated = (await UpdateThreadWorkspace(threadId, path)) as Thread;
      syncThreadReply(updated, mark);
      addToast('info', `Workspace switched to ${pathBasename(path) || path}`);
    } catch (err) {
      console.error('UpdateThreadWorkspace failed:', err);
      addErrorToast(userFacingError(err), err);
    } finally {
      applying = false;
      closeMenu();
    }
  }

  async function selectNewWorktree(): Promise<void> {
    if (!pane.thread) return;
    if (workspaceChangingDisabled) return;
    setThreadEnvMode(pane.thread, 'new-worktree');
    if (!pane.hasDraftPlaceholder) {
      closeMenu();
      return;
    }
    applying = true;
    try {
      await pane.ensureMaterializedThread();
    } finally {
      applying = false;
      closeMenu();
    }
  }

  function directoryName(wt: WorktreeListItem): string {
    return pathBasename(wt.path) || wt.path;
  }

  // The project root has its own Base row above the list.
  let linkedWorktrees = $derived(worktrees.filter((wt) => !sameNormalizedPath(wt.path, projectPath)));

  // A row is named by its directory. Two worktrees in same-named directories
  // gain the parent segments that tell them apart.
  let nameLabels = $derived(disambiguatedPathLabels(
    linkedWorktrees.map((wt) => ({ id: wt.path, name: directoryName(wt), path: wt.path })),
  ));

  function worktreeName(wt: WorktreeListItem): string {
    const label = nameLabels.get(wt.path);
    return label ? formatPathLabel(label) : directoryName(wt);
  }

  // What a worktree is checked out on: its branch, else the tag at its
  // detached HEAD, else the detached commit.
  function worktreeCheckout(wt: WorktreeListItem): CheckoutDisplay | null {
    const display = checkoutDisplay({
      branch: wt.branch,
      tag: wt.tag,
      headCommit: wt.branch ? '' : wt.head,
    });
    return display.kind === 'none' ? null : display;
  }

  // Worktree directories are named after their branch or tag by default, so
  // the checkout leads the row only when it differs from the directory name.
  // A tag of the same name still shows as the trailing tag icon: it is the
  // only sign the worktree is on no branch.
  function leadingRef(wt: WorktreeListItem): CheckoutDisplay | null {
    const ref = worktreeCheckout(wt);
    return ref && ref.name !== directoryName(wt) ? ref : null;
  }

  function trailingTag(wt: WorktreeListItem): boolean {
    return worktreeCheckout(wt)?.kind === 'tag' && !leadingRef(wt);
  }

  // A worktree git still registers whose directory is gone: nothing can run
  // there, so its row only offers the removal that clears the registration.
  const MISSING_REASON = "This worktree's folder no longer exists. Remove it to clear git's record of it.";

  function rowDisabledReason(wt: WorktreeListItem): string | undefined {
    if (wt.missing) return MISSING_REASON;
    return workspaceChangingDisabled ? disabledReason : undefined;
  }

  function worktreeRowLabel(wt: WorktreeListItem): string {
    const ref = leadingRef(wt);
    return ref ? `${ref.label} · ${worktreeName(wt)}` : worktreeName(wt);
  }

  async function requestRemove(wt: WorktreeListItem): Promise<void> {
    const ws = workspace;
    if (!ws) return;
    confirm = {
      path: wt.path,
      label: worktreeName(wt),
      ref: leadingRef(wt),
      status: null,
      loading: true,
      pending: false,
      error: null,
    };
    try {
      const status = (await GitWorktreeStatus(ws, wt.path)) as WorktreeStatus;
      // Guard against the user clicking Cancel and then opening a
      // different row's confirmation between the request and the
      // response — only apply the result if the active confirm is
      // still for this path.
      if (confirm && confirm.path === wt.path) {
        confirm = { ...confirm, status, loading: false };
      }
    } catch (err) {
      console.error('GitWorktreeStatus failed:', err);
      if (confirm && confirm.path === wt.path) {
        confirm = { ...confirm, loading: false, error: userFacingError(err) };
      }
    }
  }

  function cancelRemove(): void {
    confirm = null;
  }

  // Mirrors the backend's RemoveOtherWorktree refusal gate. "No upstream"
  // alone isn't a refusal — a freshly-created worktree off main rarely
  // has one, and treating it as risky would force the destructive button
  // path on a clean removal. Surface no-upstream as informational text in
  // riskSummary instead.
  function isRiskyStatus(s: WorktreeStatus | null): boolean {
    if (!s) return false;
    return s.dirty || s.unpushedCommits > 0 || s.orphanCommits > 0;
  }

  function riskSummary(s: WorktreeStatus | null): string {
    if (!s) return '';
    const parts: string[] = [];
    if (s.dirty) {
      parts.push(`${s.uncommittedCount} uncommitted file${s.uncommittedCount === 1 ? '' : 's'}`);
    }
    if (s.unpushedCommits > 0) {
      parts.push(`${s.unpushedCommits} unpushed commit${s.unpushedCommits === 1 ? '' : 's'}`);
    }
    if (s.orphanCommits > 0) {
      parts.push(`${s.orphanCommits} commit${s.orphanCommits === 1 ? '' : 's'} on no branch`);
    }
    if (!s.hasUpstream && s.branch) {
      parts.push('no upstream');
    }
    if (s.attachedThreads > 0) {
      parts.push(`${s.attachedThreads} thread${s.attachedThreads === 1 ? '' : 's'} attached`);
    }
    return parts.join(' · ');
  }

  async function performRemove(force: boolean): Promise<void> {
    const ws = workspace;
    if (!ws || !confirm) return;
    const path = confirm.path;
    const label = confirm.label;
    const placeholderId = pane.draftPlaceholder?.id ?? '';
    confirm = { ...confirm, pending: true, error: null };
    removing = true;
    refreshHeld = false;
    try {
      const mark = threadRowFrameMark();
      const removal = (await RemoveOtherWorktree(ws, path, force)) as WorktreeRemovalResult;
      removing = false;
      // The moved rows first: this pane's own row may be one of them, and
      // the list refresh below reads its workspace.
      syncRemovedWorktreeThreads(removal, mark);
      // Guarded, not aborted: the removal happened whatever this pane did
      // under the await. Other draft composers in the removed directory
      // are moved by the worktree:removed event.
      if (
        pane.hasDraftPlaceholder &&
        pane.draftPlaceholder?.id === placeholderId &&
        sameNormalizedPath(pane.thread?.workspacePath ?? '', ws.workspacePath)
      ) {
        pane.applyDraftPlaceholderWorkspace(placeholderWorkspaceOf(removal.workspace));
      }
      addToast('info', `Removed worktree ${label}`);
      confirm = null;
      // A list read that started before the answer describes the old tree.
      refresh?.reset();
      refresh?.request({ immediate: true });
    } catch (err) {
      removing = false;
      console.error('RemoveOtherWorktree failed:', err);
      if (confirm) {
        confirm = { ...confirm, pending: false, error: userFacingError(err) };
      }
      if (refreshHeld) refresh?.request();
    }
  }
</script>

<!-- The icon trails the ref so every worktree row's text starts at the
     same column whether or not it shows one. -->
{#snippet refIcon(kind: CheckoutDisplay['kind'])}
  <span class="ml-1 inline-flex align-[-0.125em] text-fg-hint" aria-hidden="true" data-checkout-kind={kind}><Icon icon={checkoutIcon(kind)} size={12} strokeWidth={2} /></span>
{/snippet}
{#snippet refName(ref: CheckoutDisplay)}
  {ref.label}{@render refIcon(ref.kind)}
{/snippet}

{#if !hideTrigger}
<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread || applying}
  aria-haspopup="menu"
  aria-expanded={open}
  data-trigger-icon={triggerIconName}
  data-testid="env-picker-trigger"
  class={composerTriggerClasses}
>
  <Icon icon={triggerIcon} size={12} strokeWidth={2} class="opacity-70" />
  <span class="truncate max-w-[160px] text-fg">{triggerLabel}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>
{/if}

<Popover
  anchor={anchor ?? triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-start"
  role="none"
>
  <Menu ariaLabel="Workspace" onClose={closeMenu}>
    <MenuItem
      label={projectPath ? `Base · ${pathBasename(projectPath)}` : 'Base'}
      checked={isAtProjectRoot && !stagingNewWorktree}
      disabled={!projectPath || workspaceChangingDisabled}
      title={workspaceChangingDisabled ? disabledReason : undefined}
      onSelect={() => selectPath(projectPath)}
    />
    <MenuItem
      label="New Worktree"
      checked={stagingNewWorktree}
      disabled={workspaceChangingDisabled}
      title={workspaceChangingDisabled ? disabledReason : undefined}
      onSelect={() => selectNewWorktree()}
    />
    {#if loading}
      <div
        class="px-3 py-1.5 text-xs text-text-secondary/60"
        role="presentation"
        data-testid="env-picker-loading"
      >
        Loading worktrees…
      </div>
    {:else if listError}
      <div
        class="px-3 py-1.5 text-xs text-error"
        role="presentation"
        data-testid="env-picker-list-error"
      >
        Could not list worktrees: {listError}
      </div>
    {:else if linkedWorktrees.length > 0}
      <MenuDivider />
      {#each linkedWorktrees as wt (wt.path)}
        {#if confirm && confirm.path === wt.path}
          <div
            class="px-3 py-2 text-sm"
            role="presentation"
            data-testid="env-picker-confirm-row"
          >
            <div class="text-fg truncate">
              Remove
              {#if confirm.ref}
                <span class="text-fg-hint">{@render refName(confirm.ref)} ·</span>
              {/if}
              <span class="font-medium">{confirm.label}</span>?
            </div>
            {#if confirm.loading}
              <div class="mt-1 text-[0.6875rem] text-fg-hint">Checking…</div>
            {:else if confirm.error}
              <div class="mt-1 text-[0.6875rem] text-error truncate">{confirm.error}</div>
            {:else if confirm.status}
              {#if confirm.status.missing}
                <div class="mt-1 text-[0.6875rem] text-fg-hint" data-testid="env-picker-confirm-missing">
                  Its folder is already gone. This clears git's record of it.
                </div>
              {/if}
              {#if isRiskyStatus(confirm.status)}
                <div class="mt-1 text-[0.6875rem] text-warning truncate">
                  {riskSummary(confirm.status)}
                </div>
              {:else if confirm.status.attachedThreads > 0}
                <div class="mt-1 text-[0.6875rem] text-fg-hint truncate">
                  {riskSummary(confirm.status)} — will move to project root.
                </div>
              {/if}
              {#if terminalsClosingNote(confirm.status.terminals)}
                <div class="mt-1 text-[0.6875rem] text-fg-hint truncate" data-testid="env-picker-confirm-terminals">
                  {terminalsClosingNote(confirm.status.terminals)}
                </div>
              {/if}
            {/if}
            <div class="mt-2 flex items-center justify-end gap-2">
              <Button
                variant="ghost"
                size="xs"
                onclick={cancelRemove}
                disabled={confirm.pending}
              >
                Cancel
              </Button>
              {#if confirm.status && isRiskyStatus(confirm.status)}
                <Button
                  variant="danger"
                  size="xs"
                  onclick={() => performRemove(true)}
                  disabled={confirm.pending || confirm.loading}
                  testId="env-picker-confirm-force"
                >
                  {confirm.pending ? 'Removing…' : 'Discard and remove'}
                </Button>
              {:else}
                <Button
                  variant="secondary"
                  size="xs"
                  onclick={() => performRemove(false)}
                  disabled={confirm.pending || confirm.loading}
                  testId="env-picker-confirm-remove"
                >
                  {confirm.pending ? 'Removing…' : 'Remove'}
                </Button>
              {/if}
            </div>
          </div>
        {:else}
          <MenuItem
            label={worktreeRowLabel(wt)}
            checked={sameNormalizedPath(currentWorkspace, wt.path) && !stagingNewWorktree}
            disabled={workspaceChangingDisabled || wt.missing}
            title={rowDisabledReason(wt)}
            onSelect={() => selectPath(wt.path)}
            actionLabel={`Remove worktree ${worktreeName(wt)}`}
            actionPosition="end"
            actionDisabled={workspace === null || wt.deleteBlocked}
            actionTitle={wt.deleteBlocked
              ? 'This worktree cannot be removed while an attached thread is running.'
              : `Remove worktree ${worktreeName(wt)}`}
            onAction={() => requestRemove(wt)}
          >
            {#snippet labelContent()}
              {@const ref = leadingRef(wt)}
              {#if ref}{@render refName(ref)}{' · '}{/if}{worktreeName(wt)}{#if trailingTag(wt)}{@render refIcon('tag')}{/if}
              <!-- The warning color keeps the marker legible on a dimmed row. -->
              {#if wt.missing}<span class="ml-1.5 text-xs text-warning">missing</span>{/if}
            {/snippet}
            {#snippet action()}
              {#if wt.deleteBlocked}
                <!-- Same pulsing running-dot as the sidebar: a faded
                     trash icon reads as deletable at a glance; a live
                     activity marker doesn't. -->
                <span
                  class="h-1.5 w-1.5 rounded-full bg-success animate-pulse"
                  data-testid={`env-picker-busy-${directoryName(wt)}`}
                ></span>
              {:else}
                <Icon icon={Trash2} size={12} strokeWidth={2} />
              {/if}
            {/snippet}
          </MenuItem>
        {/if}
      {/each}
    {/if}
  </Menu>
</Popover>
