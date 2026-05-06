<script lang="ts">
  import Modal from '../primitives/Modal.svelte';
  import Button from '../primitives/Button.svelte';
  import type { DiscussionDefinition } from '../../types/discussion';
  import type { Thread } from '../../types/models';
  import { ListDiscussions, StartDiscussion, GetThread } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { syncThread } from '../../stores/panes.svelte';
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
        syncThread(refreshed);
      } catch (refreshErr) {
        console.error('Failed to refresh thread after StartDiscussion:', refreshErr);
      }
      addToast('success', `Started discussion "${selected.name}".`);
      onClose();
    } catch (err) {
      console.error('Failed to start discussion:', err);
      startError = String(err);
      addToast('error', `Failed to start discussion: ${errString(err)}`);
    } finally {
      starting = false;
    }
  }
</script>

<Modal
  {open}
  title={thread ? (thread.title || 'Untitled thread') : 'Pick a discussion'}
  onClose={onClose}
  width="xl"
  padding="comfortable"
>
  {#snippet children()}
    <div class="flex flex-col gap-3">
      <p class="text-[11px] font-semibold uppercase tracking-[0.18em] text-fg-subtle">Start discussion</p>
      <p class="text-[12px] text-fg-muted leading-relaxed">
        Participants spawn as child threads and deliberate in a shared channel.
      </p>

      <div class="flex-1 min-h-0 overflow-y-auto pr-1">
        {#if loadError}
          <div role="alert" class="rounded-[var(--radius-control)] border border-error/40 bg-error/10 px-3 py-2 text-[12px] text-error">
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
        <div role="alert" aria-live="polite" class="rounded-[var(--radius-control)] border border-error/40 bg-error/10 px-3 py-2 text-[12px] text-error">
          {startError}
        </div>
      {/if}
    </div>
  {/snippet}
  {#snippet footer()}
    <Button variant="secondary" size="sm" onclick={onClose}>
      {#snippet children()}Cancel{/snippet}
    </Button>
    <Button
      variant="primary"
      size="sm"
      autofocus
      onclick={handleStart}
      disabled={!thread || !selected || loading}
      loading={starting}
    >
      {#snippet children()}{starting ? 'Starting…' : 'Start'}{/snippet}
    </Button>
  {/snippet}
</Modal>
