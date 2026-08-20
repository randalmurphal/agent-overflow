<script lang="ts">
  // Shared pane title: the renameable, draggable, focus-outlined title that
  // sits at the start of a pane header. Extracted from ChatHeader so the
  // terminal pane header (and any future pane surface) gets the exact same
  // behavior — left-click is the drag handle (reorder the pane), right-click
  // renames inline (Enter submits via RenameThread, Escape/blur cancels), and
  // the title gains an accent ring when its pane is the focused one.
  //
  // Wrapper-less on purpose: it emits the input-or-button directly into the
  // parent header's flex so callers keep their own layout. Each consumer
  // renders its own leading element (chat → attention dot, terminal → glyph)
  // as a sibling before this component.

  import { tick } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    registerPaneTitleRename,
    renameThreadTitle,
  } from '../../stores/paneTitleRename';
  import { getFocusedPaneId } from '../../stores/panes.svelte';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  interface Props {
    pane: ThreadPane;
    onPaneDragStart?: (event: DragEvent) => void;
    /** Extra classes for the title button (e.g. an attention glow). */
    glowClass?: string;
    titleTestId?: string;
    inputTestId?: string;
  }

  let {
    pane,
    onPaneDragStart,
    glowClass = '',
    titleTestId = 'pane-title',
    inputTestId = 'pane-title-input',
  }: Props = $props();

  // Deliberately the RAW focused pane id (not getFocusedThreadPaneId): the
  // accent ring marks the pane that literally holds focus. When a companion
  // is focused its ring lives on the companion, so the source thread's
  // title must NOT light up too.
  let isFocusedPane = $derived(getFocusedPaneId() === pane.paneId);

  // Inline-rename state: a local string buffer + editing toggle so the input
  // is controlled without disturbing the pane/thread state until commit.
  let editing = $state(false);
  let draftTitle = $state('');
  let inputEl: HTMLInputElement | undefined = $state(undefined);
  let renamePending = $state(false);

  // When the thread id changes, bail out of any in-flight rename — otherwise a
  // switch-thread-mid-edit would silently rename the wrong thread on Enter.
  // The id goes through a $derived cutoff: events.ts reassigns pane.thread on
  // every SAME-id activity/status patch (replaceThread({ ...pane.thread, … })),
  // and an effect subscribed to the object would close an open rename mid-edit
  // on each one, dropping the user's draft. The derived does not propagate
  // while the id is value-equal, so this runs only on an actual switch.
  const renameThreadId = $derived(pane.thread?.id);
  $effect(() => {
    void renameThreadId;
    editing = false;
    draftTitle = '';
    renamePending = false;
  });

  // Published so the composer's bare `/rename` opens THIS editor rather than
  // growing a second rename surface. Registered per pane, like the composer
  // pickers, so a multi-pane layout routes the command to the right one.
  $effect(() => registerPaneTitleRename(pane.paneId, { start: startRename }));

  function startRename(): void {
    if (!pane.thread || !pane.threadId) return;
    draftTitle = pane.thread.title;
    editing = true;
    void tick().then(() => {
      // preventScroll: DOM focus must never scroll the pane strip (see
      // paneComposerFocus.ts) — reveal belongs to revealPane alone.
      inputEl?.focus({ preventScroll: true });
      inputEl?.select();
    });
  }

  function cancelRename(): void {
    editing = false;
    draftTitle = '';
  }

  async function commitRename(): Promise<void> {
    if (!pane.thread || !pane.threadId) return;
    // Empty and no-op submits both bail quietly inside renameThreadTitle —
    // the user already sees the current title, so there's nothing to report.
    const threadId = pane.threadId;
    const currentTitle = pane.thread.title;
    renamePending = true;
    try {
      const result = await renameThreadTitle(threadId, draftTitle, currentTitle);
      if (!result.ok && result.error) pane.setGeneralError(result.error);
    } finally {
      renamePending = false;
      editing = false;
      draftTitle = '';
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    // Enter confirms the IME candidate while composing a CJK title; committing
    // the rename here would save the pre-composition text and exit edit mode.
    if (e.key === 'Enter' && isImeComposingEvent(e)) return;
    if (e.key === 'Enter') {
      e.preventDefault();
      void commitRename();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelRename();
    }
  }
</script>

{#if pane.thread}
  {#if editing}
    <input
      bind:this={inputEl}
      type="text"
      bind:value={draftTitle}
      onkeydown={handleKeydown}
      onblur={() => void commitRename()}
      disabled={renamePending}
      data-testid={inputTestId}
      aria-label="Rename Thread"
      class="text-sm font-medium text-fg bg-surface-2/60 rounded-[var(--radius-field)] px-1.5 py-0.5 min-w-0 flex-1 outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-60"
    />
  {:else}
    <!-- Title is the pane drag-handle. Right-click renames; mousedown + drag
         reorders the pane. Left-click is reserved for the drag gesture so a
         fast click doesn't accidentally start an edit. -->
    <button
      type="button"
      oncontextmenu={(event) => {
        event.preventDefault();
        startRename();
      }}
      draggable={onPaneDragStart != null}
      ondragstart={(event) => onPaneDragStart?.(event)}
      data-testid={titleTestId}
      data-focused={isFocusedPane}
      title={`${pane.thread.title} (right-click to rename)`}
      class={[
        'text-sm font-medium truncate min-w-0 text-left bg-transparent border-none px-1.5 py-0.5 rounded-[var(--radius-field)] transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
        onPaneDragStart ? 'cursor-grab active:cursor-grabbing' : 'cursor-default',
        isFocusedPane
          ? 'bg-accent/15 text-fg ring-1 ring-accent/40'
          : 'text-fg hover:bg-surface-2/40',
        glowClass,
      ].join(' ')}
    >
      {pane.thread.title}
    </button>
  {/if}
{/if}
