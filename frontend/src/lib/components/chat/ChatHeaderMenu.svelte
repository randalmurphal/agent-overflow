<script lang="ts">
  // The chat header's Thread actions menu: the last control of the cluster
  // on desktop and under compact. It holds the actions that need no
  // standing button: the drawer terminal, a fresh terminal pane, find in
  // this thread, title regeneration and opening the project in an editor.
  //
  // Under compact it also carries the rows for the controls the phone
  // header has no room for (review, the PR, take-control, git) and the
  // search across every thread, which the phone has no chord for; every
  // other row is the same on both layouts. The git rows are GitActionsControl's
  // own menu, opened from here (`openMenu`): the control mounts beside the
  // button without its split trigger, so its dialogs live outside this menu
  // and survive its close, and it still shows its status error and sign-in
  // warning.
  //
  // The menu drops from its button on both layouts, never as a bottom
  // sheet (docs/decisions.md, Phone menus).
  import EllipsisVertical from '@lucide/svelte/icons/ellipsis-vertical';
  import { threadMachine } from '../../stores/attachedBackends.svelte';
  import { ensureEditorsLoaded, getAvailableEditors, getResolvedEditor } from '../../stores/editors.svelte';
  import { chordHintForCommand } from '../../stores/keybindings.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { openMessageSearch } from '../../stores/messageSearch.svelte';
  import { hasPaneFindProvider } from '../../stores/paneFind.svelte';
  import { openInEditor } from '../../stores/openInEditor';
  import { getProject } from '../../stores/projects.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { openTerminalThread, terminalPaneReachable } from '../../stores/threadCreation.svelte';
  import { regenerateThreadTitle, titleGenerationPending } from '../../stores/threadTitleGeneration.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { threadScopeGrant } from '../../transport/entityScopes';
  import { grantBlockedTitle } from '../../transport/scopeRefusal';
  import { hasScope } from '../../transport/scopes';
  import { errString } from '../../utils/errors';
  import { handleExternalURL, safeExternalURL } from '../../utils/externalLinks';
  import { openPaneFindForReader, paneFindSeed } from '../../utils/paneFindSeed';
  import { forgeLabels } from '../../utils/forgeLabels';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';
  import GitActionsControl from '../git/GitActionsControl.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import { buttonClasses } from '../primitives/buttonClasses';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { runTerminalToggle } from '../terminal/terminalToggle';
  import StatusMark from '../issues/StatusMark.svelte';
  import { linkedTicketStateNote, openLinkedTicket, removeThreadTicketLink } from '../../stores/threadTicketLinks';
  import { openPrReview, takeControlState, terminalGrant, toggleTakeControl, toggleWorkspaceReview } from './headerPaneActions';

  let { pane }: { pane: ThreadPane } = $props();

  let compact = $derived(isCompactLayout());
  let open = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let gitControl: GitActionsControl | undefined = $state(undefined);

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl });
  }

  function toggle(event: MouseEvent): void {
    // A keyboard activation is a click with no press behind it (detail 0),
    // so a snapshot left by a press that never became a click is not its.
    const seed = event.detail > 0 ? pressSeed : undefined;
    pressSeed = undefined;
    if (open) {
      close();
      return;
    }
    openSeed = seed;
    editorRows = readEditorRows();
    open = true;
  }

  // A pick closes the menu the way Escape does, handing focus back to the
  // button before the action runs, so an action that takes focus itself
  // (the terminal, message search, the git menu) still ends up with it and
  // one that does not leaves the keyboard on the button, not on <body>.
  function pick(action: () => void): void {
    close();
    action();
  }

  // The terminal rows and Take control ride one grant (terminalGrant).
  let terminalAccess = $derived(terminalGrant(pane));
  let terminalUngranted = $derived(terminalAccess !== 'granted');
  let terminalTitle = $derived(grantBlockedTitle(terminalAccess));
  // A row names its command's chord, read from the loaded keybindings. The
  // phone has no chords, so its rows name none.
  function chordFor(commandId: string): string | undefined {
    return compact ? undefined : (chordHintForCommand(commandId) ?? undefined);
  }

  // A new terminal pane roots at the thread's project. Where it could not
  // be returned to (terminalPaneReachable: the phone, for a thread outside
  // every project) there is no row.
  let newTerminalPaneOffered = $derived(terminalPaneReachable(pane.thread?.projectId));
  let takeControl = $derived(takeControlState(pane));

  function openNewTerminalPane(): void {
    void openTerminalThread({
      projectId: pane.thread?.projectId,
      cwd: pane.thread?.workspacePath,
    });
  }

  // This pane's own find bar, the one ChatView registers for its timeline
  // (ThreadPaneFind) and `find.in-pane` opens for the focused pane. A thread
  // with no find (a draft, a host that predates the find contract) has no
  // row.
  let findAvailable = $derived(hasPaneFindProvider(pane.paneId));

  // The find's seed is the reader's selection, and a press on the button
  // takes focus from a composer selection before a row could read it. So
  // the press reads it (`pressSeed`) and the click that press ends in hands
  // it to the menu it opens (`openSeed`, set on every open). A menu opened
  // from the keyboard has none and reads the selection when the row runs.
  // A press that closes the menu, or one on a pane with no find, has no
  // row to seed and reads nothing.
  let pressSeed: string | undefined;
  let openSeed: string | undefined;

  function readPressSeed(): void {
    pressSeed = !open && findAvailable ? paneFindSeed(pane.paneId) : undefined;
  }

  function findInThread(): void {
    const seed = openSeed;
    pick(() => openPaneFindForReader(pane.paneId, seed));
  }

  // Pending is thread-entity state (stores/threadTitleGeneration): the run
  // outlives this menu and its completion arrives as an event, so the row
  // stays inert until then and ChatHeader shows the spinner beside the
  // title. RegenerateThreadTitle runs provider CLIs on the host under
  // `threads:operate`. A draft has no conversation to title from, so it
  // has no row.
  let regeneratePending = $derived(pane.threadId ? titleGenerationPending(pane.threadId) : false);
  let regenerateGrant = $derived(threadScopeGrant('threads:operate', pane.threadId, pane.thread?.projectId));

  function regenerateTitle(): void {
    if (pane.threadId) void regenerateThreadTitle(pane.threadId);
  }

  // Open in editor acts on the host desktop, at the project root. One row
  // per installed editor, the default first; picking another opens there
  // this once and leaves the saved default (Settings) alone. A row that
  // names an editor opens that one. Before the catalog loads there is one
  // unnamed row, with an empty editor id that lets the backend resolve the
  // default itself.
  //
  // The rows are read when the menu opens and stay as they are until it
  // closes: a catalog or a default that changes under an open menu would
  // otherwise move or rename the row the keyboard is on.
  //
  // projectId is a primitive first so the project lookup re-runs only when
  // the id changes, not on every row sync that mints a new thread object.
  let projectId = $derived(pane.thread?.projectId ?? null);
  let projectPath = $derived(projectId ? (getProject(projectId)?.project.path ?? null) : null);
  let backend = $derived(threadMachine(pane.threadId ?? '', pane.thread?.projectId));
  let canOpenInEditor = $derived(projectPath !== null && hasScope('host', backend));
  interface EditorRow { id: string; label: string }
  let editorRows: EditorRow[] = $state.raw([]);

  function readEditorRows(): EditorRow[] {
    if (!canOpenInEditor) return [];
    const resolved = getResolvedEditor(backend);
    const rows = [{ id: resolved?.id ?? '', label: resolved ? `Open in ${resolved.name}` : 'Open in editor' }];
    for (const editor of getAvailableEditors(backend)) {
      if (editor.id !== resolved?.id) rows.push({ id: editor.id, label: `Open in ${editor.name}` });
    }
    return rows;
  }

  $effect(() => {
    if (canOpenInEditor) void ensureEditorsLoaded(backend);
  });

  async function openIn(editorId: string): Promise<void> {
    const target = backend;
    if (projectPath === null) return;
    try {
      await openInEditor(target, projectPath, 0, 0, '', editorId);
    } catch (err) {
      addErrorToast(errString(err), err);
    } finally {
      // An open is a useful revalidation edge once the shared catalog
      // snapshot passes its TTL.
      void ensureEditorsLoaded(target);
    }
  }

  // Compact rows.
  let hasWorkspace = $derived(pane.workspace !== null);
  let gitStatus = $derived(pane.gitStatus.status);
  let gitAvailable = $derived(
    hasWorkspace && (pane.gitStatus.statusError !== null || (gitStatus?.isRepo ?? false)),
  );
  let diffSuffix = $derived(`+${gitStatus?.insertions ?? 0} −${gitStatus?.deletions ?? 0}`);
  let ticketLink = $derived(pane.thread?.ticketLink ?? null);
  // ClearThreadTicket rides threads:operate on the thread's computer.
  let ticketGrant = $derived(threadScopeGrant('threads:operate', pane.threadId, pane.thread?.projectId));
  let prHref = $derived(safeExternalURL(gitStatus?.openPrUrl));
  let prLabel = $derived.by(() => {
    const labels = forgeLabels(gitStatus?.forge);
    return gitStatus?.openPrNumber
      ? `${labels.noun} ${labels.numberSigil}${gitStatus.openPrNumber}`
      : labels.noun;
  });
