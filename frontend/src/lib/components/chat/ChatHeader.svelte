<script lang="ts">
  // Chat header: the shared pane title (PaneTitleHandle — renameable,
  // draggable, focus-outlined) plus an attention dot on the left, and the
  // action cluster on the right (Open-in-editor, git actions, terminal
  // toggle, diff toggle). Thread mode (chat vs design) is read from
  // `pane.thread.mode` and surfaced by the composer's ThreadModePicker;
  // nothing here needs to render mode chrome.

  import FolderClosed from 'lucide-svelte/icons/folder-closed';
  import SquareTerminal from 'lucide-svelte/icons/square-terminal';
  import Diff from 'lucide-svelte/icons/diff';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { OpenInEditor } from '../../stores/bindings';
  import { errString } from '../../utils/errors';
  import { getProject } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { formatChord, keybindingForCommand } from '../../stores/keybindings.svelte';
  import { resolvePaneAttentionDot } from '../panes/paneAttention';
  import { runTerminalToggle } from '../terminal/terminalToggle';
  import { openTerminalThread } from '../../stores/threadCreation.svelte';
  import PaneTitleHandle from '../panes/PaneTitleHandle.svelte';
  import PaneCloseButton from '../panes/PaneCloseButton.svelte';
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
  let isDesignThread = $derived(pane.thread?.mode === 'design');

  let terminalToggleChord = $derived(
    formatChord(keybindingForCommand('terminal.toggle') ?? 'mod+`'),
  );
  let diffPanelToggleChord = $derived(
    formatChord(keybindingForCommand('diff.panel.toggle') ?? 'mod+shift+g'),
  );

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
    <PaneTitleHandle
      {pane}
      {onPaneDragStart}
      glowClass={titleGlow}
      titleTestId="chat-header-title"
      inputTestId="chat-header-title-input"
    />
    <PaneCloseButton paneId={pane.paneId} testId="pane-close" />

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

      <!-- Plain click shares runTerminalToggle with the mod+` chord
           (terminal.toggle) so opening focuses the terminal and closing hands
           focus back to the composer. Ctrl/Cmd-click instead opens a FRESH
           terminal in a new pane (rooted at this thread's workspace) — the same
           gesture the sidebar uses for "open in new pane", and the mod+shift+~
           chord's pointer twin. -->
      <Button
        variant="secondary"
        size="xs"
        pressed={pane.showTerminal}
        ariaLabel="Toggle Terminal"
        title={`Toggle Terminal (${terminalToggleChord})`}
        onclick={(e) => {
          if (e.metaKey || e.ctrlKey) {
            void openTerminalThread({
              projectId: pane.thread?.projectId,
              cwd: pane.thread?.workspacePath,
            });
          } else {
            void ensureThenToggle(() => runTerminalToggle(pane));
          }
        }}
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
