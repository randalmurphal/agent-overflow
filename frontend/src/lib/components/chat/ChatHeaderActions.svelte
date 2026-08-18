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
  // This component is the pane's attacher on the shared, workspace-keyed
  // git-status store (stores/gitStatusStore.svelte.ts). GitActionsControl and
  // the two badges read the resulting status through `pane.gitStatus`, which
  // is a view onto the same entry — so two panes on one worktree share one
  // subscription and can never show different Commit/Push state.
  import SquareTerminal from '@lucide/svelte/icons/square-terminal';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';
  import ChevronsDownUp from '@lucide/svelte/icons/chevrons-down-up';
  import ChevronsUpDown from '@lucide/svelte/icons/chevrons-up-down';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getProject } from '../../stores/projects.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { chordHintForCommand, chordHintSuffix } from '../../stores/keybindings.svelte';
  import { runTerminalToggle } from '../terminal/terminalToggle';
  import { openTerminalThread } from '../../stores/threadCreation.svelte';
  import { openReviewCompanion } from '../../stores/reviewPane.svelte';
  import { attachGitStatus } from '../../stores/gitStatusStore.svelte';
  import { workspaceKeyForThread } from '../../utils/workspaceKey';
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

  let terminalToggleSuffix = $derived(chordHintSuffix('terminal.toggle'));
  let reviewToggleChord = $derived(chordHintForCommand('diff.panel.toggle'));

  // Attach deps as $derived primitives so the effect re-runs only when the
  // values actually change — pane.replaceThread() fires for unrelated metadata
  // (token usage, mode) on every streamed token, and value-equality on these
  // derives suppresses the downstream re-subscribe.
  //
  // Deliberately NOT the thread id: the entity is the workspace, and two
  // threads in one worktree are one entity. Tracking the id made a
  // same-workspace thread switch release and re-attach, which bounced the
  // backend refcount through zero — the whole fs watcher torn down and
  // rebuilt, and every badge in the pane blanked, for a switch that changed
  // nothing about the checkout. `hasThread` is a boolean for the same
  // reason: it flips only when the pane gains or loses a real thread row.
  let hasThread = $derived(Boolean(pane.threadId));
  let gitStatusKey = $derived(workspaceKeyForThread(pane.thread ?? null));

  // The pane's reference on its workspace's git-status entry. The store owns
  // the subscription, the retry backoff, the `git:status` routing, the
  // transport-reconnect re-acquire, and the observed-branch persist; this
  // effect owns only the reference's lifetime. Consumers read through
  // `pane.gitStatus` and never attach a second time.
  $effect(() => {
    if (!hasThread || gitStatusKey === null) return;
    const attachment = attachGitStatus(gitStatusKey, {
      // Read at SOURCE time, not attach time. The reference outlives any one
      // thread the pane shows, so a re-source (reconnect, retry) has to run
      // against a thread that still exists. The store's source prologue runs
      // untracked, so this read cannot pull the id back into the effect.
      get threadId() {
        return pane.threadId ?? '';
      },
    });
    return () => attachment.release();
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

  // One control, not two. Its meaning comes from the thread's current default
  // rather than a survey of the rendered runs: only the loaded window holds
  // any, so a survey would answer "all of which runs" differently as older
  // history pages in — and a button that relabels itself while you scroll is
  // worse than one that always says what it will do.
  let runsCollapsed = $derived(pane.activityRuns.bulkCollapsed);
  let runsToggleLabel = $derived(
    runsCollapsed ? 'Expand all activity runs' : 'Collapse all activity runs',
  );

  function toggleAllRuns(): void {
    // The viewport-bottom hold is the registry's own (see
    // `ThreadActivityRuns.setAllCollapsed`) — do not wrap.
    pane.activityRuns.setAllCollapsed(!runsCollapsed);
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

  {#if !isDesignThread}
    <!-- Collapse/expand every activity run in this thread. Also the only
         VISIBLE affordance for the run collapse mechanic: a single run is
         toggled by its rail, which consumes no width and so shows nothing
         until you find it. The setting under Settings → General → Activity
         Runs is the durable default; this is the per-thread override. -->
    <Button
      variant="secondary"
      size="xs"
      pressed={runsCollapsed}
      ariaLabel={runsToggleLabel}
      title={runsToggleLabel}
      onclick={toggleAllRuns}
      testId="activity-runs-toggle"
      class="shrink-0 w-6 px-0"
    >
      {#snippet children()}
        <Icon
          icon={runsCollapsed ? ChevronsUpDown : ChevronsDownUp}
          size={12}
          strokeWidth={2}
          class="opacity-90"
        />
      {/snippet}
    </Button>
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
    title={`Toggle Terminal${terminalToggleSuffix}`}
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
