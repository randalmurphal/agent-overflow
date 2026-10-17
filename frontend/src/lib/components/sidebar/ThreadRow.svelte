<script lang="ts">
  import ForkPendingRow from './ForkPendingRow.svelte';
  import {
    getJumpHintsVisible,
    jumpLabelForThread,
  } from '../../stores/keyboardModifiers.svelte';
  import { chordHintForCommand } from '../../stores/keybindings.svelte';
  import { assistantKey, isStandingAgent, sidebarPinGroup } from '../../utils/sidebarTree';
  import { assistantDisplayName } from '../../stores/assistants.svelte';
  import { getShowProviderIcons, setAgentExpanded, toggleAgent } from '../../stores/sidebar.svelte';
  import { asProviderID, providerLabel } from '../../providers/catalog';
  import ProviderIcon from '../shared/ProviderIcon.svelte';
  import { getSettings } from '../../stores/settings.svelte';
  import { clearSidebarCursor, getSidebarCursorThreadId } from '../../stores/sidebarCursor.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadById, getThreadLiveActivityAt } from '../../stores/threads.svelte';
  import {
    threadMachineUnreachable,
    unreachableThreadMachineName,
  } from '../../stores/attachedBackends.svelte';
  import { getMinuteNow } from '../../stores/minuteClock.svelte';
  import {
    findPaneShowingThread,
    openThreadFromNavigation,
    openThreadInNewPane,
    openThreadInPane,
  } from '../../stores/panes.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { getEffectiveThreadStatus, getThreadWaitingOn } from '../../stores/threadStatuses.svelte';
  import type { Thread } from '../../types/models';
  import type { ThreadLiveStatus } from '../../stores/threadStatuses.svelte';
  import type { ThreadStatusPill } from '../../utils/threadStatusPill';
  import { entryIdFor, getProjectLabelText } from '../../stores/projects.svelte';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import FileText from '@lucide/svelte/icons/file-text';
  import Terminal from '@lucide/svelte/icons/terminal';
  import UserRound from '@lucide/svelte/icons/user-round';
  import SquarePen from '@lucide/svelte/icons/square-pen';
  import Icon from '../primitives/Icon.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import AgentStateLine from './AgentStateLine.svelte';
  import ThreadRowActions from './ThreadRowActions.svelte';
  import ThreadRowSublabel from './ThreadRowSublabel.svelte';
  import ThreadRowForkAffordance from './ThreadRowForkAffordance.svelte';
  import ThreadContextMenu from './ThreadContextMenu.svelte';
  import ThreadRowPinButton from './ThreadRowPinButton.svelte';
  import SidebarRowMenuButton from './SidebarRowMenuButton.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import {
    archiveThreadAction,
    deleteThreadAction,
    nextAssistantThreadAction,
    PIN_GROUP_BACK,
    PIN_GROUP_FRONT,
    pinThreadAction,
    renameThreadAction,
    setThreadPinGroupAction,
    unpinThreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';
  import {
    TerminalsClosingNote,
    terminalsClosingNoteForThreadDelete,
    withTerminalsNote,
  } from '../../stores/worktreeRemoval.svelte';
  import {
    hasUnread,
    lastKnownThreadStatusPill,
    resolveThreadStatusPill,
  } from '../../utils/threadStatusPill';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import {
    beginThreadRowDrag,
    encodeThreadDragPayload,
    endThreadRowDrag,
    THREAD_ROW_DRAG_MIME,
    type ThreadDragPayload,
  } from '../../utils/threadDragPayload';
  import {
    GROUP_GLYPH_LEFT_PX,
    GROUP_MEMBER_PIN_SLOT_LEFT_PX,
    GROUP_ROW_PADDING_LEFT_PX,
    sidebarGroupMemberPaddingLeftPx,
    sidebarRowPaddingLeftPx,
    sidebarTimeLabel,
  } from '../../utils/sidebarRowMetrics';
  import { threadBackend } from '../../transport/entityIndex';
  import { HOME_BACKEND } from '../../transport/backendKey';
  import { threadHasScope } from '../../transport/entityScopes';

  let {
    thread,
    pane,
    selected = false,
    onSelectClick,
    indent = 0,
    inGroup = false,
    agent = null,
    agentChildren = [],
    archivedChild = false,
    listProjectId = null,
  }: {
    thread: Thread;
    pane: ThreadPane | null;
    selected?: boolean;
    /**
     * Called before the thread-switch path when the user clicks the row.
     * Return `true` to suppress the thread switch (row handled as a
     * multi-select action instead). `modifier` describes what the click
     * should do: 'range' (shift) or null / 'single'. Cmd/Ctrl-click is
     * reserved for opening/focusing a pane.
     */
    onSelectClick?: (modifier: 'toggle' | 'range' | 'single' | null) => boolean;
    /** Visual indent level. 0 = top, 1 = group member. */
    indent?: number;
    /** Rendered inside a group's or an agent's rail. Such a row is never a jump target. */
    inGroup?: boolean;
    /**
     * The standing-agent variant (docs/specs/standing-agents.md, "Sidebar"):
     * a chevron folds the threads the agent spawned, a person glyph marks
     * it, and a state line names its soonest loop or watch. Null for an
     * ordinary thread row.
     */
    agent?: {
      expanded: boolean;
      childCount: number;
      /**
       * The bubbled status of the agent's own thread and its children,
       * which the row shows while it is collapsed (ruling 33).
       */
      rolledUp?: { liveStatus: ThreadLiveStatus; pill: ThreadStatusPill } | null;
    } | null;
    /** The agent's live spawned threads, for the menu's Archive Done Children. */
    agentChildren?: readonly Thread[];
    /** An archived spawned thread listed under an agent's done footer. */
    archivedChild?: boolean;
    /**
     * The project whose list renders this row. A child an agent spawned into
     * another project nests under the agent here and names its own project
     * on the sublabel line.
     */
    listProjectId?: string | null;
  } = $props();

  // isActive: this thread is what the FOCUSED pane shows (the sidebar's
  // `pane` prop). isOpen: it is mounted in some pane, focused or not.
  let isActive = $derived(pane?.threadId === thread.id);
  let isOpen = $derived(findPaneShowingThread(thread.id) !== null);
  let isCursorTarget = $derived(getSidebarCursorThreadId() === thread.id);
  // Compact: no drag (a phone's long press is the menu, and Android starts a
  // drag on a held draggable), no text selection under a hold, taller rows.
  let compact = $derived(isCompactLayout());
  // Terminals aren't archivable — the row offers Delete (X) instead of
  // Archive, and the leading glyph is the terminal icon.
  let isTerminal = $derived(thread.mode === 'terminal');
  let showProviderIcon = $derived(
    getShowProviderIcons() && !isTerminal
      && asProviderID(thread.provider) !== null,
  );

  let liveStatus = $derived(getEffectiveThreadStatus(thread));
  let ownPill = $derived(resolveThreadStatusPill(thread, liveStatus, getThreadWaitingOn(thread.id)));
  let unread = $derived(hasUnread(thread));

  let forkParent = $derived.by<Thread | undefined>(() => {
    if (!thread.forkedFromThreadId) return undefined;
    return getThreadById(thread.forkedFromThreadId);
  });

  function ctx(): ThreadActionCtx {
    return {
      thread,
      isActive,
      clearPane: () => pane?.clear(),
      switchPane: async (t) => {
        if (pane) await openThreadInPane(t, pane);
        else await openThreadInPane(t);
      },
      reportError: (msg, err) => {
        if (pane) pane.setGeneralError(msg, err);
        else addErrorToast(msg, err);
      },
      replacePaneThread: (t) => pane?.replaceThread(t),
    };
  }

  async function handleJumpToParent(e: MouseEvent): Promise<void> {
    e.stopPropagation();
    if (!forkParent) return;
    if (pane) await openThreadFromNavigation(forkParent, pane);
    else await openThreadFromNavigation(forkParent);
  }

  // Inline rename state (owned by the row so the <input> replaces the
  // title span in place — same pattern as ProjectItem).
  let editing = $state(false);
  let editValue = $state('');
  let renameError: string | null = $state(null);
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let saving = $state(false);

  // Archive-confirm dialog (shown when settings.confirmArchive is true).
  // The right-click Delete path's confirm dialog lives inside
  // ThreadContextMenu; the terminal row-X's lives here (showDeleteConfirm).
  let showArchiveConfirm = $state(false);
  // Delete-confirm dialog for the terminal row-X (shown when
  // settings.confirmDelete is true).
  let showDeleteConfirm = $state(false);
  const deleteTerminalsNote = new TerminalsClosingNote();

  // Context menu anchor + state.
  let rowEl: HTMLDivElement | undefined = $state(undefined);
  let ctxOpen = $state(false);

  function handleClick(e?: MouseEvent) {
    if (editing) return;
    // A mouse click is an unambiguous "I'm picking THIS thread" — drop
    // the visual cursor so it doesn't drift from where the user is.
    clearSidebarCursor();
    if (e && (e.metaKey || e.ctrlKey)) {
      void openThreadInNewPane(thread);
      return;
    }
    if (onSelectClick && e) {
      const modifier: 'toggle' | 'range' | 'single' | null = e.shiftKey
        ? 'range'
        : null;
      if (modifier !== null) {
        const handled = onSelectClick(modifier);
        if (handled) return;
      }
    }
    if (pane) void openThreadFromNavigation(thread, pane);
    else void openThreadFromNavigation(thread);
  }

  function startRename() {
    editing = true;
    editValue = thread.title || '';
    requestAnimationFrame(() => {
      inputEl?.focus();
      inputEl?.select();
    });
  }

  async function saveRename() {
    if (saving) return;
    saving = true;
    let refused: string | null = null;
    try {
      refused = await renameThreadAction(ctx(), editValue);
    } finally {
      saving = false;
      // A refusal keeps the editor open with the reason under it.
      renameError = refused;
      if (refused) requestAnimationFrame(() => inputEl?.focus());
      else editing = false;
    }
  }

  function cancelRename() {
    editing = false;
    renameError = null;
  }

  function handleRenameKeydown(e: KeyboardEvent) {
    // Enter confirms the IME candidate while composing a CJK title; committing
    // the rename here would save the pre-composition text and exit edit mode.
    if (e.key === 'Enter' && isImeComposingEvent(e)) return;
    if (e.key === 'Enter') {
      e.preventDefault();
      saveRename();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelRename();
    }
  }

  function handleArchive(e: MouseEvent) {
    e.stopPropagation();
    if (getSettings().confirmArchive) {
      showArchiveConfirm = true;
    } else {
      void archiveThreadAction(ctx());
    }
  }

  function handleDelete(e: MouseEvent) {
    e.stopPropagation();
    if (getSettings().confirmDelete) {
      showDeleteConfirm = true;
      deleteTerminalsNote.load(() => terminalsClosingNoteForThreadDelete([thread]));
    } else {
      void deleteThreadAction(ctx());
    }
  }

  // A standing agent sits in the groups section whatever pin its thread
  // holds, so no row of it offers a pin, the collapsed project's preview
  // of it included.
  let isAgent = $derived(isStandingAgent(thread));
  // An assistant's name carries its computer while another attached
  // computer has an assistant of the same name.
  let rowTitle = $derived((isAgent ? assistantDisplayName(thread) : thread.title) || 'Untitled');
  let isPinned = $derived(!isAgent && thread.pinnedAt != null);
  // The pin, archive and delete controls write the row; inert with a reason
  // when the session lacks the grant, the same treatment the context menu
  // gives its rows.
  let operateUngranted = $derived(!threadHasScope('threads:operate', thread.id, thread.projectId));
  // StartNextAssistantThread rides threads:autonomy.
  let nextThreadUngranted = $derived(!threadHasScope('threads:autonomy', thread.id, thread.projectId));
  let isJumpTarget = $derived(
    !inGroup && !isAgent && sidebarPinGroup(thread) === 'front',
  );
  // A standing agent's own row never joins a group: the drag payload says
  // so, and every group target refuses it. A thread it spawned joins
  // groups like any other (docs/specs/standing-agents.md ruling 36).
  let agentBound = $derived(isAgent);

  function handleAgentChevronClick(e: MouseEvent): void {
    e.stopPropagation();
    if (editing) return;
    toggleAgent(assistantKey(thread));
  }

  function handleRowKeydown(e: KeyboardEvent): void {
    if (editing) return;
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      handleClick();
      return;
    }
    if (e.key === 'F2') {
      e.preventDefault();
      startRename();
      return;
    }
    if (agent === null || e.target !== e.currentTarget) return;
    if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
      e.preventDefault();
      setAgentExpanded(assistantKey(thread), e.key === 'ArrowRight');
    }
  }

  // Jump-hint label for this row when the user holds Cmd/Ctrl. Reactive:
  // The store tracks rendered pin order while the modifier is held.
  let jumpLabel = $derived.by<string | null>(() => {
    if (!getJumpHintsVisible()) return null;
    return jumpLabelForThread(thread.id) ?? null;
  });
  let jumpShortcut = $derived.by<string | null>(() => {
    if (!jumpLabel) return null;
    // null when that slot's command is unbound — the pill is a hint for a
    // chord that exists, so it renders nothing rather than a chord the
    // user cleared.
    return chordHintForCommand(`thread.jump.${jumpLabel}`);
  });

  // The row's activity stamp. getThreadLiveActivityAt folds in the
  // per-thread streaming box (row objects no longer churn per beat),
  // and getMinuteNow keeps an idle row's relative label creeping
  // forward now that unrelated beats no longer re-render every row.
  let timeLabel = $derived.by(() => {
    getMinuteNow();
    // Read against the clock of the machine that minted the stamp
    // (transport/backendClock.ts); a row whose owner is not known yet
    // reads as home, which is what every row meant before a second
    // machine could be attached.
    const backend = threadBackend(thread.id) ?? HOME_BACKEND;
    // An assistant's past thread in its done list says when a handoff
    // replaced it (docs/specs/standing-agents.md, "Sidebar").
    if (archivedChild && thread.agentSupersededAt) return `superseded ${sidebarTimeLabel(thread.agentSupersededAt, backend)}`;
    return sidebarTimeLabel(getThreadLiveActivityAt(thread), backend);
  });

  function handleContextMenu(e: MouseEvent) {
    // Cancel an in-flight rename so the menu anchor is a stable row
    // element and the input doesn't swallow the menu's outside-click.
    if (editing) cancelRename();
    e.preventDefault();
    e.stopPropagation();
    ctxOpen = true;
  }

  function handleDragStart(e: DragEvent): void {
    if (editing || !e.dataTransfer) {
      e.preventDefault();
      return;
    }
    const payload: ThreadDragPayload = {
      threadId: thread.id,
      title: thread.title || 'Untitled',
      projectId: thread.projectId ?? '',
      ...(thread.groupId ? { groupId: thread.groupId } : {}),
      ...(agentBound ? { agentBound: true } : {}),
    };
    e.dataTransfer.effectAllowed = 'copyMove';
    e.dataTransfer.setData(THREAD_ROW_DRAG_MIME, encodeThreadDragPayload(payload));
    e.dataTransfer.setData('text/plain', thread.id);
    // The group drop targets read this during dragover, where the
    // DataTransfer is in protected mode and hands back nothing.
    beginThreadRowDrag(payload);

    const ghost = document.createElement('div');
    ghost.className = 'fixed -top-96 left-0 flex items-center gap-2 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1 text-xs text-fg shadow-menu';
    if (pill) {
      const dot = document.createElement('span');
      dot.className = `h-2 w-2 rounded-full ${pill.dotClass}`;
      ghost.appendChild(dot);
    }
    const title = document.createElement('span');
    title.textContent = thread.title || 'Untitled';
    ghost.appendChild(title);
    document.body.appendChild(ghost);
    e.dataTransfer.setDragImage(ghost, 8, 8);
    window.setTimeout(() => ghost.remove(), 0);
  }

  function closeCtxMenu() {
    ctxOpen = false;
  }

  // Indent + pin gutter come from utils/sidebarRowMetrics so a group row and
  // the member rows under it line up to the pixel. An agent row takes the
  // group row's geometry: its chevron centres on the pin column and its
  // children's titles line up with its name.
  let rowPaddingLeftPx = $derived(
    agent !== null
      ? GROUP_ROW_PADDING_LEFT_PX
      : inGroup ? sidebarGroupMemberPaddingLeftPx(indent) : sidebarRowPaddingLeftPx(indent),
  );
  // Inside a group the pin centres under the group's folder glyph.
  let pinSlotLeftPx = $derived(inGroup ? GROUP_MEMBER_PIN_SLOT_LEFT_PX : 0);

  // A thread on an attached machine this client cannot reach right now
  // dims in place and stays readable from the replica (spec §10). Home is
  // never dimmed here: its drop is the transport banner's.
  let machineUnreachable = $derived(threadMachineUnreachable(thread.id, thread.projectId));
  // A status from an unreachable computer is its last report: shown, but
  // still, and labelled as such.
  let unreachableMachine = $derived(unreachableThreadMachineName(thread.id, thread.projectId));
  let ownRowPill = $derived(
    unreachableMachine !== null ? lastKnownThreadStatusPill(ownPill, unreachableMachine) : ownPill,
  );
  // A collapsed agent row carries its children's dot; expanded, each row
  // shows its own (ruling 33). The bubbled pill already went through the
  // unreachable-computer treatment in the tree.
  let rolledUp = $derived(agent !== null && !agent.expanded ? (agent.rolledUp ?? null) : undefined);
  let pill = $derived(rolledUp !== undefined ? (rolledUp?.pill ?? null) : ownRowPill);
  let dotStatus = $derived(rolledUp ? rolledUp.liveStatus : liveStatus);
  let foreignProjectName = $derived(
    listProjectId && thread.projectId && entryIdFor(thread.projectId) !== entryIdFor(listProjectId)
      ? getProjectLabelText(thread.projectId)
      : '',
  );
  const WORKTREE_META_OFFSET_PX = 14;
  let worktreeIndentPx = $derived(rowPaddingLeftPx + WORKTREE_META_OFFSET_PX);
