<script lang="ts">
  // Right-aligned action cluster for the chat header. Split out of
  // ChatHeader.svelte (which kept it under the file-size budget and gave the
  // shared git-status subscription a single home).
  //
  // Cluster order, chat threads:  [ PR#123 ] [ +X −Y ] [ Open ] [ git ] [ term ]
  // Cluster order, design threads: [ Open ] [ git ] [ term ] [ Preview ]
  //
  // The PR badge and workspace +/- render only on normal chat threads; design
  // threads keep their existing Design Preview button instead. GitActionsControl
  // (commit/push/ship) and the terminal toggle are unchanged on both.
  //
  // This component owns the pane's single gitwatch subscription via the
  // $effect below — GitActionsControl and the two badges all read the resulting
  // status from `pane.gitStatus`, so there is exactly one subscription per pane.
  import SquareTerminal from 'lucide-svelte/icons/square-terminal';
  import PanelRightOpen from 'lucide-svelte/icons/panel-right-open';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { formatChord, keybindingForCommand } from '../../stores/keybindings.svelte';
  import { getTransportStatus } from '../../stores/transportStatus.svelte';
  import { runTerminalToggle } from '../terminal/terminalToggle';
  import { openTerminalThread } from '../../stores/threadCreation.svelte';
  import { openReviewCompanion } from '../../stores/reviewPane.svelte';
  import GitActionsControl from '../git/GitActionsControl.svelte';
  import PrBadge from '../git/PrBadge.svelte';
  import WorkspaceDiffBadge from '../git/WorkspaceDiffBadge.svelte';
  import OpenInEditorControl from './OpenInEditorControl.svelte';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ProviderIcon from '../shared/ProviderIcon.svelte';
  import { isCompanionOpen, toggleCompanion } from '../../stores/companionPanes.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let isDesignThread = $derived(pane.thread?.mode === 'design');
  // Take-control is a claude-tui-only affordance: it opens a paired terminal
  // pane mirroring the live TUI session. Absent for other providers (an
  // unsupported control should not be shown rather than shown-and-disabled).
  let isClaudeTui = $derived(pane.thread?.provider === 'claude-tui');
  let takeControlOpen = $derived(isCompanionOpen(pane.paneId, 'take-control'));

  let terminalToggleChord = $derived(
    formatChord(keybindingForCommand('terminal.toggle') ?? 'mod+`'),
  );
  let reviewToggleChord = $derived(
    formatChord(keybindingForCommand('diff.panel.toggle') ?? 'mod+shift+g'),
  );

  // Subscription deps. Derived primitives so the attach $effect re-runs only
  // when the actual value changes — pane.replaceThread() for unrelated metadata
  // (token usage, mode) won't thrash the git-status pipe (value-equality on the
  // derived suppresses the downstream re-run). gitCwd resolves to the worktree
  // dir for worktree threads, so the subscription is worktree-correct.
  let threadId = $derived(pane.threadId);
  let gitCwd = $derived(
    pane.thread?.worktreePath ?? pane.thread?.workspacePath ?? null,
  );
  let transportConnected = $derived(getTransportStatus().status === 'connected');

  // Single gitwatch subscription for this pane's workspace. Lives here (not in
  // GitActionsControl) so the commit/push control and the header badges share
  // one stream. The slot owns the subscribe / retry-backoff / git:status
  // listener / observed-branch persist; this effect just wires the reactive
  // deps and returns the slot's cleanup.
  $effect(() => {
    return pane.gitStatus.attach({
      threadId,
      cwd: gitCwd,
      connected: transportConnected,
      getThread: () => pane.thread ?? null,
      getLiveThreadId: () => pane.threadId,
      reportError: (message) => pane.setGeneralError(message),
    });
  });

  // Project lookup for the badge. The projects store is a singleton;
  // undefined is fine (we just don't render the badge). projectId is derived as
  // a primitive first so the badge lookup only re-runs when the id actually
  // changes — pane.replaceThread() mints a fresh thread object on every
  // token-usage push during streaming, but the id string is value-equal, so the
  // $derived short-circuits (same guard the subscription deps above use).
  let projectId = $derived(pane.thread?.projectId ?? null);
  let projectBadge = $derived.by(() => {
    if (!projectId) return null;
    const project = getProject(projectId);
    if (!project) return null;
    return {
      id: project.project.id,
      name: project.project.name,
      path: project.project.path,
    };
  });

  // Diff / Design panels need a real thread row — their backend bindings and
  // preview URL key off it. Drawer terminals are allowed on placeholders; they
  // use the synthetic placeholder id and are cleaned up or migrated by
  // ThreadPane.
  async function ensureThenToggle(toggle: () => void): Promise<void> {
    if (!pane.threadId) {
      addToast('info', 'Start the thread before opening this panel.');
      return;
    }
    toggle();
  }

  function toggleWorkspaceReview(): void {
    if (!pane.threadId) return;
    if (pane.showReviewPane) {
      pane.setShowReviewPane(false);
      return;
    }
    void openReviewCompanion(pane.paneId, pane.threadId, { scope: 'workspace' });
  }
</script>

<!-- Right cluster: wraps, doesn't disappear, at narrow widths. Visible on
     placeholders too — the caller's `pane.thread` gate already filters out
     "no thread at all" panes, and the subcomponents self-gate on
     `pane.threadId` for the pieces that genuinely need a persisted row. -->
<div class="ml-auto flex items-center gap-1.5 shrink-0 flex-wrap justify-end">
  {#if !isDesignThread}
    <PrBadge status={pane.gitStatus.status} />
    <WorkspaceDiffBadge
      status={pane.gitStatus.status}
      pressed={pane.showReviewPane}
      chord={reviewToggleChord}
      onActivate={() => void ensureThenToggle(toggleWorkspaceReview)}
    />
  {/if}

  {#if projectBadge}
    <OpenInEditorControl path={projectBadge.path} name={projectBadge.name} />
  {/if}

  <GitActionsControl {pane} />

  {#if isClaudeTui}
    <!-- Take-control: opens a paired terminal pane to the right that mirrors
         this claude-tui session's live PTY. The terminal-green Claude glyph
         ties the button to the TUI provider. Toggles the paired pane. -->
    <Button
      variant="secondary"
      size="xs"
      pressed={takeControlOpen}
      ariaLabel="Toggle take-control terminal"
      title="Take control — open the live Claude TUI terminal"
      onclick={() => void ensureThenToggle(() => toggleCompanion(pane.paneId, 'take-control'))}
      testId="take-control-toggle"
      class="shrink-0 w-6 px-0"
    >
      {#snippet children()}
        <ProviderIcon provider="claude-tui" size={13} />
      {/snippet}
    </Button>
  {/if}

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
        runTerminalToggle(pane);
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
  {/if}
</div>
