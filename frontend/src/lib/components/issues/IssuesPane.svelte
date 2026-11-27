<script module lang="ts">
  import type { SplitSpec } from '../shared/splitWidth.svelte';

  /** The list beside a ticket keeps 520, the ticket 380; the split starts
   * at half. */
  const TICKET_SPLIT: SplitSpec = { listMin: 520, listMax: Infinity, detailMin: 380, initial: (body) => Math.floor(body / 2) };
  /** A ticket shows beside the list only in a pane this wide. */
  const BESIDE_MIN_PX = 900;
  /** The tabs fold into one picker in a pane narrower than this. */
  const TABS_MIN_PX = 640;

  // The Time screen loads on its first switch to Time, once per page.
  let timeScreen: Promise<typeof import('./TimeScreen.svelte')> | null = null;
  function loadTimeScreen(): Promise<typeof import('./TimeScreen.svelte')> {
    timeScreen ??= import('./TimeScreen.svelte');
    return timeScreen;
  }

  // The Backlog screen loads on its first switch to Backlog, once per page.
  let backlogScreen: Promise<typeof import('./BacklogScreen.svelte')> | null = null;
  function loadBacklogScreen(): Promise<typeof import('./BacklogScreen.svelte')> {
    backlogScreen ??= import('./BacklogScreen.svelte');
    return backlogScreen;
  }

  // The Dashboards screen loads on its first switch to Dashboards, once per page.
  // The board's summary row, loaded the first time a board pins a widget.
  let summaryRow: Promise<typeof import('./BoardSummaryRow.svelte')> | null = null;
  function loadSummaryRow(): Promise<typeof import('./BoardSummaryRow.svelte')> {
    summaryRow ??= import('./BoardSummaryRow.svelte');
    return summaryRow;
  }
  let dashboardScreen: Promise<typeof import('./DashboardScreen.svelte')> | null = null;
  function loadDashboardScreen(): Promise<typeof import('./DashboardScreen.svelte')> {
    dashboardScreen ??= import('./DashboardScreen.svelte');
    return dashboardScreen;
  }
</script>

