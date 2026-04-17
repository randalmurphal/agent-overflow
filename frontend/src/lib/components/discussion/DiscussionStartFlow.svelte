<script lang="ts">
  import { fade, scale } from 'svelte/transition';
  import type { DiscussionDefinition } from '../../types/discussion';
  import type { Thread } from '../../types/models';
  import { ListDiscussions, StartDiscussion, GetThread } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { replaceThread as replaceThreadList } from '../../stores/threads.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import DiscussionPicker from './DiscussionPicker.svelte';

  let {
    open,
    thread,
    pane,
    onClose,
  }: {
    open: boolean;
    thread: Thread | null;
    pane: ThreadPane;
    onClose: () => void;
  } = $props();

  let loading = $state(false);
  let starting = $state(false);
  let discussions = $state<DiscussionDefinition[]>([]);
  let selectedName = $state<string | null>(null);
  let loadError = $state<string | null>(null);
  let startError = $state<string | null>(null);
  let dialogEl: HTMLDivElement | undefined = $state(undefined);
  let loadGeneration = 0;

  /**
   * Ordered discussions: project-scoped for this thread's project first, then
   * global. Matches the backend `resolveDiscussionDefinition` precedence so the
   * top entry is what the backend would have picked anyway.
   */
  let orderedDiscussions = $derived.by<DiscussionDefinition[]>(() => {
    const projectPath = thread?.projectPath ?? '';
    const project = discussions.filter(
      (d) => d.scope === 'project' && (d.projectId ?? '') === projectPath,
    );
    const global = discussions.filter((d) => d.scope !== 'project');
    return [...project, ...global];
  });

  let selected = $derived(
    orderedDiscussions.find((d) => d.name === selectedName) ?? orderedDiscussions[0] ?? null,
  );

  $effect(() => {
    if (!open) return;
    startError = null;
    loadError = null;
    selectedName = null;
    const generation = ++loadGeneration;
    loading = true;

    void (async () => {
      try {
        const projectScope = thread?.projectPath ?? '';
        const [projectList, globalList] = await Promise.all([
          projectScope ? (ListDiscussions('project') as Promise<DiscussionDefinition[] | null>) : Promise.resolve([]),
          ListDiscussions('global') as Promise<DiscussionDefinition[] | null>,
        ]);
        if (generation !== loadGeneration) return;
        const merged = [...(projectList ?? []), ...(globalList ?? [])];
        discussions = merged;
      } catch (err) {
        if (generation !== loadGeneration) return;
        console.error('Failed to load discussions:', err);
        loadError = String(err);
        discussions = [];
      } finally {
        if (generation === loadGeneration) {
          loading = false;
        }
      }
    })();
  });

  $effect(() => {
    if (open && dialogEl) {
      const focusable = dialogEl.querySelector<HTMLElement>('[role="option"], button');
      focusable?.focus();
    }
  });

  function handleSelect(def: DiscussionDefinition): void {
    selectedName = def.name;
  }

  async function handleStart(): Promise<void> {
    if (!thread || !selected || starting) return;
    startError = null;
    starting = true;
    try {
      await StartDiscussion(thread.id, selected.name);
      try {
        const refreshed = (await GetThread(thread.id)) as Thread;
        pane.replaceThread(refreshed);
        replaceThreadList(refreshed);
      } catch (refreshErr) {
        console.error('Failed to refresh thread after StartDiscussion:', refreshErr);
      }
      addToast('success', `Started discussion "${selected.name}".`);
      onClose();
    } catch (err) {
      console.error('Failed to start discussion:', err);
      startError = String(err);
      addToast('error', `Failed to start discussion: ${err}`);
    } finally {
      starting = false;
    }
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
    }
  }

  function handleBackdropClick(e: MouseEvent): void {
    if (e.target === e.currentTarget) {
      onClose();
    }
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    transition:fade={{ duration: 150 }}
    class="fixed inset-0 z-[60] flex items-center justify-center bg-overlay backdrop-blur-sm"
    onclick={handleBackdropClick}
    onkeydown={handleKeydown}
  >
    <div
      bind:this={dialogEl}
      transition:scale={{ start: 0.95, duration: 150 }}
      role="dialog"
      aria-modal="true"
      aria-labelledby="start-discussion-title"
      class="bg-surface-1 border border-border rounded-2xl shadow-xl w-full max-w-xl mx-4 p-5 max-h-[85vh] flex flex-col"
    >
      <div class="flex items-start justify-between gap-3">
        <div>
          <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Start discussion</p>
          <h2 id="start-discussion-title" class="mt-1 text-base font-semibold text-text-primary">
            {thread ? thread.title || 'Untitled thread' : 'Pick a discussion'}
          </h2>
          <p class="mt-1 text-xs text-text-secondary">
            Participants spawn as child threads and deliberate in a shared channel.
          </p>
        </div>
        <button
          type="button"
          onclick={onClose}
          class="text-text-secondary hover:text-text-primary p-1 rounded cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
          aria-label="Close"
        >
          <svg class="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
            <path d="M18 6L6 18M6 6l12 12" />
          </svg>
        </button>
      </div>

      <div class="mt-4 flex-1 min-h-0 overflow-y-auto pr-1">
        {#if loadError}
          <div role="alert" class="rounded-xl border border-error/40 bg-error/12 px-3 py-2 text-xs text-error">
            Failed to load discussions: {loadError}
          </div>
        {:else}
          <DiscussionPicker
            discussions={orderedDiscussions}
            selectedName={selected?.name ?? null}
            onSelect={handleSelect}
            {loading}
            emptyLabel="No discussions defined yet. Add one from Settings → Discussions."
          />
        {/if}
      </div>

      {#if startError}
        <div role="alert" aria-live="polite" class="mt-3 rounded-xl border border-error/40 bg-error/12 px-3 py-2 text-xs text-error">
          {startError}
        </div>
      {/if}

      <div class="mt-4 flex justify-end gap-2 shrink-0">
        <button
          type="button"
          onclick={onClose}
          class="rounded-xl border border-border px-4 py-2 text-xs text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          Cancel
        </button>
        <button
          type="button"
          onclick={handleStart}
          disabled={!thread || !selected || starting || loading}
          class="rounded-xl bg-accent px-4 py-2 text-xs font-semibold text-surface-0 hover:opacity-90 disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          {starting ? 'Starting...' : 'Start'}
        </button>
      </div>
    </div>
  </div>
{/if}
