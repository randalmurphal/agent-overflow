<script lang="ts">
  // Branch trigger + list for the below-composer bar. The trigger
  // doubles as a "from <base>" picker whenever the worktree intent
  // has the creating-branch flag set: in that quadrant the rows are a
  // base picker, not a checkout target. Otherwise behavior splits by
  // workspace mode:
  //
  //   - mode='local':         pick a row → checkout that branch, or the
  //     tag detached
  //   - mode='new-worktree':  pick a row → stage as the worktree's
  //     attach target. Branches already checked out in another worktree
  //     are disabled here; workspace selection belongs to EnvPicker.
  //
  // Rows carry wire refs (utils/gitRef.ts), so a tag travels as its tag ref
  // to every binding and into the intent and MRU. Tags load only from a
  // backend with `git-tags.v1` and only while the menu is open.
  //
  // A staged base of '' is the current checkout: the trigger names it and
  // its row is the selected one.
  //
  // The "+ New branch…" entry inside the dropdown is the local-mode
  // entry point into the create-branch flow; the inline "+ new branch"
  // button next to BranchPicker (in WorktreeNameInput.svelte) is the
  // new-worktree-mode entry point. Both call startNewBranch and
  // surface the same WorktreeNameInput text input above the strip.

  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Plus from '@lucide/svelte/icons/plus';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import X from '@lucide/svelte/icons/x';
  import Icon from '../../primitives/Icon.svelte';
  import SteppedSpinner from '../../primitives/SteppedSpinner.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { GitBranch, WorkspaceRef } from '../../../types/git';
  import { addToast } from '../../../stores/toast.svelte';
  import { recentBranchSelections, recordBranchSelection } from '../../../stores/branchMru';
  import { sameNormalizedPath } from '../../../utils/path';
  import {
    exitCreateBranchMode,
    isLocalBase,
    LOCAL_BASE_SENTINEL,
    setAttachBranch,
    setNewBranchBase,
    startNewBranch,
    worktreeIntentForThread,
  } from '../../../stores/worktreeIntent.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuDivider from '../../primitives/MenuDivider.svelte';
  import ConfirmDialog from '../../shared/ConfirmDialog.svelte';
  import BranchPruneDialog from './BranchPruneDialog.svelte';
  import { registerComposerPicker } from '../../../stores/composerPickerRegistry.svelte';
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';
  import { chordHintSuffix } from '../../../stores/keybindings.svelte';
  import {
    checkoutDisplay,
    checkoutIcon,
    paneCheckout,
    refCheckout,
    type CheckoutKind,
  } from '../../../utils/checkout';
  import { orderPickerRows, type PickerRow } from './branchPickerRows';
  import { createPickerTags, workspaceHasTags } from './branchPickerTags.svelte';
  import { checkoutPickerRow } from './branchPickerCheckout';
  import {
    branchUsesAnotherWorktree,
    ROW_LABEL_MAX_CHARS,
    rowSuffix,
    truncateRowLabel,
  } from './branchRowFacts';
  import { createBranchSync } from './branchPickerSync.svelte';
  import { createPickerBranches } from './branchPickerBranches.svelte';

  interface Props {
    pane: ThreadPane;
    anchor?: HTMLElement;
    hideTrigger?: boolean;
  }

  let { pane, anchor, hideTrigger = false }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let query = $state('');
  let applying = $state(false);
  // Non-null while the prune preview dialog is up; captures the WORKSPACE
  // at open so a pane switch mid-dialog can't retarget the deletion.
  let pruneDialogWorkspace: WorkspaceRef | null = $state(null);
  const pickerTags = createPickerTags();
  const pickerBranches = createPickerBranches({
    pane: () => pane,
    checkoutBranch: () => checkout.branch,
    isOpen: () => open,
    onFetched: (ws) => pickerTags.reload(ws),
  });
  const branchSync = createBranchSync({ pane: () => pane, onBranches: pickerBranches.replace });

  // One read for every pane. A draft placeholder holds a git-status
  // subscription of its own now (ChatHeaderActions attaches on the
  // WORKSPACE, not on a thread row), so there is no second, one-shot
  // project-root read to keep in sync with this one.
  let workspaceDirty = $derived(!!pane.gitStatus.status?.hasChanges);
  // The checkout this picker acts on. Null disables the trigger outright.
  let workspace = $derived(pane.workspace);

  // What the workspace is on: live status once loaded, else the row's branch.
  let checkout = $derived(paneCheckout(pane));
  let currentDisplay = $derived(checkoutDisplay(checkout));
  let currentWorkspace = $derived(pane.thread?.workspacePath ?? '');
  let intent = $derived(worktreeIntentForThread(pane.thread));

  // The ref staged by the active flow, or '' for the current checkout:
  //   - creatingBranch:        the base (rows set the base)
  //   - new-worktree, !creating: the attach target (rows attach)
  //   - local, !creating:      nothing staged (rows checkout)
  let stagedRef = $derived.by(() => {
    if (intent.creatingBranch) return isLocalBase(intent.newBranchBase) ? '' : intent.newBranchBase;
    if (intent.mode === 'new-worktree') return intent.attachBranch;
    return '';
  });
  let triggerDisplay = $derived(stagedRef ? checkoutDisplay(refCheckout(stagedRef)) : currentDisplay);
  let triggerLabel = $derived.by(() => {
    if (intent.creatingBranch) {
      if (isLocalBase(intent.newBranchBase)) return 'From Base (with changes)';
      return `From ${triggerDisplay.name || 'branch'}`;
    }
    return triggerDisplay.label;
  });
  export function label(): string { return triggerLabel; }
  /** What the trigger names; picks its icon (`checkoutIcon`). */
  export function kind(): CheckoutKind { return triggerDisplay.kind; }
  export function title(): string { return triggerDisplay.title; }
  export function openPicker(): void {
    if (!open && pane.thread && !applying) void handleTrigger();
  }
  let pickerChordSuffix = $derived(chordHintSuffix('composer.picker.branch'));

  let tagsSupported = $derived(workspaceHasTags(workspace));
  let showRefreshTags = $derived(tagsSupported && !!pane.gitStatus.status?.hasOriginRemote);

  // Recent picks, as wire refs, read once per open.
  let mruRefs: string[] = $state([]);
  let pickerRows = $derived(orderPickerRows({
    branches: pickerBranches.branches,
    tags: pickerTags.tags,
    recent: mruRefs,
    query,
  }));

  function refreshMru(): void {
    mruRefs = recentBranchSelections(pane.thread?.projectId ?? '');
  }

  // Local-row only makes sense as a base picker — the user is choosing
  // what their NEW branch is built off. Outside the create flow it would
  // suggest "checkout the local working copy", which isn't a thing.
  let showLocalRow = $derived(workspaceDirty && intent.creatingBranch);

  let isLocalSelected = $derived(intent.creatingBranch && isLocalBase(intent.newBranchBase));

  async function handleTrigger(): Promise<void> {
    open = !open;
    if (!open) {
      pickerTags.clear();
      return;
    }
    refreshMru();
    await pickerBranches.open();
  }

  // Tags belong to the repository, not the checked-out ref: the open
  // picker follows its workspace.
  $effect(() => {
    if (open && workspace) pickerTags.ensure(workspace);
  });

  function handleRefreshTags(): void {
    if (workspace) void pickerTags.refresh(workspace);
  }

  function handlePrune(): void {
    if (!workspace) return;
    pruneDialogWorkspace = workspace;
    closeMenu();
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    open = false;
    query = '';
    pickerTags.clear();
    restorePickerFocus(reason, { paneId: pane.paneId, triggerEl: anchor ?? triggerEl });
  }

  $effect(() => {
    return registerComposerPicker(pane.paneId, 'branch', {
      isOpen: () => open,
      open: () => {
        openPicker();
      },
      close: () => {
        if (!open) return;
        closeMenu();
      },
    });
  });

  function branchUnavailableReason(branch: GitBranch): string | undefined {
    if (intent.creatingBranch) return undefined;
    if (!branchUsesAnotherWorktree(branch, currentWorkspace)) return undefined;
    return `Branch is checked out in ${branch.worktreePath}. Select that worktree from the environment picker.`;
  }

  function isSelectedBranch(branch: GitBranch): boolean {
    if (branch.name !== checkout.branch) return false;
    if (branch.worktreePath) return sameNormalizedPath(branch.worktreePath, currentWorkspace);
    return true;
  }

  function isCheckedOut(row: PickerRow): boolean {
    if (row.kind === 'tag') return row.name === checkout.tag;
    return isSelectedBranch(row.branch);
  }

  // Highlight reflects the active flow's target:
  //   - creating: the row that is the staged base ('' is the checked-out row)
  //   - new-worktree, !creating: the row that is the staged attach target
  //   - local, !creating: the row that's currently checked out
  function isRowSelected(row: PickerRow): boolean {
    if (intent.creatingBranch) return intent.newBranchBase ? row.ref === intent.newBranchBase : isCheckedOut(row);
    if (intent.mode === 'new-worktree') return row.ref === intent.attachBranch;
    return isCheckedOut(row);
  }

  // Rows disable purely on selection availability; MenuItem's action is
  // independent of row `disabled`, so a sync icon on a checked-out-elsewhere
  // branch stays clickable. selectRow re-checks as backstop. A tag is never
  // unavailable: any number of worktrees may sit on one.
  function rowUnavailableReason(row: PickerRow): string | undefined {
    return row.kind === 'branch' ? branchUnavailableReason(row.branch) : undefined;
  }

  function rowTitle(row: PickerRow): string | undefined {
    const nameTitle = row.name.length > ROW_LABEL_MAX_CHARS ? row.name : '';
    const unavailableReason = rowUnavailableReason(row) ?? '';
    if (nameTitle && unavailableReason) return `${nameTitle}\n${unavailableReason}`;
    return nameTitle || unavailableReason || undefined;
  }

  function handleSearchKeydown(event: KeyboardEvent): void {
    event.stopPropagation();
  }

  function startCreate(): void {
    startNewBranch(pane);
    closeMenu();
  }

  function cancelCreate(): void {
    if (!pane.thread) return;
    exitCreateBranchMode(pane.thread);
    closeMenu();
  }

  function selectLocalRow(): void {
    if (!pane.thread) return;
    if (!intent.creatingBranch) return;
    setNewBranchBase(pane.thread, LOCAL_BASE_SENTINEL);
    closeMenu();
  }

  async function selectRow(row: PickerRow): Promise<void> {
    if (!pane.thread) {
      closeMenu();
      return;
    }
    if (intent.creatingBranch) {
      setNewBranchBase(pane.thread, row.ref);
      closeMenu();
      return;
    }
    if (applying) {
      closeMenu();
      return;
    }
    const unavailableReason = rowUnavailableReason(row);
    if (unavailableReason) {
      addToast('error', unavailableReason);
      closeMenu();
      return;
    }
    if (intent.mode === 'new-worktree') {
      setAttachBranch(pane.thread, row.ref);
      recordBranchSelection(pane.thread.projectId ?? '', row.ref);
      closeMenu();
      return;
    }
    if (isCheckedOut(row)) {
      closeMenu();
      return;
    }
    const ws = workspace;
    if (!ws) {
      addToast('error', 'Project is unavailable for checkout.');
      closeMenu();
      return;
    }
    applying = true;
    try {
      await checkoutPickerRow(pane, ws, row);
    } finally {
      applying = false;
      closeMenu();
    }
  }
