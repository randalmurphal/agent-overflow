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
  import { isCompanionOpen } from '../../stores/companionPanes.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { openTerminalThread } from '../../stores/threadCreation.svelte';
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
  import { openPrReview, toggleTakeControl, toggleWorkspaceReview } from './headerPaneActions';

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

  // The drawer terminal, a fresh terminal thread and the take-control
  // mirror all open a PTY on the thread's computer under `terminal:operate`:
  // the same gate the palette's terminal commands read. Inert with a reason
  // rather than absent, like every other ungranted control.
  let terminalGrant = $derived(threadScopeGrant('terminal:operate', pane.threadId, pane.thread?.projectId));
  let terminalUngranted = $derived(terminalGrant !== 'granted');
  let terminalTitle = $derived(grantBlockedTitle(terminalGrant));
  // A row names its command's chord, read from the loaded keybindings. The
  // phone has no chords, so its rows name none.
  function chordFor(commandId: string): string | undefined {
    return compact ? undefined : (chordHintForCommand(commandId) ?? undefined);
  }

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
  let pressSeed: string | undefined;
  let openSeed: string | undefined;

  function findInThread(): void {
    const seed = openSeed;
    pick(() => openPaneFindForReader(pane.paneId, seed));
  }

  // Pending is thread-entity state (stores/threadTitleGeneration): the run
  // outlives this menu and its completion arrives as an event, so the row
  // stays inert until then and ChatHeader shows the spinner beside the
  // title. RegenerateThreadTitle runs provider CLIs on the host under
  // `threads:operate`.
  let regeneratePending = $derived(pane.threadId ? titleGenerationPending(pane.threadId) : false);
  let regenerateGrant = $derived(threadScopeGrant('threads:operate', pane.threadId, pane.thread?.projectId));

  function regenerateTitle(): void {
    if (pane.threadId) void regenerateThreadTitle(pane.threadId);
  }

  // Open in editor acts on the host desktop, at the project root. One row
  // per installed editor, the default first; picking another opens there
  // this once and leaves the saved default (Settings) alone. The row works
  // before the catalog loads: an empty editor id lets the backend resolve
  // the default itself.
  //
  // projectId is a primitive first so the project lookup re-runs only when
  // the id changes, not on every row sync that mints a new thread object.
  let projectId = $derived(pane.thread?.projectId ?? null);
  let projectPath = $derived(projectId ? (getProject(projectId)?.project.path ?? null) : null);
  let backend = $derived(threadMachine(pane.threadId ?? '', pane.thread?.projectId));
  let canOpenInEditor = $derived(projectPath !== null && hasScope('host', backend));
  let editorRows = $derived.by((): { id: string; label: string }[] => {
    if (!canOpenInEditor) return [];
    const available = getAvailableEditors(backend);
    const resolved = getResolvedEditor(backend);
    if (available.length < 2) {
      return [{ id: '', label: resolved ? `Open in ${resolved.name}` : 'Open in editor' }];
    }
    return [
      ...available.filter((editor) => editor.id === resolved?.id),
      ...available.filter((editor) => editor.id !== resolved?.id),
    ].map((editor) => ({ id: editor.id, label: `Open in ${editor.name}` }));
  });

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
  let prHref = $derived(safeExternalURL(gitStatus?.openPrUrl));
  let prLabel = $derived.by(() => {
    const labels = forgeLabels(gitStatus?.forge);
    return gitStatus?.openPrNumber
      ? `${labels.noun} ${labels.numberSigil}${gitStatus.openPrNumber}`
      : labels.noun;
  });
  let isClaudeTui = $derived(pane.thread?.provider === 'claude-tui');
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
  onpointerdown={() => (pressSeed = paneFindSeed(pane.paneId))}
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
          checked={pane.showReviewPane}
          onSelect={() => pick(() => toggleWorkspaceReview(pane))}
        />
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
        checked={pane.showTerminal}
        disabled={terminalUngranted}
        title={terminalTitle}
        onSelect={() => pick(() => runTerminalToggle(pane))}
      />
      <MenuItem
        label="New terminal pane"
        kbd={chordFor('terminal.newPane')}
        disabled={terminalUngranted}
        title={terminalTitle}
        onSelect={() => pick(openNewTerminalPane)}
      />
      {#if compact && isClaudeTui}
        <MenuItem
          label="Take control"
          checked={isCompanionOpen(pane.paneId, 'take-control')}
          disabled={terminalUngranted}
          title={terminalTitle}
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
      <MenuItem
        label="Regenerate title"
        disabled={regeneratePending || regenerateGrant !== 'granted'}
        title={grantBlockedTitle(regenerateGrant)}
        onSelect={() => pick(regenerateTitle)}
      />
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
