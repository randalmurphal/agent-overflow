<script lang="ts">
  // Chat header: thread title + project chip on the left, action cluster
  // on the right (Open-in-editor, git actions, terminal toggle, diff
  // toggle). Thread mode (chat vs design) is read from `pane.thread.mode`
  // and surfaced by the composer's ThreadModePicker; nothing here needs
  // to render mode chrome.
  //
  // The inline-rename flow below mirrors the behavior the old
  // ChatView.svelte header used: click the title to switch to an input,
  // Enter to submit (RenameThread), Escape / blur to cancel.

  import { tick } from 'svelte';
  import FolderClosed from 'lucide-svelte/icons/folder-closed';
  import SquareTerminal from 'lucide-svelte/icons/square-terminal';
  import Diff from 'lucide-svelte/icons/diff';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import X from 'lucide-svelte/icons/x';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import {
    RenameThread,
    GetThread,
    OpenInEditor,
  } from '../../stores/bindings';
  import { destroyPane, getFocusedPaneId, syncThread } from '../../stores/panes.svelte';
  import { errString } from '../../utils/errors';
  import { getProject } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { formatChord, keybindingForCommand } from '../../stores/keybindings.svelte';
  import { resolvePaneAttentionDot } from '../panes/paneAttention';
  import GitActionsControl from '../git/GitActionsControl.svelte';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';

  interface Props {
    pane: ThreadPane;
    onPaneDragStart?: (event: DragEvent) => void;
  }

  let { pane, onPaneDragStart }: Props = $props();
  let attentionDot = $derived(resolvePaneAttentionDot(pane.thread ?? null));
  let titleGlow = $derived(attentionDot?.pill.glowClass ?? '');
  let isFocusedPane = $derived(getFocusedPaneId() === pane.paneId);
  let isDesignThread = $derived(pane.thread?.mode === 'design');

  let terminalToggleChord = $derived(
    formatChord(keybindingForCommand('terminal.toggle') ?? 'mod+`'),
  );
  let diffPanelToggleChord = $derived(
    formatChord(keybindingForCommand('diff.panel.toggle') ?? 'mod+shift+g'),
  );

  // Inline-rename state. Mirrors the old header's pattern — local string
  // buffer + editing toggle so Svelte's input can treat the field as
  // controlled without disturbing the pane state.
  let editing = $state(false);
  let draftTitle = $state('');
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let renamePending = $state(false);

  // Whenever the thread id changes, bail out of any in-flight rename —
  // otherwise a user-switched-thread-mid-edit scenario would silently
  // rename the wrong thread when they hit Enter.
  $effect(() => {
    const id = pane.thread?.id;
    if (id == null) return;
    editing = false;
    draftTitle = '';
    renamePending = false;
  });

  // Project lookup for the badge. The projects store is a singleton;
  // undefined is fine (we just don't render the badge).
  let projectBadge = $derived.by(() => {
    const projectId = pane.thread?.projectId;
    if (!projectId) return null;
    const project = getProject(projectId);
    if (!project) return null;
    return {
      id: project.project.id,
      name: project.project.name,
      path: project.project.path,
    };
  });

  function startRename(): void {
    if (!pane.thread || !pane.threadId) return;
    draftTitle = pane.thread.title;
    editing = true;
    void tick().then(() => {
      inputEl?.focus();
      inputEl?.select();
    });
  }

  function cancelRename(): void {
    editing = false;
    draftTitle = '';
  }

  async function commitRename(): Promise<void> {
    if (!pane.thread || !pane.threadId) return;
    const next = draftTitle.trim();
    // Empty submits and no-op submits both bail out quietly. The user
    // already sees their current title — no need to toast a no-op.
    if (next === '' || next === pane.thread.title) {
      cancelRename();
      return;
    }
    const threadId = pane.threadId;
    renamePending = true;
    try {
      await RenameThread(threadId, next);
      // RenameThread returns void; re-read the row so the pane + sidebar
      // pick up the new title without hand-assembling a Thread.
      const updated = (await GetThread(threadId)) as Thread;
      syncThread(updated);
    } catch (err) {
      console.error('Rename thread failed:', err);
      pane.setGeneralError(`Failed to rename thread: ${errString(err)}`);
    } finally {
      renamePending = false;
      editing = false;
      draftTitle = '';
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter') {
      e.preventDefault();
      void commitRename();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelRename();
    }
  }

  async function openProjectInEditor(): Promise<void> {
    if (!projectBadge) return;
    try {
      // projectBadge.path is already absolute; workspacePath is unused.
      await OpenInEditor(projectBadge.path, 0, 0, '');
    } catch (err) {
      addToast('error', errString(err));
    }
  }

  // Terminal / Diff / Design panels need a real thread row — terminal
  // session id, diff backend bindings, design preview URL all key off
  // it. On a placeholder, materialize before toggling so the click
  // actually opens the panel instead of flipping the pressed state of
  // a button whose downstream gate (ThreadTerminalPlacement.threadId,
  // DiffPanelDrawer's backend bindings, /design/{threadId}/main/ URL)
  // would otherwise reject the synthetic draft id.
  async function ensureThenToggle(toggle: () => void): Promise<void> {
    const threadId = pane.threadId ?? (await pane.ensureMaterializedThread());
    if (!threadId) return;
    toggle();
  }
</script>