</script>

<!--
  Open-thread marker: a 2px accent bar on the shell's left edge (left of
  the pin gutter) for every thread mounted in some pane; the focused
  pane's thread adds a fill on top. Both derive from the theme's accent
  token. The keyboard cursor keeps its own inset ring, so the two never
  share a channel. The bar is the shell's ::after because the approval /
  input glow (`.status-glow-*` in app.css) owns ::before: an open row
  that is also blocked on the user needs both, and one pseudo-element
  cannot be a 2px bar and a full-row ring at once.
-->
<!--
  The shell, not the row, takes the pointer: click, double-click,
  contextmenu (which is also the phone's long press, via the window-level
  bridge) and drag. The row is the focusable ARIA button, but it is one
  line tall and the worktree / computer sublabel under it is a sibling
  inside the shell. A tap on that sublabel must still open the thread, a
  long press on it must still open THIS row's menu, and a press-and-move
  on it must drag the thread rather than select the label's text. So the
  whole shell is the target and selection is off across it under compact.
  Controls inside the row that own their click stop propagation as before.
-->
{#if thread.forkPreparing}
  <ForkPendingRow title={thread.title} />
{:else}
<!-- Keyboard activation lives on the focusable row inside. -->
<!-- svelte-ignore a11y_no_static_element_interactions, a11y_click_events_have_key_events -->
<div
  onclick={(e) => handleClick(e)}
  ondblclick={startRename}
  oncontextmenu={handleContextMenu}
  ondragstart={handleDragStart}
  ondragend={endThreadRowDrag}
  draggable={!editing && !compact}
  class="group/thread-item relative rounded-[var(--radius-field)] transition-colors cursor-pointer compact:select-none
    {selected ? 'bg-accent/15' : isActive ? 'bg-accent/20' : 'hover:bg-surface-2/30'}
    {isOpen ? 'after:absolute after:left-0 after:inset-y-1 after:w-0.5 after:rounded-full after:bg-accent' : ''}
    {isOpen && !isActive ? 'after:opacity-70' : ''}
    {isCursorTarget ? 'ring-1 ring-accent/70 ring-inset' : (ownRowPill?.ringClass ?? '')}
    {ownRowPill?.glowClass ?? ''}"
  data-testid="thread-row-shell"
  data-open={isOpen || null}
  data-focused={isActive || null}
  data-cursor-target={isCursorTarget || null}
>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    bind:this={rowEl}
    onkeydown={handleRowKeydown}
    role="button"
    tabindex={0}
    aria-pressed={selected}
    aria-expanded={agent !== null ? agent.expanded : undefined}
    class="group/thread-row relative flex items-center gap-1.5 h-6 pr-1 compact:h-9 compact:select-none rounded-[var(--radius-field)] cursor-pointer transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40
      {selected || isOpen ? 'text-fg' : 'text-fg-muted group-hover/thread-item:text-fg'}
      {machineUnreachable || archivedChild ? 'opacity-50' : ''}"
    style="padding-left: {rowPaddingLeftPx}px"
    data-testid="thread-row"
    data-sidebar-thread-id={thread.id}
    data-sidebar-jump-target={isJumpTarget ? '' : undefined}
    data-sidebar-agent={agent !== null ? '' : undefined}
    data-expanded={agent !== null ? agent.expanded : undefined}
    data-archived-child={archivedChild || undefined}
    data-effective-status={liveStatus}
    data-machine-unreachable={machineUnreachable || undefined}
  >
  <!--
    Leading pin slot. Absolutely positioned inside the row's reserved
    pin gutter (PIN_SLOT_PX of padding-left) so the pin sits in the
    gap between the project rail and the row content without
    contributing a flex gap of its own. The wrapper is non-interactive;
    the button inside opts back in to pointer events when visible.
  -->
  {#if agent !== null}
    <!-- The chevron is the only fold target: name, dot and state line open
         the thread. Always rendered, as a group's is, so the row reads as
         a container even with nothing under it. -->
    <button
      type="button"
      onclick={handleAgentChevronClick}
      ondblclick={(e) => e.stopPropagation()}
      data-testid="thread-row-agent-expand"
      aria-label={agent.expanded ? 'Collapse agent' : 'Expand agent'}
      aria-expanded={agent.expanded}
      class="flex items-center justify-center w-4 h-4 rounded text-fg-subtle hover:text-fg hover:bg-surface-2/30 shrink-0 cursor-pointer focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40"
    >
      <Icon
        icon={ChevronRight}
        size={11}
        strokeWidth={2.5}
        class={'opacity-80 transition-transform ' + (agent.expanded ? 'rotate-90' : '')}
      />
    </button>
    <span
      class="inline-flex items-center shrink-0 text-accent"
      aria-label="Standing agent"
      title="Standing agent"
      role="img"
      data-testid="thread-row-agent-icon"
    >
      <Icon icon={UserRound} size={11} strokeWidth={1.75} />
    </span>
  {:else if !isAgent && !archivedChild}
    <div class="absolute inset-y-0 flex items-center justify-center w-6 pointer-events-none" style="left: {pinSlotLeftPx}px">
      <ThreadRowPinButton
        {isPinned}
        pinGroup={thread.pinGroup}
        disabled={operateUngranted}
        onToggle={() => { if (isPinned) void unpinThreadAction(ctx()); else void pinThreadAction(ctx()); }}
        onCycleBurner={() => void setThreadPinGroupAction(
          ctx(),
          thread.pinGroup === PIN_GROUP_BACK ? PIN_GROUP_FRONT : PIN_GROUP_BACK,
        )}
      />
    </div>
  {/if}

  {#if pill}
    <span
      class="w-1.5 h-1.5 rounded-full shrink-0 {pill.dotClass} {pill.pulse ? 'animate-pulse' : ''}"
      role="status"
      aria-label={pill.label}
      title={pill.label}
      data-testid="thread-row-status-dot"
      data-status={dotStatus}
      data-rolled-up={rolledUp ? '' : undefined}
    ></span>
  {/if}

  {#if editing}
    <!-- svelte-ignore a11y_autofocus -->
    <input
      bind:this={inputEl}
      bind:value={editValue}
      onkeydown={handleRenameKeydown}
      onblur={saveRename}
      disabled={saving}
      aria-label="Rename Thread"
      aria-invalid={renameError !== null}
      oninput={() => { renameError = null; }}
      class="text-xs compact:text-base flex-1 min-w-0 bg-surface-0 border border-accent/50 rounded-[var(--radius-field)] px-1 py-0.5 text-fg focus:outline-none"
      onclick={(e) => e.stopPropagation()}
    />
  {:else}
    {#if isTerminal}
      <!--
        Terminal rows render the boxless lucide `Terminal` glyph (a `>_`
        prompt) — the SAME glyph the chat history shows for bash/terminal
        tool calls (ToolKindIcon kind="terminal"), in the matching green
        `text-ico-terminal` — so a terminal reads as "a terminal" at a glance.

        This is checked BEFORE the draft branch on purpose. The backend marks
        any thread with no persisted items as a draft (store.go: IsDraft is
        "no items exist for the thread"), and a terminal is a degenerate,
        item-less thread, so it reaches this row with isDraft === true. Mode is
        the stronger signal — a terminal is never a composer draft — so it must
        win the icon slot regardless of the draft flag.
      -->
      <span
        class="inline-flex items-center shrink-0 text-ico-terminal"
        data-testid="thread-row-terminal-icon"
        aria-label="Terminal thread"
        title="Terminal"
      >
        <Icon icon={Terminal} size={11} strokeWidth={1.75} />
      </span>
    {:else if thread.isDraft}
      <span
        class="inline-flex items-center shrink-0 text-fg-subtle"
        data-testid="thread-row-draft-icon"
        aria-label="Draft thread"
        title="Draft"
      >
        <Icon icon={FileText} size={11} strokeWidth={1.75} class="opacity-70" />
      </span>
    {/if}
    <span
      data-testid="thread-row-title"
      class="text-xs truncate flex-1 min-w-0 {unread ? 'font-semibold text-fg' : agent !== null ? 'font-medium' : ''}"
      title={rowTitle}
    >
      {rowTitle}
    </span>
    {#if agent !== null && !agent.expanded && agent.childCount > 0}
      <span
        class="-ml-0.5 mr-1 shrink-0 min-w-5 text-right text-[0.625rem] tabular-nums text-fg-hint"
        data-testid="thread-row-agent-count"
      >
        {agent.childCount}
      </span>
    {/if}

    <div class="flex items-center gap-1 shrink-0 -ml-0.5" data-testid="thread-row-trailing">
      {#if agent !== null}
        <!-- New Thread, Same Assistant: on hover, and at rest under the
             compact layout, which has no hover. -->
        <button
          type="button"
          onclick={(e) => { e.stopPropagation(); void nextAssistantThreadAction(ctx()); }}
          disabled={nextThreadUngranted}
          aria-label="New Thread, Same Assistant"
          title={nextThreadUngranted ? 'Not granted to this device' : 'New Thread, Same Assistant'}
          data-testid="thread-row-next-thread"
          class="flex items-center justify-center h-5 w-5 compact:h-9 compact:w-9 rounded-[var(--radius-field)] shrink-0 cursor-pointer text-fg-subtle hover:text-fg hover:bg-surface-2/40 opacity-0 pointer-events-none transition-opacity duration-150 group-hover/thread-item:opacity-100 group-hover/thread-item:pointer-events-auto group-has-[:focus-visible]/thread-row:opacity-100 group-has-[:focus-visible]/thread-row:pointer-events-auto compact:opacity-100 compact:pointer-events-auto focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40"
        >
          <Icon icon={SquarePen} size={12} strokeWidth={2} class="opacity-90" />
        </button>
      {/if}
      {#if thread.forkedFromThreadId}
        <ThreadRowForkAffordance {forkParent} onJumpToParent={handleJumpToParent} />
      {/if}
      {#if showProviderIcon}
        <span
          class="inline-flex items-center shrink-0"
          title={providerLabel(thread.provider)}
          role="img"
          aria-label={providerLabel(thread.provider)}
          data-testid="thread-row-provider"
        >
          <ProviderIcon provider={thread.provider} size={12} />
        </span>
      {/if}
      <!--
        Right-side slot. A fixed min-w-5 keeps the layout stable when the
        time label fades out and the action button (archive or delete)
        fades in on hover / keyboard focus. Both live in `relative` so the
        button can absolute-position over the time without pushing layout.
        The keyboard reveal keys off `group-has-[:focus-visible]/thread-row`
        (a focus-VISIBLE descendant) rather than `:focus-within`, so a mouse
        click on the tabindex=0 row doesn't leave the action stuck visible.
      -->
      <div class="relative shrink-0 flex items-center justify-end min-w-5">
        {#if jumpShortcut}
          <!--
            Modifier-held jump-hint pill. Fades in on the right side,
            replacing the relative-time stamp. The shown keybinding navigates to
            this row when active.
          -->
          <span
            class="inline-flex h-5 items-center rounded-[var(--radius-field)] border border-border-subtle bg-surface-1/90 px-1.5 font-mono text-[0.625rem] font-medium text-fg shadow-sheet pointer-events-none"
            aria-hidden="true"
            data-testid="thread-row-jump-hint"
          >
            {jumpShortcut}
          </span>
        {:else}
          <span
            class="text-[0.625rem] tabular-nums text-fg-hint transition-opacity duration-150 pointer-events-none group-hover/thread-item:opacity-0 group-has-[:focus-visible]/thread-row:opacity-0"
            data-testid="thread-row-time"
          >
            {timeLabel}
          </span>
          <!--
            Hover actions stay unmounted while the jump-hint pill is up —
            otherwise the absolutely-positioned archive/delete button paints
            over the ctrl+# pill on the hovered row.
          -->
          <div
            class="absolute inset-y-0 right-0 flex items-center opacity-0 pointer-events-none transition-opacity duration-150 group-hover/thread-item:opacity-100 group-hover/thread-item:pointer-events-auto group-has-[:focus-visible]/thread-row:opacity-100 group-has-[:focus-visible]/thread-row:pointer-events-auto"
          >
            <ThreadRowActions
              onArchive={isTerminal || archivedChild ? undefined : handleArchive}
              onDelete={isTerminal ? handleDelete : undefined}
              disabled={operateUngranted}
            />
          </div>
        {/if}
      </div>
    </div>
    <SidebarRowMenuButton label="Thread actions" testId="thread-row-menu" onOpen={handleContextMenu} />
  {/if}
  </div>

  {#if editing && renameError}
    <p role="alert" class="px-2 pb-1 text-[0.625rem] text-error" style="padding-left: {worktreeIndentPx}px" data-testid="thread-row-rename-error">{renameError}</p>
  {:else if agent !== null}
    <AgentStateLine threadId={thread.id} indentPx={GROUP_GLYPH_LEFT_PX} />
  {:else if !editing}
    <ThreadRowSublabel {thread} indentPx={worktreeIndentPx} projectName={foreignProjectName} />
  {/if}
</div>

  <ThreadContextMenu
  {thread}
  {pane}
  anchor={rowEl}
  open={ctxOpen}
  onClose={closeCtxMenu}
  onRename={startRename}
  {isActive}
  {agentBound}
  {agentChildren}
  {archivedChild}
/>

<ConfirmDialog
  open={showArchiveConfirm}
  title="Archive Thread"
  description="This will hide the thread from the sidebar. Open Settings → Storage to find it later."
  confirmLabel="Archive"
  onConfirm={() => { showArchiveConfirm = false; void archiveThreadAction(ctx()); }}
  onCancel={() => { showArchiveConfirm = false; }}
/>

<ConfirmDialog
  open={showDeleteConfirm}
  title="Delete Terminal"
  description={withTerminalsNote(
    'This will remove the terminal from the sidebar and close its shell. This cannot be undone.',
    deleteTerminalsNote.note,
  )}
  confirmLabel="Delete"
  destructive={true}
  onConfirm={() => { showDeleteConfirm = false; deleteTerminalsNote.clear(); void deleteThreadAction(ctx()); }}
  onCancel={() => { showDeleteConfirm = false; deleteTerminalsNote.clear(); }}
/>

{/if}
