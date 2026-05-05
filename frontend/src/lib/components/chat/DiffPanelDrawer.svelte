<script lang="ts">
  import Pencil from 'lucide-svelte/icons/pencil';
  import X from 'lucide-svelte/icons/x';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import {
    GetMessageCheckpointDiff,
    GetSessionAgentDiff,
    GetWorkspaceCurrentDiff,
    ListThreadCheckpoints,
    SendDiffReviewComments,
  } from '../../stores/bindings';
  import {
    createDiffReviewComment,
    deleteDiffReviewComment,
    getDiffReviewComments,
    refreshDiffReviewComments,
    setActiveDiffReviewSource,
    updateDiffReviewComment,
  } from '../../stores/diffReviewComments.svelte';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import type { Checkpoint } from '../../types/checkpoint';
  import type { DiffReviewComment, DiffReviewCommentInput, DiffReviewScope } from '../../types/models';
  import { parsePatchFiles, patchFileRowId } from '../../utils/patchFiles';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import DiffPanelHeaderBar from './diff-panel/DiffPanelHeaderBar.svelte';
  import DiffPanelChipStrip from './diff-panel/DiffPanelChipStrip.svelte';
  import DiffPanelFileCard from './diff-panel/DiffPanelFileCard.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let diffText = $state('');
  let loading = $state(false);
  let expanded = $state<Set<string>>(new Set());
  let editingCommentId = $state<string | null>(null);
  let editingBody = $state('');
  let sendingComments = $state(false);
  let checkpointRequestID = 0;
  let diffRequestID = 0;
  let commentsRequestID = 0;

  const checkpoints = $derived(pane.diffPanel.checkpoints);
  const visibleCheckpoints = $derived(
    checkpoints.filter((c) => c.turnIndex === 0 || (c.files?.length ?? 0) > 0),
  );
  const selectedUserItemId = $derived(pane.diffPanel.selectedCheckpointUserItemId);
  const selectedCheckpoint = $derived(
    selectedUserItemId === null
      ? null
      : checkpoints.find((c) => c.userItemId === selectedUserItemId) ?? null,
  );
  const error = $derived(pane.diffPanel.error);
  const viewMode = $derived(pane.diffPanel.viewMode);
  const tabMode = $derived(pane.diffPanel.tabMode);
  const threadId = $derived(pane.thread?.id ?? null);
  const files = $derived(parsePatchFiles(diffText));
  const fileRows = $derived(files.map((file, index) => ({ file, rowId: patchFileRowId(file, index) })));
  const totals = $derived.by(() => files.reduce(
    (acc, file) => ({
      files: acc.files + 1,
      additions: acc.additions + file.additions,
      deletions: acc.deletions + file.deletions,
    }),
    { files: 0, additions: 0, deletions: 0 },
  ));
  const wordWrap = $derived(getSettings().diffWordWrap);
  const showChipStrip = $derived(tabMode === 'messages');
  const diffSourceKey = $derived(diffText ? diffSourceKeyFor(diffText) : '');
  const reviewScope = $derived<DiffReviewScope | null>(
    tabMode === 'workspace'
      ? 'workspace'
      : tabMode === 'messages' && selectedUserItemId === null
        ? 'session'
        : null,
  );
  const commentable = $derived(Boolean(threadId && reviewScope && diffSourceKey));
  const isTurnActive = $derived(getActiveTurn(threadId) !== null);
  const reviewComments = $derived(
    threadId && reviewScope && diffSourceKey ? getDiffReviewComments(threadId, reviewScope, diffSourceKey) : [],
  );
  const draftReviewComments = $derived(reviewComments.filter((comment) => comment.status === 'draft'));

  async function refreshCheckpoints(): Promise<void> {
    const requestID = ++checkpointRequestID;
    if (!threadId) {
      pane.diffPanel.setCheckpoints([]);
      return;
    }
    try {
      const next = ((await ListThreadCheckpoints(threadId)) ?? []) as Checkpoint[];
      if (requestID !== checkpointRequestID) return;
      const sorted = [...next].sort((a, b) => a.turnIndex - b.turnIndex);
      pane.diffPanel.setCheckpoints(sorted);
      if (selectedUserItemId !== null && !sorted.some((c) => c.userItemId === selectedUserItemId)) {
        pane.diffPanel.selectCheckpointUserItem(null);
      }
    } catch (err) {
      if (requestID !== checkpointRequestID) return;
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
    }
  }

  async function loadDiff(): Promise<void> {
    const requestID = ++diffRequestID;
    if (!threadId) {
      if (requestID === diffRequestID) diffText = '';
      return;
    }
    if (tabMode !== 'workspace' && checkpoints.length === 0) {
      if (requestID === diffRequestID) diffText = '';
      return;
    }
    loading = true;
    pane.diffPanel.setError(null);
    try {
      let nextDiff = '';
      if (tabMode === 'messages') {
        nextDiff = selectedCheckpoint
          ? (((await GetMessageCheckpointDiff(threadId, selectedCheckpoint.userItemId)) ?? '') as string)
          : (((await GetSessionAgentDiff(threadId)) ?? '') as string);
      } else {
        nextDiff = ((await GetWorkspaceCurrentDiff(threadId)) ?? '') as string;
      }
      if (requestID !== diffRequestID) return;
      diffText = nextDiff;
      expanded = new Set();
    } catch (err) {
      if (requestID !== diffRequestID) return;
      diffText = '';
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
    } finally {
      if (requestID === diffRequestID) loading = false;
    }
  }

  async function loadComments(): Promise<void> {
    const requestID = ++commentsRequestID;
    if (!threadId || !reviewScope || !diffSourceKey) return;
    try {
      await refreshDiffReviewComments(threadId, reviewScope, diffSourceKey);
    } catch (err) {
      if (requestID !== commentsRequestID) return;
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
    }
  }

  function selectCheckpoint(userItemId: string | null): void {
    pane.diffPanel.selectCheckpointUserItem(userItemId);
  }

  function jumpToSelectedCheckpoint(): void {
    if (!selectedCheckpoint) return;
    pane.requestScrollToItem(selectedCheckpoint.userItemId, {
      behavior: 'animated',
      flash: true,
    });
  }

  function toggleFile(rowId: string): void {
    const next = new Set(expanded);
    if (next.has(rowId)) next.delete(rowId);
    else next.add(rowId);
    expanded = next;
  }

  function setAllFiles(open: boolean): void {
    if (open && totals.files > 40 && !window.confirm(`Expand all ${totals.files} changed files? Large diffs can take a moment to render.`)) {
      return;
    }
    expanded = open ? new Set(fileRows.map((row) => row.rowId)) : new Set();
  }

  async function createComment(input: DiffReviewCommentInput): Promise<void> {
    if (!threadId || !reviewScope || !diffSourceKey) return;
    try {
      await createDiffReviewComment(threadId, { ...input, scope: reviewScope, sourceKey: diffSourceKey });
      setActiveDiffReviewSource(threadId, reviewScope, diffSourceKey);
    } catch (err) {
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
      throw err;
    }
  }

  function startEditComment(comment: DiffReviewComment): void {
    editingCommentId = comment.id;
    editingBody = comment.body;
  }

  async function saveCommentEdit(comment: DiffReviewComment): Promise<void> {
    if (!threadId || !reviewScope || !diffSourceKey) return;
    const body = editingBody.trim();
    if (!body) return;
    try {
      await updateDiffReviewComment(threadId, reviewScope, diffSourceKey, comment.id, { body });
      editingCommentId = null;
      editingBody = '';
    } catch (err) {
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
    }
  }

  async function deleteComment(comment: DiffReviewComment): Promise<void> {
    if (!threadId || !reviewScope || !diffSourceKey) return;
    try {
      await deleteDiffReviewComment(threadId, reviewScope, diffSourceKey, comment.id);
      if (editingCommentId === comment.id) {
        editingCommentId = null;
        editingBody = '';
      }
    } catch (err) {
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
    }
  }

  async function sendCommentsOnly(): Promise<void> {
    if (!threadId || !reviewScope || !diffSourceKey || draftReviewComments.length === 0 || sendingComments || isTurnActive) return;
    sendingComments = true;
    try {
      await SendDiffReviewComments(threadId, reviewScope, diffSourceKey, draftReviewComments.map((comment) => comment.id));
      await refreshDiffReviewComments(threadId, reviewScope, diffSourceKey);
      editingCommentId = null;
      editingBody = '';
    } catch (err) {
      pane.diffPanel.setError(err instanceof Error ? err.message : String(err));
    } finally {
      sendingComments = false;
    }
  }

  function commentLocation(comment: DiffReviewComment): string {
    if (comment.side === 'file') return comment.filePath;
    const line = comment.side === 'old' ? comment.oldLine : (comment.newLine || comment.oldLine);
    return line ? `${comment.filePath}:${line}` : comment.filePath;
  }

  $effect(() => {
    threadId;
    void refreshCheckpoints();
  });

  $effect(() => {
    threadId;
    selectedUserItemId;
    tabMode;
    void loadDiff();
  });

  $effect(() => {
    const tid = threadId;
    const scope = reviewScope;
    const sourceKey = diffSourceKey;
    if (tid && scope && sourceKey) {
      void loadComments();
    } else if (tid) {
      setActiveDiffReviewSource(tid, null);
    }
  });

  function diffSourceKeyFor(text: string): string {
    let hash = 0x811c9dc5;
    for (let index = 0; index < text.length; index += 1) {
      hash ^= text.charCodeAt(index);
      hash = Math.imul(hash, 0x01000193) >>> 0;
    }
    return `fnv1a:${hash.toString(16).padStart(8, '0')}:${text.length}`;
  }
