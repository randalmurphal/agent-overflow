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
  // The "+ New branch…" entry inside the dropdown is the local-mode
  // entry point into the create-branch flow; the inline "+ new branch"
  // button next to BranchPicker (in WorktreeNameInput.svelte) is the
  // new-worktree-mode entry point. Both call startNewBranch and
  // surface the same WorktreeNameInput text input above the strip.

  import { untrack } from 'svelte';
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
  import { GitListBranches, GitMaybeFetchRemotes } from '../../../stores/bindings';
  import { workspaceKeyForRef } from '../../../utils/workspaceKey';
  import { addToast } from '../../../stores/toast.svelte';
  import { recentBranchSelections, recordBranchSelection } from '../../../stores/branchMru';
  import { userFacingError } from '../../../utils/userFacingError';
  import { sameNormalizedPath } from '../../../utils/path';
  import {
    exitCreateBranchMode,
    isLocalBase,
    LOCAL_BASE_SENTINEL,
    paneCheckout,
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
  import { isTagRef, refLabel } from '../../../utils/gitRef';
  import { checkoutDisplay, checkoutIcon, type CheckoutKind } from '../../git/checkoutDisplay';
  import { orderPickerRows, type PickerRow } from './branchPickerRows';
  import { createPickerTags, workspaceHasTags } from './branchPickerTags.svelte';
  import { checkoutPickerRow } from './branchPickerCheckout';
  import {
    branchUsesAnotherWorktree,
    ROW_LABEL_MAX_CHARS,
    rowSuffix,
    showsSyncAction,
    truncateRowLabel,
  } from './branchRowFacts';
  import { createBranchSync } from './branchPickerSync.svelte';

  interface Props {
    pane: ThreadPane;
    anchor?: HTMLElement;
    hideTrigger?: boolean;
  }

  let { pane, anchor, hideTrigger = false }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let branches: GitBranch[] = $state([]);
  let query = $state('');
  let loading = $state(false);
  // Why the open list is empty; cleared by the next successful list.
  let loadError = $state<string | null>(null);
  let applying = $state(false);
  // Non-null while the prune preview dialog is up; captures the WORKSPACE
  // at open so a pane switch mid-dialog can't retarget the deletion.
  let pruneDialogWorkspace: WorkspaceRef | null = $state(null);
  let lastOpenBranchKey = $state('');
  let branchRefreshSeq = 0;
  // The workspace key the open tag list was read for.
  let lastTagsKey = '';
  const branchSync = createBranchSync({
    pane: () => pane,
    onBranches: (threadId, next) => {
      if (pane.thread?.id === threadId) branches = next;
    },
  });

  // One read for every pane. A draft placeholder holds a git-status
  // subscription of its own now (ChatHeaderActions attaches on the
  // WORKSPACE, not on a thread row), so there is no second, one-shot
  // project-root read to keep in sync with this one.
  let workspaceDirty = $derived(!!pane.gitStatus.status?.hasChanges);
  // The checkout this picker acts on. Null disables the trigger outright.
  let workspace = $derived(pane.workspace);

  // What the workspace is on: live status once loaded, else the row's branch.
  let checkout = $derived(paneCheckout(pane));
  let currentBranch = $derived(checkout.branch);
  let currentTag = $derived(checkout.tag);
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
  let triggerDisplay = $derived(stagedRef
    ? checkoutDisplay(isTagRef(stagedRef) ? { tag: refLabel(stagedRef) } : { branch: refLabel(stagedRef) })
    : checkoutDisplay(checkout));
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
  export function openPicker(): void {
    if (!open && pane.thread && !applying) void handleTrigger();
  }
  let pickerChordSuffix = $derived(chordHintSuffix('composer.picker.branch'));

  const pickerTags = createPickerTags();
  let tagsSupported = $derived(workspaceHasTags(workspace));
  let showRefreshTags = $derived(tagsSupported && !!pane.gitStatus.status?.hasOriginRemote);

  // Recent picks, as wire refs, read once per open.
  let mruRefs: string[] = $state([]);
  let pickerRows = $derived(orderPickerRows({
    branches,
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

  function branchRefreshKey(
    threadIdentity: string | undefined,
    ws: WorkspaceRef | null,
    branch: string,
  ): string {
    return `${threadIdentity ?? ''}\0${workspaceKeyForRef(ws) ?? ''}\0${branch}`;
  }

  async function refreshBranches(threadIdentity: string, ws: WorkspaceRef): Promise<void> {
    const seq = ++branchRefreshSeq;
    try {
      const res = (await GitListBranches(ws)) as GitBranch[] | null;
      if (seq !== branchRefreshSeq) return;
      if (pane.thread?.id !== threadIdentity || !open) return;
      branches = Array.isArray(res) ? res : [];
      loadError = null;
    } catch (err) {
      if (seq !== branchRefreshSeq) return;
      if (pane.thread?.id !== threadIdentity || !open) return;
      branches = [];
      loadError = userFacingError(err);
    }
  }

  function loadTags(ws: WorkspaceRef): void {
    if (workspaceHasTags(ws)) void pickerTags.load(ws);
  }

  async function handleTrigger(): Promise<void> {
    open = !open;
    if (!open) {
      pickerTags.clear();
      return;
    }
    if (!pane.thread || loading) return;
    const ws = workspace;
    if (!ws) return;
    const threadIdentity = pane.thread.id;
    lastOpenBranchKey = branchRefreshKey(threadIdentity, ws, currentBranch);
    lastTagsKey = workspaceKeyForRef(ws) ?? '';
    refreshMru();
    loading = true;
    loadError = null;
    // The dirty bit streams in through the shared workspace-keyed git-status
    // store for every pane, placeholder included — nothing to fetch here.
    // Tags load beside branches so a large tag set never delays them.
    loadTags(ws);
    const fetchBranches = refreshBranches(threadIdentity, ws);
    void (async () => {
      let fetched: boolean;
      try {
        fetched = !!(await GitMaybeFetchRemotes(ws));
      } catch (err) {
        console.error('background fetch failed:', err);
        return;
      }
      // Refresh what the picker lists now: the pane may have moved to
      // another workspace while the fetch ran.
      const current = workspace;
      if (!fetched || !open || !pane.thread || !current) return;
      loadTags(current);
      await refreshBranches(pane.thread.id, current);
    })();
    try {
      await fetchBranches;
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    const branch = currentBranch;
    const threadIdentity = pane.thread?.id;
    const ws = workspace;
    const key = branchRefreshKey(threadIdentity, ws, branch);
    if (!open || !threadIdentity || !ws) {
      lastOpenBranchKey = key;
      return;
    }
    if (key === lastOpenBranchKey) return;
    lastOpenBranchKey = key;
    void refreshBranches(threadIdentity, ws);
  });

  // Tags belong to the repository, not the checked-out ref: reread only
  // when the open picker's workspace changes.
  $effect(() => {
    const ws = workspace;
    if (!open || !ws) return;
    const key = workspaceKeyForRef(ws) ?? '';
    if (key === lastTagsKey) return;
    lastTagsKey = key;
    untrack(() => {
      pickerTags.clear();
      loadTags(ws);
    });
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
    if (branch.name !== currentBranch) return false;
    if (branch.worktreePath) return sameNormalizedPath(branch.worktreePath, currentWorkspace);
    return true;
  }

  function isCheckedOut(row: PickerRow): boolean {
    if (row.kind === 'tag') return !currentBranch && row.name === currentTag;
    return isSelectedBranch(row.branch);
  }

  // Highlight reflects the active flow's target:
  //   - creating: the row that is the staged base
  //   - new-worktree, !creating: the row that is the staged attach target
  //   - local, !creating: the row that's currently checked out
  function isRowSelected(row: PickerRow): boolean {
    if (intent.creatingBranch) return row.ref === intent.newBranchBase;
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
  title={`Branch: ${triggerLabel}${pickerChordSuffix}`}
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
    {#if loading}
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
          description={currentBranch || undefined}
          checked={isLocalSelected}
          onSelect={selectLocalRow}
        />
        <MenuDivider />
      {/if}
      {#if loadError}
        <div
          class="line-clamp-3 max-w-72 break-words px-3 py-1.5 text-xs text-error"
          role="presentation"
          title={loadError}
          data-testid="branch-picker-error"
        >
          Failed to load branches: {loadError}
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
              action={showsSyncAction(row) ? syncIcon : undefined}
              actionLabel={branchSync.syncing === row.name
                ? `Syncing ${row.name}`
                : `Sync ${row.name} from upstream`}
              actionDisabled={row.kind !== 'branch' || branchSync.actionDisabled(row.branch)}
              actionTitle={row.kind === 'branch' ? branchSync.actionTitle(row.branch) : undefined}
              onAction={() => { if (row.kind === 'branch') void branchSync.request(row.branch, closeMenu); }}
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
