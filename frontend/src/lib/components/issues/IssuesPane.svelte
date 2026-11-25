<script module lang="ts">
  import type { SplitSpec } from '../shared/splitWidth.svelte';

  /** The list beside a ticket keeps 520, the ticket 380; the split starts
   * at half. */
  const TICKET_SPLIT: SplitSpec = { listMin: 520, listMax: Infinity, detailMin: 380, initial: (body) => Math.floor(body / 2) };
  /** A ticket shows beside the list only in a pane this wide. */
  const BESIDE_MIN_PX = 900;
  /** The tabs fold into one picker in a pane narrower than this. */
  const TABS_MIN_PX = 640;

  // The ticket's components load on its first open, once per page.
  let ticketScreen: Promise<typeof import('./TicketScreen.svelte')> | null = null;
  function loadTicketScreen(): Promise<typeof import('./TicketScreen.svelte')> {
    ticketScreen ??= import('./TicketScreen.svelte');
    return ticketScreen;
  }

  // The Time screen loads on its first switch to Time, once per page.
  let timeScreen: Promise<typeof import('./TimeScreen.svelte')> | null = null;
  function loadTimeScreen(): Promise<typeof import('./TimeScreen.svelte')> {
    timeScreen ??= import('./TimeScreen.svelte');
    return timeScreen;
  }
</script>