</script>

<section
  aria-label="Diff Panel"
  data-testid="diff-panel-drawer"
  class="flex min-h-0 flex-1 flex-col bg-surface-0"
>
  <header class="border-b border-border bg-surface-1/70">
    <DiffPanelHeaderBar
      {totals}
      {viewMode}
      setViewMode={(mode) => pane.diffPanel.setViewMode(mode)}
      {wordWrap}
      setWordWrap={(next) => updateSetting('diffWordWrap', next)}
      {tabMode}
      setTabMode={(mode) => pane.diffPanel.setTabMode(mode)}
      onClose={() => pane.setDiffPanelOpen(false)}
    />
    {#if showChipStrip}
      <DiffPanelChipStrip
        {visibleCheckpoints}
        selectedUserItemId={selectedUserItemId}
        onSelectCheckpoint={selectCheckpoint}
        onJumpToCheckpoint={jumpToSelectedCheckpoint}
      />
    {/if}
  </header>

  <div class="flex min-h-0 flex-1 flex-col">
    {#if error}
      <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-[12px] text-error" data-testid="diff-panel-error">{error}</div>
    {/if}
    <div class="flex items-center gap-2 border-b border-border-subtle px-3 py-2">
      <button class="rounded border border-border-subtle px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2" onclick={() => setAllFiles(true)}>Expand all</button>
      <button class="rounded border border-border-subtle px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2" onclick={() => setAllFiles(false)}>Collapse all</button>
    </div>

    <div class="min-h-0 flex-1 overflow-auto px-3 py-3">
      {#if loading}
        <div class="py-8 text-center text-[13px] text-fg-muted" role="status">Loading diff...</div>
      {:else if checkpoints.length === 0 && tabMode !== 'workspace'}
        <div class="py-8 text-center text-[13px] text-fg-muted">No checkpoints yet.</div>
      {:else if files.length === 0}
        <div class="py-8 text-center text-[13px] text-fg-muted">No changes in this range.</div>
      {:else}
        <div class="space-y-2" data-testid="diff-viewer">
          {#each fileRows as { file, rowId } (rowId)}
            <DiffPanelFileCard
              {file}
              open={expanded.has(rowId)}
              workspacePath={paneWorkspacePath(pane)}
              {viewMode}
              {wordWrap}
              {commentable}
              {reviewScope}
              sourceKey={diffSourceKey}
              comments={reviewComments}
              onToggle={() => toggleFile(rowId)}
              onCreateComment={createComment}
            />
          {/each}
        </div>
      {/if}
    </div>

    {#if commentable && draftReviewComments.length > 0}
      <!--
        The drawer's comments strip is the live draft surface. After
        `sendCommentsOnly` flips the drafts to `sent`, they fall out of
        `draftReviewComments` and the strip collapses on its own — so
        "send" visibly clears the panel without a manual refresh.
      -->
      <section class="border-t border-border bg-surface-1/85 px-3 py-2" aria-label="Diff comments">
        <div class="mb-2 flex items-center justify-between gap-3">
          <div class="text-[11px] font-medium uppercase tracking-[0.08em] text-fg-muted">
            Comments
          </div>
          <button
            type="button"
            class="rounded border border-accent/45 px-2 py-1 text-[11px] font-medium text-accent hover:bg-accent/10 disabled:opacity-45"
            disabled={sendingComments || isTurnActive}
            title={isTurnActive ? 'Send from the chat box while the agent is working' : 'Send comments'}
            onclick={sendCommentsOnly}
          >
            Send comments
          </button>
        </div>
        <div class="max-h-44 space-y-2 overflow-auto pr-1">
          {#each draftReviewComments as comment (comment.id)}
            <article class="rounded border border-border-subtle bg-surface-0/70 px-2 py-2">
              <div class="mb-1 flex items-center gap-1">
                <span class="min-w-0 flex-1 truncate font-mono text-[11px] text-fg-muted">{commentLocation(comment)}</span>
                {#if editingCommentId !== comment.id}
                  <IconButton label="Edit comment" size="sm" onClick={() => startEditComment(comment)}>
                    {#snippet children()}<Icon icon={Pencil} size={12} />{/snippet}
                  </IconButton>
                  <IconButton label="Delete comment" size="sm" onClick={() => void deleteComment(comment)}>
                    {#snippet children()}<Icon icon={X} size={12} />{/snippet}
                  </IconButton>
                {/if}
              </div>
              {#if editingCommentId === comment.id}
                <textarea
                  bind:value={editingBody}
                  rows="2"
                  class="w-full resize-none rounded border border-border-subtle bg-surface-1 px-2 py-1.5 text-[12px] text-fg focus:border-accent/60 focus:outline-none"
                ></textarea>
                <div class="mt-2 flex justify-end gap-2">
                  <button type="button" class="rounded px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2" onclick={() => { editingCommentId = null; editingBody = ''; }}>Cancel</button>
                  <button type="button" class="rounded bg-accent px-2 py-1 text-[11px] font-medium text-accent-contrast disabled:opacity-45" disabled={!editingBody.trim()} onclick={() => saveCommentEdit(comment)}>Save</button>
                </div>
              {:else}
                <p class="whitespace-pre-wrap text-[12px] leading-relaxed text-fg">{comment.body}</p>
              {/if}
            </article>
          {/each}
        </div>
      </section>
    {/if}
  </div>
</section>
