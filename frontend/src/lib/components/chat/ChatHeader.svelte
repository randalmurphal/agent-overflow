<script lang="ts">
  // Chat header: thread title on the left, project badge + meters +
  // diff toggle + git actions on the right.
  //
  // The interaction-mode badge is gone (ModeCycleButton on the composer
  // toolbar owns it now), and so is the ModelPicker / RuntimeModePicker
  // / BranchToolbar cluster (composer toolbar + below-composer bar).
  // What remains is the chrome that's either thread-scoped metadata
  // (title, project, workspace stats) or actions users want near the
  // thread title (diffs, commit/push/PR). Plan review lives in the
  // composer toolbar next to the mode/access controls.
  //
  // The inline-rename flow below mirrors the behavior the old
  // ChatView.svelte header used: click the title to switch to an input,
  // Enter to submit (RenameThread), Escape / blur to cancel.

  import { tick } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import { RenameThread, GetThread } from '../../stores/bindings';
  import { replaceThread } from '../../stores/threads.svelte';
  import { errString } from '../../utils/errors';
  import { getProject } from '../../stores/projects.svelte';
  import { expandProject } from '../../stores/sidebar.svelte';
  import ContextWindowMeter from './ContextWindowMeter.svelte';
  import GitActionsControl from '../git/GitActionsControl.svelte';
  import Button from '../primitives/Button.svelte';
  import EditorLink from '../common/EditorLink.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

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
    if (!pane.thread) return;
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
    if (!pane.thread) return;
    const next = draftTitle.trim();
    // Empty submits and no-op submits both bail out quietly. The user
    // already sees their current title — no need to toast a no-op.
    if (next === '' || next === pane.thread.title) {
      cancelRename();
      return;
    }
    const threadId = pane.thread.id;
    renamePending = true;
    try {
      await RenameThread(threadId, next);
      // RenameThread returns void; re-read the row so the pane + sidebar
      // pick up the new title without hand-assembling a Thread.
      const updated = (await GetThread(threadId)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
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

  function focusProjectInSidebar(): void {
    if (!projectBadge) return;
    expandProject(projectBadge.id);
  }
</script>

{#if pane.thread}
  <div
    data-testid="chat-header"
    class="flex items-center gap-2 border-b border-border-subtle bg-transparent px-5 py-2 shrink-0 min-w-0 flex-nowrap"
  >
    <!-- Title — double-click to rename, or single-click the inline button. -->
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
        onclick={startRename}
        data-testid="chat-header-title"
        title={pane.thread.title}
        class="text-sm font-medium text-fg truncate min-w-0 text-left bg-transparent border-none p-0 cursor-text hover:text-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 rounded transition-colors"
      >
        {pane.thread.title}
      </button>
    {/if}

    <!-- Right cluster: wraps, doesn't disappear, at narrow widths. -->
    <div class="ml-auto flex items-center gap-1.5 shrink-0 flex-wrap justify-end">
      {#if projectBadge}
        <button
          type="button"
          onclick={focusProjectInSidebar}
          data-testid="chat-header-project"
          title={`Project: ${projectBadge.name}`}
          class="rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[11px] text-fg-muted hover:text-fg hover:border-border transition-colors cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 max-w-[160px] truncate"
        >
          {projectBadge.name}
        </button>
        <!-- Open the project root in the user's editor. Stays adjacent
             to the badge it acts on; the icon-only button keeps the
             header dense at narrow widths. -->
        <EditorLink
          path={projectBadge.path}
          asIcon
          label={`Open ${projectBadge.name} in editor`}
          class="shrink-0"
        />
      {/if}

      {#if pane.contextWindow}
        <div class="shrink-0" data-testid="chat-header-context-meter">
          <ContextWindowMeter data={pane.contextWindow} />
        </div>
      {/if}

      <Button
        variant="ghost"
        size="xs"
        pressed={pane.diffPanel.open}
        ariaLabel="Toggle Diff Panel"
        title="Toggle Diff Panel (⇧⌘G)"
        onclick={() => pane.toggleDiffPanel()}
        testId="diff-panel-toggle"
        class="shrink-0"
      >
        {#snippet children()}Diffs{/snippet}
      </Button>

      <GitActionsControl {pane} />
    </div>
  </div>
{/if}