<script lang="ts">
  // The Issues tool pane (docs/specs/issues.md, "Pane", "Board" and
  // "Time"): one app-wide pane over every project's boards on every
  // attached computer. The header holds the board picker, the screens
  // (Board and Time, folded into one picker in a narrow pane and under
  // compact), and on the Board Only mine, Display and Search, and Refresh;
  // under compact Only mine moves into the Display menu. Time shows the
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
  // ticket.
  import { onDestroy } from 'svelte';
  import ContextMenu from '../primitives/ContextMenu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import SplitResizer from '../shared/SplitResizer.svelte';
  import { SplitWidth } from '../shared/splitWidth.svelte';
  import type { RowView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
  import { registerIssuesBackHandler } from '../../stores/issuesCommands';
  import { registerToolDetailProbe } from '../../stores/toolPanes.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Search from '@lucide/svelte/icons/search';
  import UserRound from '@lucide/svelte/icons/user-round';
  import X from '@lucide/svelte/icons/x';
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
  } from '../../stores/issuesPane.svelte';
  import { dismissTrackerRefreshNotice, refreshTrackerBoard } from '../../stores/trackerBoard.svelte';
  import {
    isBoardChoiceOpenable,
    readTrackerBoardChoices,
    trackerBoardChoiceComputers,
    trackerBoardChoices,
  } from '../../stores/trackerBoardChoices.svelte';
  import { cardMatches, filtering, screenColumns, searchWords, type CardFilter } from '../../stores/trackerBoardModel';
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
  const splits = { board: new SplitWidth('issuesTicketSplit:board', TICKET_SPLIT), time: new SplitWidth('issuesTicketSplit:time', TICKET_SPLIT) };
  function measure(width: number): void {
    paneWidth = width;
    splits.board.paneWidth = width;
    splits.time.paneWidth = width;
  }
  const besideFits = $derived(!compact && paneWidth >= BESIDE_MIN_PX);
  const selection = createBoardSelection({ openedFromProject: openedFrom, shown: () => shown, besideFits: () => besideFits });
  onDestroy(() => selection.destroy());
  const screen = $derived(selection.screen);
  const split = $derived(splits[screen]);
  // The Time screen open now, for Refresh, and whether its refresh is out.
  let timeView: { refresh(): void } | undefined = $state();
  let timeRefreshing = $state(false);
  const ticket = $derived(selection.ticket);
  // A ticket over the board covers the board's notices with it: what they
  // say is the board's, and the ticket says its own.
  const boardCovered = $derived(!!ticket && selection.placement === 'over');

  // The ticket view open now, for its own step back (the Details rail).
  let ticketView: { stepBack(): boolean } | undefined = $state();
  function stepBack(): boolean {
    if (!selection.ticket) return false;
    if (!ticketView?.stepBack()) selection.stepBack();
    return true;
  }
  const disposeProbe = registerToolDetailProbe('issues', () => selection.ticket !== null);
  const disposeBack = registerIssuesBackHandler(stepBack);
  onDestroy(() => {
    disposeBack();
    disposeProbe();
  });

  function openCard(row: RowView): void {
    selection.openTicket(row.card.id, { key: row.card.key, title: row.card.title, card: row.card }, 'list');
  }

  let cardMenu = $state<{ x: number; y: number; row: RowView } | null>(null);
  function openCardMenu(row: RowView, event: MouseEvent): void {
    cardMenu = { x: event.clientX, y: event.clientY, row };
  }
  // A menu row acts on the card the menu was opened on, taken before the
  // menu closes.
  function fromCardMenu(action: (row: RowView) => void): void {
    const open = cardMenu;
    cardMenu = null;
    if (open) action(open.row);
  }
  async function copyKey(key: string): Promise<void> {
    if (!(await copyToClipboard(key))) addToast('error', 'Could not copy the key.');
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
  const filter: CardFilter = $derived({ mine: onlyMine ? me : null, words: searchWords(query) });
  const matchCount = $derived.by(() => {
    if (screen !== 'board' || !held || !filtering(filter)) return 0;
    let n = 0;
    for (const col of screenColumns(held)) for (const r of col.rows) if (cardMatches(r.card, filter)) n += 1;
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
    if (screen === 'time') timeView?.refresh();
    else if (attachment) void refreshTrackerBoard(attachment.key);
  }

  // The board's listing: its connection's kind, whether it may be opened.
  const boardChoice = $derived(board ? choices.find((c) => c.backend === board.backend && c.choice.connection === board.connection && c.choice.board === board.board)?.choice ?? null : null);

  function lastRead(at: number): string {
    return at > 0 ? `Last read ${relativeTime(at, 'locale', board?.backend)}.` : 'Not read yet.';
  }
</script>

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
      {#if !compact && screen === 'board'}
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
      {#if board && screen === 'board'}
        <DisplayMenu
          {hidden}
          onField={(field, on) => setCardFieldShown(board, field, on)}
          onlyMine={compact ? { on: onlyMine, set: setOnlyMine } : undefined}
        />
      {/if}
      {#if screen === 'board'}
        <PaneHeaderIconButton label="Search" testId="issues-search" onclick={() => openPaneFind(paneId, { returnFocus: document.activeElement as HTMLElement | null })}>
          <Icon icon={Search} size={14} strokeWidth={2} />
        </PaneHeaderIconButton>
        <PaneHeaderIconButton label="Refresh the board" testId="issues-refresh" disabled={!boardState || boardState.ended} onclick={refresh}>
          <Icon icon={RefreshCw} size={13} strokeWidth={1.75} class={boardState?.refreshing ? 'animate-spin' : ''} />
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
  {#if !boardCovered && screen === 'board'}
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
  {#if onlyMine && boardComputerAnswered && !me}
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
    <RenderBoundary label={screen === 'time' ? 'Time' : 'The board'} testId="issues-board-render-error">
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
              connection={timeBoard.connection}
              connectionKind={choice.connection_kind}
              connectionOn={isBoardChoiceOpenable(choice) && !boardState?.ended}
              author={me}
              computerName={computerName(timeBoard.backend)}
              offline={boardOffline}
              {compact}
              shown={selection.timeShown}
              openIssue={ticket?.ref.issue ?? null}
              onOpenTicket={(t) => selection.openTicket(t.issue, { key: t.key, title: t.title, card: null }, 'list')}
            />
          {:catch err}
            <p class="px-4 py-6 text-sm text-error" role="alert" data-testid="time-load-error">The Time screen could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
          {/await}
        {/if}
      {:else if held && attachment}
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
          onOpenCard={openCard}
          onCardMenu={openCardMenu}
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
  {#if ticket && board && selection.ticketAttachment}
    {@const held = selection.ticketAttachment}
    {@const open = ticket}
    {@const openBoard = board}
    <div
      class={selection.placement === 'over' ? 'absolute inset-0 z-[3] flex min-h-0 flex-col bg-surface-0' : 'flex min-h-0 min-w-0 flex-1 flex-col'}
      data-testid="issues-ticket"
      data-placement={selection.placement}
    >
      <RenderBoundary label="The ticket" testId="issues-ticket-render-error">
        {#await loadTicketScreen()}
          <p class="px-4 py-6 text-sm text-fg-muted" data-testid="ticket-loading">Opening the ticket…</p>
        {:then mod}
          {#key held.key}
            <mod.default
              bind:this={ticketView}
              attachment={held}
              seed={open.seed}
              board={openBoard.board}
              kind={choices.find((c) => c.backend === openBoard.backend && c.choice.connection === openBoard.connection)?.choice.connection_kind ?? ''}
              computerName={computerName(openBoard.backend)}
              offline={boardOffline}
              {compact}
              trail={selection.trail}
              placement={selection.placement}
              {besideFits}
              onPlacement={(p) => setTicketPlacement(screen, p)}
              onBack={() => selection.stepBack()}
              onClose={() => selection.closeTicket()}
              onOpenIssue={(issue, seed) => selection.openTicket(issue, seed, 'ticket')}
            />
          {/key}
        {:catch err}
          <p class="px-4 py-6 text-sm text-error" role="alert" data-testid="ticket-load-error">The ticket screen could not load: {err instanceof Error ? err.message : String(err)}. Reload the window to try again.</p>
        {/await}
      </RenderBoundary>
    </div>
  {/if}
  </div>
</div>

{#if cardMenu}
  <ContextMenu x={cardMenu.x} y={cardMenu.y} ariaLabel="Card actions" onDismiss={() => { cardMenu = null; }}>
    <MenuItem label="Open" testId="issues-card-menu-open" onSelect={() => fromCardMenu(openCard)} />
    <MenuItem label="Copy key" testId="issues-card-menu-copy-key" onSelect={() => fromCardMenu((row) => { void copyKey(row.card.key); })} />
  </ContextMenu>
{/if}