<script lang="ts">
  // The Issues tool pane (docs/specs/issues.md, "Pane", "Board", "Backlog
  // and sprint planning" and "Time"): one app-wide pane over every
  // project's boards on every attached computer. The header holds the
  // board picker, the screens (Board, Backlog, Dashboards and Time, folded
  // into one picker in a narrow pane and under compact), and on the Board
  // and the Backlog Only mine, Display and Search, and Refresh; under
  // compact Only mine moves into the Display menu. Backlog (BacklogScreen,
  // loaded on the first switch to it) shows the same board's sprints and
  // backlog. Dashboards (DashboardScreen, loaded likewise) counts the
  // board's copy; the board's subscription stays voted shown under it, so
  // the copy keeps syncing, and Refresh reads the board and counts again. Time shows the
  // person's logged time on the board's connection (TimeScreen, loaded on
  // the first switch to it); while it shows, the board is not mounted and
  // its subscription is voted hidden, and Refresh reads the week. Notices
  // under the header say what the board's computer last told about it: a
  // failure with when it last read the board, what a Refresh answered, a
  // board that ended. Which board it shows, and its subscription, are the
  // pane's board selection (`createBoardSelection`); the subscription is
  // voted hidden while the window is hidden or the pane is not on screen
  // (`isPaneOnScreen`: another pane promoted, or a compact screen showing
  // something else). A card opens its ticket over the board, or beside it
  // in a pane 900 or wider when the person chose that; the board stays
  // mounted under it, with its scroll and its loaded depth, and is voted
  // hidden while a ticket covers it. Back, Escape and the phone's Back
  // step back through the tickets opened from one another, then close the
  // ticket. A card pressed with Ctrl or Cmd, or "Open in its own pane" in
  // its menu, opens its ticket in a pane of its own beside this one
  // (stores/ticketPanes.svelte.ts); the same row in the ticket's menu
  // moves the ticket open here into one, and this pane shows the board.
  import { onDestroy, untrack } from 'svelte';
  import SplitResizer from '../shared/SplitResizer.svelte';
  import { SplitWidth } from '../shared/splitWidth.svelte';
  import type { MoveView, RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { attachTrackerTicket, trackerTicketKey } from '../../stores/trackerTicket.svelte';
  import { openTicketMove, refreshFormReads } from '../../stores/trackerTicketWrites.svelte';
  import { registerIssuesBackHandler } from '../../stores/issuesCommands';
  import { registerPaneDetailProbe } from '../../stores/toolPanes.svelte';
  import TicketHost from './TicketHost.svelte';
  import { openTicketPane } from '../../stores/ticketPanes.svelte';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Search from '@lucide/svelte/icons/search';
  import UserRound from '@lucide/svelte/icons/user-round';
  import X from '@lucide/svelte/icons/x';
  import Funnel from '@lucide/svelte/icons/funnel';
  import Plus from '@lucide/svelte/icons/plus';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import ToolPaneHeader from '../panes/ToolPaneHeader.svelte';
  import PaneHeaderIconButton from '../panes/PaneHeaderIconButton.svelte';
  import PaneFindBar from '../panes/PaneFindBar.svelte';
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import ErrorNotice from '../shared/ErrorNotice.svelte';
  import BoardPicker from './BoardPicker.svelte';
  import BoardScreen from './BoardScreen.svelte';
  import DisplayMenu from './DisplayMenu.svelte';
  import IssuesTabs from './IssuesTabs.svelte';
  import { captureError, type CapturedError } from '../../stores/errorReports.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { isPaneOnScreen } from '../../stores/paneLayout.svelte';
  import { openSettingsOverlay, openTrackerBoardsSettings } from '../../stores/settingsOverlay.svelte';
  import { openPaneFind, registerPaneFindProvider } from '../../stores/paneFind.svelte';
  import { getProject, getProjects } from '../../stores/projects.svelte';
  import { projectBackend } from '../../transport/entityIndex';
  import { scopeGrant } from '../../transport/scopes';
  import { getTransportStatusFor } from '../../stores/transportStatus.svelte';
  import { isTransportOutage } from '../../transport/wsClient';
  import { attachedBackendEntry, backendDisplayName } from '../../stores/attachedBackends.svelte';
  import { toolOpenedFromProject } from '../../stores/toolPanes.svelte';
  import {
    createBoardSelection,
    hiddenCardFields,
    saveOnlyMine,
    savedOnlyMine,
    setCardFieldShown,
    setTicketPlacement,
    showsBoard,
    takePaneOpenRequest,
    paneOpenRequested,
    type IssuesScreen,
  } from '../../stores/issuesPane.svelte';
  import { dismissTrackerRefreshNotice, refreshTrackerBoard } from '../../stores/trackerBoard.svelte';
  import type { BoardTicketSet, TicketSetTile } from '../../stores/boardTicketSet.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import {
    isBoardChoiceOpenable,
    readTrackerBoardChoices,
    trackerBoardChoiceComputers,
    trackerBoardChoices,
  } from '../../stores/trackerBoardChoices.svelte';
  import { cardMatches, filtering, screenColumns, searchWords, type CardFilter } from '../../stores/trackerBoardModel';
  import { backlogSections } from '../../stores/trackerBacklogModel';
  import { timeOfflineText } from '../../stores/trackerTimeModel';
  import { relativeTime } from '../../utils/format';

  interface Props {
    paneId: string;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { paneId, onPaneDragStart }: Props = $props();

  const compact = $derived(isCompactLayout());
  // Read once: the project of the thread this pane was opened from.
  const openedFrom = toolOpenedFromProject('issues');

  const shown = $derived(isPaneOnScreen(paneId));
  // The body a screen's list and a ticket beside it share, measured into
  // each screen's split; its width also decides whether a ticket fits
  // beside and whether the tabs fold.
  let paneWidth = $state(0);
  const splits: Record<IssuesScreen, SplitWidth> = {
    board: new SplitWidth('issuesTicketSplit:board', TICKET_SPLIT),
    backlog: new SplitWidth('issuesTicketSplit:backlog', TICKET_SPLIT),
    dashboards: new SplitWidth('issuesTicketSplit:dashboards', TICKET_SPLIT),
    time: new SplitWidth('issuesTicketSplit:time', TICKET_SPLIT),
  };
  function measure(width: number): void {
    paneWidth = width;
    for (const split of Object.values(splits)) split.paneWidth = width;
  }
  const besideFits = $derived(!compact && paneWidth >= BESIDE_MIN_PX);
  const selection = createBoardSelection({ openedFromProject: openedFrom, shown: () => shown, besideFits: () => besideFits });
  onDestroy(() => selection.destroy());
  const screen = $derived(selection.screen);
  const split = $derived(splits[screen]);
  // The Time screen open now, for Refresh, and whether its refresh is out.
  let timeView: { refresh(): void } | undefined = $state();
  let timeRefreshing = $state(false);
  // The Dashboards screen open now, for Refresh, and whether it is counting.
  let dashboardView: { refresh(): void } | undefined = $state();
  let rowView: { refreshLogged(): void } | undefined = $state();
  let dashboardCounting = $state(false);
  // A ticket another surface asked for (a thread's link) opens here.
  $effect(() => {
    if (!paneOpenRequested()) return;
    const request = untrack(takePaneOpenRequest);
    if (request) selection.openRequested(request);
  });
  const ticket = $derived(selection.ticket);
  // A ticket over the board covers the board's notices with it: what they
  // say is the board's, and the ticket says its own.
  const boardCovered = $derived(!!ticket && selection.placement === 'over');

  // The ticket view open now, for its own step back (the Details rail).
  let ticketView: TicketHost | undefined = $state();
  function stepBack(): boolean {
    if (!selection.ticket) return false;
    if (!ticketView?.stepBack()) selection.stepBack();
    return true;
  }
  // A tool pane's id is fixed for its life (`tool-issues`).
  // svelte-ignore state_referenced_locally
  const disposeProbe = registerPaneDetailProbe(paneId, () => selection.ticket !== null);
  // svelte-ignore state_referenced_locally
  const disposeBack = registerIssuesBackHandler(paneId, () => { stepBack(); });
  onDestroy(() => {
    disposeBack();
    disposeProbe();
  });

  function openCard(row: RowView, ownPane = false): void {
    const seed = { key: row.card.key, title: row.card.title, card: row.card };
    if (!ownPane) {
      selection.openTicket(row.card.id, seed, 'list');
      return;
    }
    if (!board) return;
    // The pane has no board to fall back on: the card takes the board's project.
    openTicketPane({ ref: { backend: board.backend, connection: board.connection, issue: row.card.id }, seed, project: board.projectId }, { board: board.board });
  }

  // The ticket open here moves into a pane of its own, which takes the
  // focus, and this pane shows the board. The new pane holds the ticket
  // before this one lets it go: its subscription through the bridge (the
  // pane's body loads after the move), so nothing it holds (the view, the
  // earlier comments) is read again, and its drafts through the pane's
  // record.
  function moveToOwnPane(): void {
    const open = selection.ticket;
    if (!open || !board) return;
    const bridge = attachTrackerTicket(open.ref, false);
    // The ticket keeps the project it has here: a ticket pane has no board
    // to fall back on.
    openTicketPane({ ...open, project: selection.tickets.project }, { board: board.board, handoff: () => bridge.release() });
    selection.tickets.close();
  }

  // A card's Start work: its ticket opens here with its Start work form,
  // which the ticket's Work opens, so the form and the run's steps have
  // one home.
  function startWorkFrom(row: RowView): void {
    openCard(row);
    selection.tickets.askStartWork();
  }

  // A move from the board whose screen takes fields: the ticket opens with
  // that move's form.
  function moveWithFields(row: RowView, move: MoveView): void {
    if (!board) return;
    openCard(row);
    openTicketMove(trackerTicketKey({ backend: board.backend, connection: board.connection, issue: row.card.id }), move);
  }

  const choices = $derived(trackerBoardChoices());
  const computers = $derived(trackerBoardChoiceComputers());
  const board = $derived(selection.board);
  const attachment = $derived(selection.attachment);
  const pending = $derived(selection.choosing);
  // The board's computer answered what its projects list.
  const boardComputerAnswered = $derived(!!board && (computers.find((c) => c.backend === board.backend)?.state.rows ?? null) !== null);
  // The board's computer cannot be reached now. The subscription holds
  // nothing until it connects again (the entity store drops a value on its
  // computer's every connection change).
  const boardOffline = $derived(!!board && isTransportOutage(getTransportStatusFor(board.backend).status));

  // The board's connection's tracker, for the tickets it opens.

  const boardState = $derived(attachment?.current ?? null);
  const held = $derived(boardState?.held ?? null);

  // Search is the pane's find bar, filtering the board.
  let query = $state('');
  // The verified account is the connection's: any listing of it names it.
  const me = $derived(
    board
      ? choices.find((c) => c.backend === board.backend && c.choice.connection === board.connection && c.choice.verified_account)?.choice.verified_account ?? null
      : null,
  );
  // This pane's own; the next pane opens with it.
  let onlyMine = $state(savedOnlyMine());
  function setOnlyMine(on: boolean): void {
    onlyMine = on;
    saveOnlyMine(on);
  }
  // The tickets a summary row tile filters the board to, on the Board and
  // the Backlog; no filter until its first read lands. Its store loads with
  // the first tile picked, from the row's chunk.
  let ticketSet = $state.raw<BoardTicketSet | null>(null);
  let ticketSetLoad: Promise<BoardTicketSet> | null = null;
  let paneGone = false;
  function pickTicketSet(tile: TicketSetTile | null): void {
    if (ticketSet || !tile) {
      ticketSet?.pick(tile);
      return;
    }
    ticketSetLoad ??= import('../../stores/boardTicketSet.svelte').then((m) => {
      const made = m.createBoardTicketSet({ board: () => board, source: () => dashboardSource, pins: () => boardState?.pins ?? null, shown: () => selection.boardShown });
      if (paneGone) made.destroy();
      else ticketSet = made;
      return made;
    });
    ticketSetLoad.then((made) => { if (!paneGone) made.pick(tile); }, (err: unknown) => {
      ticketSetLoad = null;
      addErrorToast(`Could not filter the board to ${tile.title}: ${err instanceof Error ? err.message : String(err)}`, err);
    });
  }
  onDestroy(() => {
    paneGone = true;
    ticketSet?.destroy();
  });
  const filter: CardFilter = $derived({ mine: onlyMine ? me : null, words: searchWords(query), only: ticketSet?.tile ? ticketSet.ids : null });
  let ticketSetFailure: CapturedError | null = $state(null);
  let ticketSetFailureOf: unknown = null;
  $effect(() => {
    const err = ticketSet?.error ?? null;
    if (err === ticketSetFailureOf) return;
    ticketSetFailureOf = err;
    ticketSetFailure = err ? captureError(err, { context: 'Could not read the tickets the board is filtered to' }) : null;
  });

  const matchCount = $derived.by(() => {
    if (!showsBoard(screen) || !held || !filtering(filter)) return 0;
    let n = 0;
    const lists = screen === 'board' ? screenColumns(held) : backlogSections(held);
    for (const list of lists) for (const r of list.rows) if (cardMatches(r.card, filter)) n += 1;
    return n;
  });
  // A tool pane's id is fixed for its life (`tool-issues`).
  // svelte-ignore state_referenced_locally
  const releaseFind = registerPaneFindProvider(paneId, {
    noun: 'cards',
    result: {
      get count() { return matchCount; },
      current: -1,
      pending: false,
    },
    search: (q) => { query = q; },
    clear: () => { query = ''; },
  });
  onDestroy(releaseFind);
  let bodyEl: HTMLElement | undefined = $state();

  const hidden = $derived(board ? hiddenCardFields(board) : new Set<never>());

  function computerName(backend: string): string {
    const entry = attachedBackendEntry(backend);
    return entry ? backendDisplayName(entry) : 'A computer';
  }

  function projectName(_backend: string, projectId: string): string {
    return getProject(projectId)?.project.name ?? 'A project';
  }

  // Captured once per failure, so Details and copy describe the one shown.
  let refreshFailure: CapturedError | null = $state(null);
  let readFailure: CapturedError | null = $state(null);
  let readFailureOf: unknown = null;
  $effect(() => {
    const notice = boardState?.refreshNotice;
    refreshFailure = notice?.kind === 'failed' ? captureError(notice.error, { context: 'Could not refresh the board' }) : null;
  });
  $effect(() => {
    // A full or patch read's failure, else the first column's that failed.
    const err = boardState?.readError ?? boardState?.sectionErrors.values().next().value ?? null;
    if (err === readFailureOf) return;
    readFailureOf = err;
    readFailure = err ? captureError(err, { context: "Could not read the board from this computer's copy" }) : null;
  });
  const choiceFailures = $derived(computers.flatMap((c) => (c.state.kind === 'failed' ? [{ backend: c.backend, error: c.state.error }] : [])));
  const offlineComputers = $derived(computers.flatMap((c) => (c.state.kind === 'offline' ? [c.backend] : [])));
  // Every computer answered: only then can the pane say what the boards are.
  const allAnswered = $derived(computers.every((c) => c.state.kind === 'answered'));
  const anyListed = $derived(choices.length > 0);
  // A computer whose boards are all off and that lets this window turn a
  // connection on.
  const offTarget = $derived(choices.find((c) => !isBoardChoiceOpenable(c.choice) && scopeGrant('tracker:operate', c.backend) === 'granted')?.backend ?? null);
  let dismissedChoiceFailures = $state.raw<readonly unknown[]>([]);

  // The way to Settings > Issues > Boards, for a project on a computer
  // that lets this window change a project's boards; null otherwise.
  interface BoardsSettingsTarget { projectId: string; backend: string }
  function boardsSettingsFor(projectId: string | null, backend: string | undefined): BoardsSettingsTarget | null {
    if (!projectId || backend === undefined || scopeGrant('tracker:operate', backend) !== 'granted') return null;
    return { projectId, backend };
  }
  // The first project by name, then ID, of those whose Boards this window
  // may change: the order a computer lists its boards in.
  const firstProjectByName = $derived.by(() => {
    let best: { id: string; name: string } | null = null;
    for (const { project } of getProjects()) {
      if (project.archived || !boardsSettingsFor(project.id, projectBackend(project.id))) continue;
      if (!best || project.name < best.name || (project.name === best.name && project.id < best.id)) best = project;
    }
    return best?.id ?? null;
  });
  // No project lists a board: the project the pane was opened from, else
  // the first by name.
  const emptyTarget = $derived.by(() => {
    if (board || pending || !allAnswered || anyListed) return null;
    const projectId = (openedFrom && getProject(openedFrom) ? openedFrom : firstProjectByName) ?? null;
    return boardsSettingsFor(projectId, projectId ? projectBackend(projectId) : undefined);
  });
  const endedTarget = $derived(boardState?.ended && board ? boardsSettingsFor(board.projectId, board.backend) : null);

  function refresh(): void {
    if (screen === 'time') {
      timeView?.refresh();
      return;
    }
    if (attachment && board) {
      void refreshTrackerBoard(attachment.key);
      refreshFormReads(board.backend, board.connection);
    }
    if (screen === 'dashboards') dashboardView?.refresh();
    else rowView?.refreshLogged();
  }

  // The board a dashboard counts: its subscription once the subscribe
  // answered, and how many times the board changed since.
  const dashboardSource = $derived(
    board && boardState?.subscription ? { backend: board.backend, board, subscription: boardState.subscription, changes: boardState.changes, dashboards: boardState.dashboards } : null,
  );
  // The board's summary row: mounted while the board pins a widget or the
  // person edits the row, which a board with none starts from its Add a
  // tile. Editing is the board's: another board shows out of it.
  const rowPins = $derived(boardState?.pins ?? null);
  let rowEditedOn = $state('');
  const boardId = $derived(board ? `${board.backend}\n${board.projectId}\n${board.connection}\n${board.board}` : '');
  const rowEditing = $derived(boardId !== '' && rowEditedOn === boardId);
  const rowAddable = $derived(!!board && scopeGrant('tracker:operate', board.backend) === 'granted');

  // The board's listing: its connection's kind, whether it may be opened.
  // The board's tracker, jira or linear; empty until a listing names it.
  const boardKind = $derived(board ? choices.find((c) => c.backend === board.backend && c.choice.connection === board.connection)?.choice.connection_kind ?? '' : '');
  const boardChoice = $derived(board ? choices.find((c) => c.backend === board.backend && c.choice.connection === board.connection && c.choice.board === board.board)?.choice ?? null : null);

  function lastRead(at: number): string {
    return at > 0 ? `Last read ${relativeTime(at, 'locale', board?.backend)}.` : 'Not read yet.';
  }
</script>

{#snippet ticketSetChip()}
  {#if ticketSet?.tile}
    {@const set = ticketSet}
    {@const tile = ticketSet.tile}
    <div class="flex items-center gap-2 px-3 pt-2 text-[0.75rem]" data-testid="issues-ticket-set">
      <span class="inline-flex h-7 min-w-0 items-center gap-2 rounded-[var(--radius-control)] border border-accent bg-accent/15 pl-2.5 pr-1 text-fg-muted compact:h-8">
        <Icon icon={Funnel} size={13} strokeWidth={2} class="shrink-0" />
        {#if set.ids}
          <span class="truncate" data-testid="issues-ticket-set-text">Showing only <b class="font-semibold text-fg">{tile.title}</b> · {set.ids.size === 1 ? '1 ticket' : `${set.ids.size} tickets`}</span>
        {:else if set.incomplete}
          <span class="truncate" data-testid="issues-ticket-set-text">Showing every ticket: <b class="font-semibold text-fg">{tile.title}</b> is not known, {set.incomplete}</span>
        {:else if set.error}
          <span class="truncate" data-testid="issues-ticket-set-text">Could not find the tickets of <b class="font-semibold text-fg">{tile.title}</b></span>
        {:else}
          <span class="truncate" data-testid="issues-ticket-set-text">Finding the tickets of <b class="font-semibold text-fg">{tile.title}</b>…</span>
        {/if}
        <IconButton label="Show every ticket" size="xs" variant="tint" testId="issues-ticket-set-clear" onClick={() => set.pick(null)}>
          {#snippet children()}<Icon icon={X} size={12} strokeWidth={2.25} />{/snippet}
        </IconButton>
      </span>
    </div>
    {#if ticketSetFailure}
      <ErrorNotice message="{set.ids ? `Could not read the tickets of ${tile.title} again` : `Could not find the tickets of ${tile.title}`}: {ticketSetFailure.report.reason}" captured={ticketSetFailure} testId="issues-ticket-set-error" />
    {/if}
  {/if}
{/snippet}

<div class="flex h-full min-h-0 flex-col" data-testid="issues-pane">
  <ToolPaneHeader {paneId} kind="issues" {onPaneDragStart} onBack={compact && ticket ? () => { stepBack(); } : undefined} backTestId="issues-back">
    <div class="flex min-w-0 flex-1 items-center gap-1">
      <BoardPicker
        {choices}
        current={board}
        {computerName}
        {projectName}
        currentName={held?.shape.name || null}
        emptyLabel={pending ? 'Listing boards…' : allAnswered ? 'No boards' : 'Boards'}
        emptyText={pending ? 'Listing boards…' : allAnswered ? 'No project lists a board yet.' : 'The boards could not all be listed; see the notice under the header.'}
        onPick={(ref) => selection.pick(ref)}
        onOpen={readTrackerBoardChoices}
      />
      <IssuesTabs current={screen} folded={compact || paneWidth < TABS_MIN_PX} onPick={(next) => selection.setScreen(next)} />
      <span class="flex-1"></span>
      {#if !compact && showsBoard(screen)}
        <button
          type="button"
          class="inline-flex h-6 shrink-0 items-center gap-1 rounded-[var(--radius-control)] px-2 text-[0.75rem] transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 {onlyMine ? 'bg-accent/15 text-accent' : 'text-fg-muted hover:bg-surface-1 hover:text-fg'}"
          aria-pressed={onlyMine}
          data-testid="issues-only-mine"
          onclick={() => setOnlyMine(!onlyMine)}
        >
          <Icon icon={UserRound} size={13} strokeWidth={2} />
          <span>Only mine</span>
        </button>
      {/if}
      {#if board && showsBoard(screen)}
        <DisplayMenu
          {hidden}
          onField={(field, on) => setCardFieldShown(board, field, on)}
          onlyMine={compact ? { on: onlyMine, set: setOnlyMine } : undefined}
        />
      {/if}
      {#if showsBoard(screen)}
        <PaneHeaderIconButton label="Search" testId="issues-search" onclick={() => openPaneFind(paneId, { returnFocus: document.activeElement as HTMLElement | null })}>
          <Icon icon={Search} size={14} strokeWidth={2} />
        </PaneHeaderIconButton>
        <PaneHeaderIconButton label="Refresh the board" testId="issues-refresh" disabled={!boardState || boardState.ended} onclick={refresh}>
          <Icon icon={RefreshCw} size={13} strokeWidth={1.75} class={boardState?.refreshing ? 'animate-spin' : ''} />
        </PaneHeaderIconButton>
      {:else if screen === 'dashboards'}
        <PaneHeaderIconButton label="Refresh the dashboard" testId="issues-refresh" disabled={!boardState || boardState.ended} onclick={refresh}>
          <Icon icon={RefreshCw} size={13} strokeWidth={1.75} class={boardState?.refreshing || dashboardCounting ? 'animate-spin' : ''} />
        </PaneHeaderIconButton>
      {:else}
        <PaneHeaderIconButton label="Refresh the week" testId="issues-refresh" disabled={!timeView} onclick={refresh}>
          <Icon icon={RefreshCw} size={13} strokeWidth={1.75} class={timeRefreshing ? 'animate-spin' : ''} />
        </PaneHeaderIconButton>
      {/if}
    </div>
  </ToolPaneHeader>
  <PaneFindBar {paneId} title="Issues" fallbackFocus={() => bodyEl ?? null} />

  {#each choiceFailures as failure (failure.backend)}
    {#if !dismissedChoiceFailures.includes(failure.error)}
      <ErrorNotice
        message="Could not list the boards of {computerName(failure.backend)}: {failure.error.report.reason}"
        captured={failure.error}
        onDismiss={() => { dismissedChoiceFailures = [...dismissedChoiceFailures, failure.error]; }}
        testId="issues-choices-error"
      />
    {/if}
  {/each}
  {#if !boardCovered && (showsBoard(screen) || screen === 'dashboards')}
  {#if attachment?.error}
    <p class="border-b border-error/30 bg-error/10 px-3 py-1.5 text-xs text-error" role="alert" data-testid="issues-subscribe-error">Could not open this board: {attachment.error}</p>
  {/if}
  {#if boardState?.ended}
    <div class="flex items-center gap-2 border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" data-testid="issues-board-ended">
      <span class="min-w-0 flex-1">This board is no longer followed: its connection was turned off or removed, or no project lists it any more. Pick another board.</span>
      {#if endedTarget}
        {@const target = endedTarget}
        <button type="button" class="shrink-0 text-accent hover:underline" data-testid="issues-ended-settings" onclick={() => openTrackerBoardsSettings(target.projectId, target.backend)}>Open the project's boards</button>
      {/if}
    </div>
  {:else if boardState?.failure}
    <p class="border-b border-warning/30 bg-warning/10 px-3 py-1.5 text-xs text-warning" role="status" data-testid="issues-board-failure">{boardState.failure.message} {lastRead(boardState.readAt)}</p>
  {/if}
  {#if boardState?.refreshNotice && boardState.refreshNotice.kind !== 'failed'}
    <div class="flex items-center gap-2 border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" role="status" data-testid="issues-refresh-notice" data-kind={boardState.refreshNotice.kind}>
      <span class="min-w-0 flex-1">{boardState.refreshNotice.message}</span>
      {#if boardState.refreshNotice.kind === 'not_run'}
        <button type="button" class="shrink-0 text-accent hover:underline" data-testid="issues-refresh-again" onclick={refresh}>Refresh again</button>
      {/if}
      <IconButton label="Dismiss" size="xs" variant="tint" testId="issues-refresh-notice-dismiss" onClick={() => attachment && dismissTrackerRefreshNotice(attachment.key)}>
        {#snippet children()}<Icon icon={X} size={12} strokeWidth={2.25} />{/snippet}
      </IconButton>
    </div>
  {/if}
  {#if refreshFailure && attachment}
    <ErrorNotice
      message="Could not refresh the board: {refreshFailure.report.reason}"
      captured={refreshFailure}
      onDismiss={() => attachment && dismissTrackerRefreshNotice(attachment.key)}
      testId="issues-refresh-error"
    />
  {/if}
  {#if readFailure}
    <ErrorNotice
      message="Could not read the board from this computer's copy: {readFailure.report.reason}"
      captured={readFailure}
      onDismiss={() => { readFailure = null; }}
      testId="issues-read-error"
    />
  {/if}
  {#if board && boardOffline && !boardState?.ended}
    <p class="border-b border-border-subtle bg-surface-1 px-3 py-1.5 text-xs text-fg-muted" role="status" data-testid="issues-board-offline">{computerName(board.backend)} is not connected. The board shows when it connects.</p>
  {/if}
  {#if showsBoard(screen) && onlyMine && boardComputerAnswered && !me}
    <p class="border-b border-border-subtle px-3 py-1.5 text-xs text-fg-muted" data-testid="issues-only-mine-unknown">Only mine shows every card: the app does not know which account is yours on this board's connection.</p>
  {/if}
  {/if}

  <div class="relative flex min-h-0 flex-1" bind:clientWidth={() => paneWidth, measure}>
  <div
    class="relative flex min-h-0 flex-col {ticket && selection.placement === 'beside' ? 'shrink-0 border-r border-border-subtle' : 'min-w-0 flex-1'}"
    style:width={ticket && selection.placement === 'beside' ? `${split.width}px` : undefined}
    bind:this={bodyEl}
    tabindex="-1"
    aria-hidden={ticket && selection.placement === 'over' ? 'true' : undefined}
    inert={!!ticket && selection.placement === 'over'}
    data-testid="issues-board-region"
    data-screen={screen}
  >
    <RenderBoundary label={screen === 'time' ? 'Time' : screen === 'backlog' ? 'The backlog' : screen === 'dashboards' ? 'The dashboard' : 'The board'} testId="issues-board-render-error">
      {#if !board}
        <div class="flex flex-col items-start gap-2 px-4 py-6 text-sm text-fg-muted" data-testid="issues-empty">
          {#if pending}
            <p>Listing boards…</p>
          {:else}
            {#each offlineComputers as backend (backend)}
              <p data-testid="issues-empty-offline">{computerName(backend)} is not connected. Its boards show when it connects.</p>
            {/each}
            {#if allAnswered && !anyListed}
              <p data-testid="issues-empty-none">No project lists a board yet. A project lists its boards in Settings.</p>
              {#if emptyTarget}
                {@const target = emptyTarget}
                <button type="button" class="text-accent hover:underline" data-testid="issues-empty-settings" onclick={() => openTrackerBoardsSettings(target.projectId, target.backend)}>Choose boards in Settings</button>
              {/if}
            {:else if anyListed && choices.every((c) => !isBoardChoiceOpenable(c.choice))}
              <p data-testid="issues-empty-off">Every board listed here is on a connection that is turned off. Turn one on in Settings, then pick its board.</p>
              {#if offTarget !== null}
                {@const target = offTarget}
                <button type="button" class="text-accent hover:underline" data-testid="issues-empty-connections" onclick={() => openSettingsOverlay('tracker-connections', target)}>Open the tracker connections</button>
              {/if}
            {/if}
          {/if}
        </div>
      {:else if screen === 'time'}
        {#if !boardChoice}
          <!-- No listing names the board: its computer is not connected,
               the listing is still out, or no listing has it any more. -->
          {#if boardOffline}
            <p class="px-4 py-6 text-sm text-fg-muted" data-testid="time-offline">{timeOfflineText(computerName(board.backend))}</p>
          {:else if boardComputerAnswered || boardState?.ended}
            <p class="px-4 py-6 text-sm text-fg-muted" data-testid="time-board-gone">No project lists this board any more, or its connection was removed. Pick another board to see your time.</p>
          {:else}
            <p class="px-4 py-6 text-sm text-fg-muted" data-testid="issues-time-listing">Listing boards…</p>
          {/if}
        {:else}
          {@const choice = boardChoice}
          {@const timeBoard = board}
          {#await loadTimeScreen()}
            <p class="px-4 py-6 text-sm text-fg-muted" data-testid="time-loading">Opening Time…</p>
          {:then mod}
            <mod.default
              bind:this={timeView}
              bind:refreshing={timeRefreshing}
              backend={timeBoard.backend}
              project={timeBoard.projectId}
              connection={timeBoard.connection}
              board={timeBoard.board}
              boardChanges={boardState?.changes ?? 0}
              connectionKind={choice.connection_kind}
              connectionOn={isBoardChoiceOpenable(choice) && !boardState?.ended}
              author={me}
              computerName={computerName(timeBoard.backend)}
              offline={boardOffline}
              {compact}
              shown={selection.timeShown}
              askedWeek={selection.askedWeek}
              onAskedWeekTaken={selection.takeAskedWeek}
              openIssue={ticket?.ref.issue ?? null}
              onOpenTicket={(t) => selection.openTicket(t.issue, { key: t.key, title: t.title, card: null }, 'list')}
            />
          {:catch err}
            <p class="px-4 py-6 text-sm text-error" role="alert" data-testid="time-load-error">The Time screen could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
          {/await}
        {/if}
      {:else if held && attachment && screen === 'backlog'}
        {@const backlogBoard = board}
        {@render ticketSetChip()}
        {#await loadBacklogScreen()}
          <p class="px-4 py-6 text-sm text-fg-muted" data-testid="backlog-loading">Opening the backlog…</p>
        {:then mod}
          <mod.default
            boardKey={attachment.key}
            boardRef={backlogBoard}
            {held}
            paging={boardState!.paging}
            sectionErrors={boardState!.sectionErrors}
            {filter}
            {hidden}
            {compact}
            shown={selection.boardShown}
            openIssue={ticket?.ref.issue ?? null}
            kind={boardKind}
            onOpenCard={openCard}
            onMoveWithFields={moveWithFields}
            ticketProject={(ref) => selection.tickets.projectFor(ref)}
            onStartWork={startWorkFrom}
          />
        {:catch err}
          <p class="px-4 py-6 text-sm text-error" role="alert" data-testid="backlog-load-error">The Backlog screen could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
        {/await}
      {:else if held && dashboardSource && screen === 'dashboards'}
        {@const dashboardBoard = board}
        {#await loadDashboardScreen()}
          <p class="px-4 py-6 text-sm text-fg-muted" data-testid="dashboard-loading">Opening the dashboard…</p>
        {:then mod}
          <mod.default
            bind:this={dashboardView}
            bind:counting={dashboardCounting}
            source={dashboardSource}
            pins={rowPins}
            boardName={boardChoice?.name ?? 'this board'}
            shown={selection.dashboardsShown}
            {compact}
            computerName={computerName(dashboardBoard.backend)}
            onOpenTicket={(row) => selection.openTicket(row.id, { key: row.key, title: row.title, card: null }, 'list')}
          />
        {:catch err}
          <p class="px-4 py-6 text-sm text-error" role="alert" data-testid="dashboard-load-error">The Dashboards screen could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
        {/await}
      {:else if held && attachment && screen !== 'dashboards'}
        {#if rowPins && (rowPins.length > 0 || rowEditing) && dashboardSource}
          {@const rowSource = dashboardSource}
          {@const pins = rowPins}
          {#await loadSummaryRow() then mod}
            <mod.default
              bind:this={rowView}
              source={rowSource}
              {pins}
              shown={selection.boardShown}
              editing={rowEditing}
              onEditing={(on) => { rowEditedOn = on ? boardId : ''; }}
              adding={pins.length === 0}
              filteredTo={ticketSet?.tile?.widget ?? ''}
              onFilter={pickTicketSet}
              onOpenDashboards={() => selection.setScreen('dashboards')}
            />
          {:catch err}
            <p class="border-b border-border-subtle px-3 py-1.5 text-xs text-error" role="alert" data-testid="board-row-load-error">The board's summary row could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
          {/await}
        {:else if rowPins && dashboardSource && rowAddable}
          <div class="flex border-b border-border-subtle px-3 py-1" data-testid="board-row-empty">
            <button
              type="button"
              class="inline-flex h-6 items-center gap-1 rounded-[var(--radius-control)] px-1.5 text-[0.75rem] text-fg-muted hover:bg-surface-1 hover:text-fg focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:h-8 compact:select-none"
              title="Show dashboard numbers above the board"
              data-testid="board-row-start"
              onclick={() => { rowEditedOn = boardId; }}
            >
              <Icon icon={Plus} size={13} strokeWidth={2} />
              Add a tile
            </button>
          </div>
        {/if}
        {@render ticketSetChip()}
        <BoardScreen
          boardKey={attachment.key}
          boardRef={board}
          {held}
          paging={boardState!.paging}
          sectionErrors={boardState!.sectionErrors}
          {filter}
          {hidden}
          {compact}
          shown={selection.boardShown}
          openIssue={ticket?.ref.issue ?? null}
          typed={boardKind !== 'linear'}
          onOpenCard={openCard}
          onMoveWithFields={moveWithFields}
          ticketProject={(ref) => selection.tickets.projectFor(ref)}
          onStartWork={startWorkFrom}
        />
      {:else if boardState && !boardState.ended}
        <p class="px-4 py-6 text-sm text-fg-muted" data-testid="issues-not-read">This computer has not read the board yet. It appears here once it has.</p>
      {:else if !boardState && !attachment?.error && !boardOffline}
        <p class="px-4 py-6 text-sm text-fg-muted" data-testid="issues-opening">Opening the board…</p>
      {/if}
    </RenderBoundary>
    {#if ticket && selection.placement === 'beside'}
      <SplitResizer column={split} testId="issues-ticket-resize" />
    {/if}
  </div>
  {#if ticket && board}
    {@const openBoard = board}
    <div
      class={selection.placement === 'over' ? 'absolute inset-0 z-[3] flex min-h-0 flex-col bg-surface-0' : 'flex min-h-0 min-w-0 flex-1 flex-col'}
      data-testid="issues-ticket"
      data-placement={selection.placement}
    >
      <TicketHost
        bind:this={ticketView}
        trail={selection.tickets}
        board={openBoard.board}
        {compact}
        placement={besideFits ? { value: selection.placement, set: (p) => setTicketPlacement(screen, p) } : undefined}
        onOwnPane={moveToOwnPane}
        hostPaneId={paneId}
      />
    </div>
  {/if}
  </div>
</div>
