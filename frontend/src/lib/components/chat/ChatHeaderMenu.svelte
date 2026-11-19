<script lang="ts">
  // The chat header's Thread actions menu: the last control of the cluster
  // on desktop and under compact. It holds the actions that need no
  // standing button: the drawer terminal, a fresh terminal pane, message
  // search, title regeneration and opening the project in an editor.
  //
  // Under compact it also carries the rows for the controls the phone
  // header has no room for (review, the PR, take-control, git); every other
  // row is the same on both layouts. The git rows are GitActionsControl's
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
  import { openInEditor } from '../../stores/openInEditor';
  import { isCompanionOpen } from '../../stores/companionPanes.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { openTerminalThread } from '../../stores/threadCreation.svelte';
  import { regenerateThreadTitle, titleGenerationPending } from '../../stores/threadTitleGeneration.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import { threadHasScope } from '../../transport/entityScopes';
  import { hasScope } from '../../transport/scopes';
  import { errString } from '../../utils/errors';
  import { handleExternalURL, safeExternalURL } from '../../utils/externalLinks';
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

  const UNGRANTED = 'Not granted to this device';

  let compact = $derived(isCompactLayout());
  let open = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let gitControl: GitActionsControl | undefined = $state(undefined);

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl });
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
  let terminalUngranted = $derived(!threadHasScope('terminal:operate', pane.threadId, pane.thread?.projectId));
  let terminalTitle = $derived(terminalUngranted ? UNGRANTED : undefined);
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

  // Pending is thread-entity state (stores/threadTitleGeneration): the run
  // outlives this menu and its completion arrives as an event, so the row
  // stays inert until then and ChatHeader shows the spinner beside the
  // title. RegenerateThreadTitle runs provider CLIs on the host under
  // `threads:operate`.
  let regeneratePending = $derived(pane.threadId ? titleGenerationPending(pane.threadId) : false);
  let regenerateUngranted = $derived(!threadHasScope('threads:operate', pane.threadId, pane.thread?.projectId));

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
  onclick={() => (open = !open)}
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
      <MenuItem
        label="Search messages"
        kbd={chordFor('search.messages')}
        onSelect={() => pick(() => openMessageSearch(pane.paneId))}
      />
      <MenuItem
        label="Regenerate title"
        disabled={regeneratePending || regenerateUngranted}
        title={regenerateUngranted ? UNGRANTED : undefined}
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