</script>

{#snippet syncIcon()}
  <Icon icon={RefreshCw} size={12} strokeWidth={2} />
{/snippet}

{#if !hideTrigger}
<button
  bind:this={triggerEl}
  type="button"
  onclick={handleTrigger}
  disabled={!pane.thread || applying}
  aria-haspopup="menu"
  aria-expanded={open}
  title={`${triggerDisplay.title}${pickerChordSuffix}`}
  data-testid="branch-picker-trigger"
  data-checkout-kind={triggerDisplay.kind}
  class={composerTriggerClasses}
>
  <Icon icon={checkoutIcon(triggerDisplay.kind)} size={12} strokeWidth={2} class="opacity-70" />
  <span class="truncate max-w-[160px] text-fg">{triggerLabel}</span>
  <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
</button>
{/if}

<Popover
  anchor={anchor ?? triggerEl}
  {open}
  onClose={closeMenu}
  placement="top-end"
  role="none"
>
  <Menu ariaLabel="Branches" onClose={closeMenu}>
    {#if intent.creatingBranch}
      <MenuItem
        label="Cancel new branch"
        onSelect={cancelCreate}
      >
        {#snippet icon()}
          <Icon icon={X} size={12} strokeWidth={2} />
        {/snippet}
      </MenuItem>
    {:else}
      <MenuItem
        label="New branch…"
        onSelect={startCreate}
      >
        {#snippet icon()}
          <Icon icon={Plus} size={12} strokeWidth={2} />
        {/snippet}
      </MenuItem>
    {/if}
    <MenuItem
      label="Prune branches…"
      disabled={workspace === null}
      onSelect={handlePrune}
    >
      {#snippet icon()}
        <Icon icon={Trash2} size={12} strokeWidth={2} />
      {/snippet}
    </MenuItem>
    <MenuDivider />
    <div class="px-2 pb-1">
      <input
        type="search"
        value={query}
        placeholder={tagsSupported ? 'Search branches and tags' : 'Search Branches'}
        onkeydown={handleSearchKeydown}
        oninput={(e) => (query = (e.target as HTMLInputElement).value)}
        class={[
          'h-7 w-72 rounded border border-border-subtle bg-surface-0',
          'px-2 text-xs text-text-primary placeholder:text-fg-hint',
          'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        ].join(' ')}
      />
    </div>
    {#if pickerBranches.loading}
      <div
        class="px-3 py-1.5 text-xs text-text-secondary/60"
        role="presentation"
        data-testid="branch-picker-loading"
      >
        Loading branches…
      </div>
    {:else}
      {#if showLocalRow}
        <MenuItem
          label="Base (with changes)"
          description={currentDisplay.kind === 'none' ? undefined : currentDisplay.label}
          checked={isLocalSelected}
          onSelect={selectLocalRow}
        />
        <MenuDivider />
      {/if}
      {#if pickerBranches.error}
        <div
          class="line-clamp-3 max-w-72 break-words px-3 py-1.5 text-xs text-error"
          role="presentation"
          title={pickerBranches.error}
          data-testid="branch-picker-error"
        >
          Failed to load branches: {pickerBranches.error}
        </div>
      {:else if pickerRows.rows.length === 0 && !pickerTags.error}
        <div
          class="px-3 py-1.5 text-xs text-text-secondary/60"
          role="presentation"
          data-testid="branch-picker-empty"
        >
          {tagsSupported ? 'No branches or tags' : 'No branches'}
        </div>
      {/if}
      {#if pickerRows.rows.length > 0 || pickerTags.error || showRefreshTags}
        <!-- Refresh is the list's last row, so it costs no menu height. -->
        <div class="max-h-56 overflow-y-auto" data-testid="branch-picker-list">
          {#each pickerRows.rows as row (row.ref)}
            <MenuItem
              label={truncateRowLabel(row.name)}
              suffix={rowSuffix(row, currentWorkspace)}
              checked={isRowSelected(row)}
              disabled={!!rowUnavailableReason(row)}
              title={rowTitle(row)}
              testId={row.kind === 'tag' ? 'branch-picker-tag-row' : undefined}
              onSelect={() => selectRow(row)}
              action={branchSync.shows(row) ? syncIcon : undefined}
              actionLabel={branchSync.actionLabel(row)}
              actionDisabled={branchSync.actionDisabled(row)}
              actionTitle={branchSync.actionTitle(row)}
              onAction={() => void branchSync.request(row, closeMenu)}
            />
          {/each}
          {#if pickerTags.error}
            <div
              class="line-clamp-3 max-w-72 break-words px-3 py-1.5 text-xs text-error"
              role="presentation"
              title={pickerTags.error}
              data-testid="branch-picker-tags-error"
            >
              Failed to load tags: {pickerTags.error}
            </div>
          {/if}
          {#if pickerRows.hiddenTags > 0}
            <div
              class="px-3 py-1.5 text-xs text-fg-hint"
              role="presentation"
              data-testid="branch-picker-more-tags"
            >
              {pickerRows.hiddenTags} more {pickerRows.hiddenTags === 1 ? 'tag' : 'tags'}. Search to narrow.
            </div>
          {/if}
          {#if showRefreshTags}
            <MenuItem
              label={pickerTags.refreshing ? 'Refreshing tags…' : 'Refresh tags from origin'}
              disabled={pickerTags.refreshing || workspace === null}
              testId="branch-picker-refresh-tags"
              onSelect={handleRefreshTags}
            >
              {#snippet icon()}
                {#if pickerTags.refreshing}
                  <SteppedSpinner size={12} />
                {:else}
                  <Icon icon={RefreshCw} size={12} strokeWidth={2} />
                {/if}
              {/snippet}
            </MenuItem>
          {/if}
        </div>
      {/if}
    {/if}
  </Menu>
</Popover>

<ConfirmDialog
  open={!!branchSync.confirmation}
  title="Sync Worktree Branch"
  description={branchSync.confirmation
    ? `${branchSync.confirmation.branchName} is checked out in ${branchSync.confirmation.targetWorkspace}. Syncing it will run git pull in that worktree.`
    : ''}
  confirmLabel="Sync"
  onConfirm={branchSync.confirm}
  onCancel={branchSync.cancel}
/>

{#if pruneDialogWorkspace}
  <BranchPruneDialog
    workspace={pruneDialogWorkspace}
    open={true}
    onClose={() => { pruneDialogWorkspace = null; }}
  />
{/if}