{#if pane.thread}
  <div
    data-testid="chat-header"
    class="flex items-center gap-2 border-b border-border-subtle bg-transparent px-5 py-2 shrink-0 min-w-0 flex-nowrap"
  >
    {#if attentionDot}
      <span
        aria-label={attentionDot.pill.label}
        title={attentionDot.pill.label}
        class={[
          'shrink-0 h-2.5 w-2.5 rounded-full',
          attentionDot.pill.dotClass,
          attentionDot.pill.pulse ? 'animate-pulse' : '',
          attentionDot.pill.glowClass ?? '',
        ].join(' ')}
        data-testid="pane-attention-dot"
        data-pane-id={pane.paneId}
        data-status={attentionDot.status}
      ></span>
    {/if}
    <!-- Title is the pane drag-handle. Right-click renames; mousedown +
         drag reorders the pane. Left-click is reserved for the drag
         gesture so a fast click doesn't accidentally start an edit. -->
    {#if editing}
      <input
        bind:this={inputEl}
        type="text"
        bind:value={draftTitle}
        onkeydown={handleKeydown}
        onblur={() => void commitRename()}
        disabled={renamePending}
        data-testid="chat-header-title-input"
        aria-label="Rename Thread"
        class="text-sm font-medium text-fg bg-surface-2/60 rounded-[var(--radius-field)] px-1.5 py-0.5 min-w-0 flex-1 outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-60"
      />
    {:else}
      <button
        type="button"
        oncontextmenu={(event) => {
          event.preventDefault();
          startRename();
        }}
        draggable={onPaneDragStart != null}
        ondragstart={(event) => onPaneDragStart?.(event)}
        data-testid="chat-header-title"
        data-focused={isFocusedPane}
        title={`${pane.thread.title} (right-click to rename)`}
        class={[
          'text-sm font-medium truncate min-w-0 text-left bg-transparent border-none px-1.5 py-0.5 rounded-[var(--radius-field)] transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
          onPaneDragStart ? 'cursor-grab active:cursor-grabbing' : 'cursor-default',
          isFocusedPane
            ? 'bg-accent/15 text-fg ring-1 ring-accent/40'
            : 'text-fg hover:bg-surface-2/40',
          titleGlow,
        ].join(' ')}
      >
        {pane.thread.title}
      </button>
    {/if}
    <button
      type="button"
      aria-label="Close Pane"
      title="Close Pane"
      onpointerdown={(event) => event.stopPropagation()}
      onclick={(event) => {
        event.stopPropagation();
        destroyPane(pane.paneId);
      }}
      data-testid="pane-close"
      class="flex h-5 w-5 shrink-0 items-center justify-center rounded-[var(--radius-field)] text-fg-hint opacity-70 transition-colors hover:bg-surface-2/70 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    >
      <Icon icon={X} size={12} strokeWidth={2} />
    </button>

    <!-- Right cluster: wraps, doesn't disappear, at narrow widths.
         Visible on placeholders too — the outer `pane.thread` gate
         already filters out "no thread at all" panes, and the
         subcomponents below self-gate on `pane.threadId` for the
         pieces that genuinely need a persisted row (git
         subscription). -->
    <div class="ml-auto flex items-center gap-1.5 shrink-0 flex-wrap justify-end">
      {#if projectBadge}
        <Button
          variant="secondary"
          size="xs"
          onclick={openProjectInEditor}
          ariaLabel={`Open ${projectBadge.name} in editor`}
          title={`Open ${projectBadge.name} in editor`}
          testId="chat-header-open-editor"
          class="shrink-0"
        >
          {#snippet leading()}
            <Icon icon={FolderClosed} size={12} strokeWidth={2} class="opacity-90" />
          {/snippet}
          {#snippet children()}Open{/snippet}
        </Button>
      {/if}

      <GitActionsControl {pane} />

      <Button
        variant="secondary"
        size="xs"
        pressed={pane.showTerminal}
        ariaLabel="Toggle Terminal"
        title={`Toggle Terminal (${terminalToggleChord})`}
        onclick={() => void ensureThenToggle(() => pane.toggleTerminal())}
        testId="terminal-toggle"
        class="shrink-0 w-6 px-0"
      >
        {#snippet children()}
          <Icon icon={SquareTerminal} size={12} strokeWidth={2} class="opacity-90" />
        {/snippet}
      </Button>

      {#if isDesignThread}
        <Button
          variant="secondary"
          size="xs"
          pressed={pane.showDesignPreviewPanel}
          ariaLabel="Toggle Design Preview"
          title="Toggle Design Preview"
          onclick={() => void ensureThenToggle(() => pane.toggleDesignPreviewPanel())}
          testId="design-preview-toggle"
          class="shrink-0 w-6 px-0"
        >
          {#snippet children()}
            <Icon icon={PanelRightOpen} size={12} strokeWidth={2} class="opacity-90" />
          {/snippet}
        </Button>
      {:else}
        <Button
          variant="secondary"
          size="xs"
          pressed={pane.diffPanel.open}
          ariaLabel="Toggle Diff Panel"
          title={`Toggle Diff Panel (${diffPanelToggleChord})`}
          onclick={() => void ensureThenToggle(() => pane.toggleDiffPanel())}
          testId="diff-panel-toggle"
          class="shrink-0 w-6 px-0"
        >
          {#snippet children()}
            <Icon icon={Diff} size={12} strokeWidth={2} class="opacity-90" />
          {/snippet}
        </Button>
      {/if}
    </div>
  </div>
{/if}
