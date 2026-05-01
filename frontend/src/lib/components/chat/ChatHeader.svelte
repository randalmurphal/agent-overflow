<script lang="ts">
  // Chat header: thread title + project chip on the left; action
  // cluster on the right (Open-in-editor, git actions,
  // terminal toggle, diff toggle). Layout mirrors t3-code so the chip
  // travels with the title rather than the actions.
  //
  // The interaction-mode badge is gone (ModeCycleButton on the composer
  // toolbar owns it now), and so is the ModelPicker / RuntimeModePicker
  // / BranchToolbar cluster (composer toolbar + below-composer bar).
  // What remains is the chrome that's either thread-scoped metadata
  // (title, project, workspace stats) or actions users want near the
  // thread title (open-in-editor, commit/push/PR, terminal, diffs).
  // Plan review lives in the composer toolbar next to the mode/access
  // controls.
  //
  // The inline-rename flow below mirrors the behavior the old
  // ChatView.svelte header used: click the title to switch to an input,
  // Enter to submit (RenameThread), Escape / blur to cancel.

  import { tick } from 'svelte';
  import FolderClosed from 'lucide-svelte/icons/folder-closed';
  import SquareTerminal from 'lucide-svelte/icons/square-terminal';
  import Diff from 'lucide-svelte/icons/diff';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import {
    RenameThread,
    GetThread,
    OpenInEditor,
  } from '../../stores/bindings';
  import { replaceThread } from '../../stores/threads.svelte';
  import { errString } from '../../utils/errors';
  import { getProject } from '../../stores/projects.svelte';
  import { expandProject } from '../../stores/sidebar.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import GitActionsControl from '../git/GitActionsControl.svelte';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';

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

  async function openProjectInEditor(): Promise<void> {
    if (!projectBadge) return;
    try {
      // projectBadge.path is already absolute; workspacePath is unused.
      await OpenInEditor(projectBadge.path, 0, 0, '');
    } catch (err) {
      addToast('error', errString(err));
    }
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

    <!-- Project chip sits inline with the title (matches t3-code's
         layout: thread name + project pill on the left, action cluster
         on the right). Click focuses the project in the sidebar. -->
    {#if projectBadge}
      <button
        type="button"
        onclick={focusProjectInSidebar}
        data-testid="chat-header-project"
        title={`Project: ${projectBadge.name}`}
        class="rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[11px] text-fg-muted hover:text-fg hover:border-border transition-colors cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 max-w-[160px] truncate shrink-0"
      >
        {projectBadge.name}
      </button>
    {/if}

    <!-- Right cluster: wraps, doesn't disappear, at narrow widths. -->
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
        title="Toggle Terminal (⌘J)"
        onclick={() => pane.toggleTerminal()}
        testId="terminal-toggle"
        class="shrink-0 w-6 px-0"
      >
        {#snippet children()}
          <Icon icon={SquareTerminal} size={12} strokeWidth={2} class="opacity-90" />
        {/snippet}
      </Button>

      <Button
        variant="secondary"
        size="xs"
        pressed={pane.diffPanel.open}
        ariaLabel="Toggle Diff Panel"
        title="Toggle Diff Panel (⇧⌘G)"
        onclick={() => pane.toggleDiffPanel()}
        testId="diff-panel-toggle"
        class="shrink-0 w-6 px-0"
      >
        {#snippet children()}
          <Icon icon={Diff} size={12} strokeWidth={2} class="opacity-90" />
        {/snippet}
      </Button>
    </div>
  </div>
{/if}