</script>

{#if compact}
  <GitActionsControl {pane} trigger={false} menuAnchor={triggerEl} bind:this={gitControl} />
{/if}
<!-- A native button, not the Button primitive: a menu trigger says it has
     a popup and whether it is open, where the primitive's `pressed` would
     announce a toggle. -->
<button
  bind:this={triggerEl}
  type="button"
  aria-label="Thread actions"
  aria-haspopup="menu"
  aria-expanded={open}
  title="Thread actions"
  onpointerdown={readPressSeed}
  onclick={toggle}
  data-testid="chat-header-more"
  class={buttonClasses({ variant: 'secondary', size: 'xs', pressed: open, class: 'shrink-0 w-6 compact:w-9 px-0' })}
>
  <Icon icon={EllipsisVertical} size={12} strokeWidth={2} class="opacity-90" />
</button>
<Popover anchor={triggerEl} {open} onClose={close} placement="bottom-end" role="none">
  {#snippet children()}
    <Menu ariaLabel="Thread actions" onClose={close}>
      {#if compact && hasWorkspace}
        <MenuItem
          label="Review changes"
          suffix={diffSuffix}
          checkbox
          checked={pane.showReviewPane}
          onSelect={() => pick(() => toggleWorkspaceReview(pane))}
        />
      {/if}
      {#if compact && ticketLink && pane.thread}
        {@const thread = pane.thread}
        <!-- The phone header has no room for the ticket's button: the
             ticket is this row, above the pull request
             (docs/specs/issues.md, "Compact layout"). -->
        <MenuItem
          label={ticketLink.key}
          suffix={ticketLink.title}
          title={linkedTicketStateNote(ticketLink) || undefined}
          testId="chat-header-menu-ticket"
          onSelect={() => pick(() => void openLinkedTicket(thread))}
        >
          {#snippet icon()}<span class={ticketLink.state === 'current' ? '' : 'opacity-50'}><StatusMark group={ticketLink.statusGroup} size={11} /></span>{/snippet}
        </MenuItem>
      {/if}
      {#if compact && prHref}
        <MenuItem
          label={prLabel}
          onSelect={() => pick(() => {
            if (!openPrReview(pane) && prHref) void handleExternalURL(prHref);
          })}
        />
      {/if}
      <MenuItem
        label="Terminal"
        kbd={chordFor('terminal.toggle')}
        checkbox
        checked={pane.showTerminal}
        disabled={terminalUngranted}
        title={terminalTitle}
        onSelect={() => pick(() => runTerminalToggle(pane))}
      />
      {#if newTerminalPaneOffered}
        <MenuItem
          label="New terminal pane"
          kbd={chordFor('terminal.newPane')}
          disabled={terminalUngranted}
          title={terminalTitle}
          onSelect={() => pick(openNewTerminalPane)}
        />
      {/if}
      {#if compact && takeControl.offered}
        {@const grant = takeControl.grant}
        <MenuItem
          label="Take control"
          checkbox
          checked={takeControl.open}
          disabled={grant !== 'granted'}
          title={grantBlockedTitle(grant)}
          onSelect={() => pick(() => toggleTakeControl(pane))}
        />
      {/if}
      <!-- No Browser row under compact: the companion is a native view on
           the host, which the compact screen never shows
           (docs/specs/remote-access.md, "Hidden on the phone"), and the
           store never opens it there. -->
      {#if findAvailable}
        <MenuItem
          label="Find in thread"
          kbd={chordFor('find.in-pane')}
          onSelect={findInThread}
        />
      {/if}
      {#if compact}
        <MenuItem label="Search messages" onSelect={() => pick(() => openMessageSearch(pane.paneId))} />
      {/if}
      {#if pane.threadId}
        <MenuItem
          label="Regenerate title"
          disabled={regeneratePending || regenerateGrant !== 'granted'}
          title={grantBlockedTitle(regenerateGrant)}
          onSelect={() => pick(regenerateTitle)}
        />
      {/if}
      {#if ticketLink && pane.thread}
        {@const thread = pane.thread}
        <MenuDivider />
        <MenuItem
          label="Remove ticket link"
          testId="chat-header-menu-ticket-remove"
          disabled={ticketGrant !== 'granted'}
          title={grantBlockedTitle(ticketGrant)}
          onSelect={() => pick(() => void removeThreadTicketLink(thread))}
        />
      {/if}
      {#if editorRows.length > 0}
        <MenuDivider />
        {#each editorRows as editor (editor.id)}
          <MenuItem label={editor.label} onSelect={() => pick(() => void openIn(editor.id))} />
        {/each}
      {/if}
      {#if compact && gitAvailable}
        <MenuDivider />
        <MenuItem label="Git actions…" onSelect={() => pick(() => gitControl?.openMenu())} />
      {/if}
    </Menu>
  {/snippet}
</Popover>
