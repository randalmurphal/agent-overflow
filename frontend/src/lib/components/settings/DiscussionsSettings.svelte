<script lang="ts">
  import type { DiscussionDefinition, DiscussionScope } from '../../types/discussion';
  import { createEmptyDiscussionDefinition } from '../../types/discussion';
  import { ListDiscussions } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import DiscussionListPanel from '../discussion/DiscussionListPanel.svelte';
  import DiscussionEditor from '../discussion/DiscussionEditor.svelte';

  type Mode =
    | { kind: 'new' }
    | { kind: 'edit'; original: DiscussionDefinition };

  let discussions = $state<DiscussionDefinition[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let mode = $state<Mode>({ kind: 'new' });
  let draftForNew = $state<DiscussionDefinition>(createEmptyDiscussionDefinition());

  async function loadAll(selectAfter: { scope: DiscussionScope; name: string } | null = null): Promise<void> {
    loading = true;
    loadError = null;
    try {
      const [global, project] = await Promise.all([
        ListDiscussions('global') as Promise<DiscussionDefinition[] | null>,
        ListDiscussions('project') as Promise<DiscussionDefinition[] | null>,
      ]);
      discussions = [...(global ?? []), ...(project ?? [])];
      if (selectAfter) {
        const match = discussions.find((d) => d.scope === selectAfter.scope && d.name === selectAfter.name);
        if (match) {
          mode = { kind: 'edit', original: match };
          return;
        }
      }
      // If we're in edit mode but the original disappeared (deleted elsewhere), fall back to new.
      if (mode.kind === 'edit') {
        const original = mode.original;
        const stillExists = discussions.some(
          (d) => d.scope === original.scope && d.name === original.name,
        );
        if (!stillExists) {
          mode = { kind: 'new' };
          draftForNew = createEmptyDiscussionDefinition();
        }
      }
    } catch (err) {
      console.error('Failed to load discussions:', err);
      loadError = String(err);
      addToast('error', `Failed to load discussions: ${err}`);
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void loadAll();
  });

  function handleSelect(def: DiscussionDefinition): void {
    mode = { kind: 'edit', original: def };
  }

  function handleNew(): void {
    draftForNew = createEmptyDiscussionDefinition();
    mode = { kind: 'new' };
  }

  async function handleSaved(saved: DiscussionDefinition): Promise<void> {
    await loadAll({ scope: saved.scope, name: saved.name });
  }

  async function handleDeleted(): Promise<void> {
    mode = { kind: 'new' };
    draftForNew = createEmptyDiscussionDefinition();
    await loadAll();
  }

  let selectedKey = $derived(
    mode.kind === 'edit' ? `${mode.original.scope}::${mode.original.name}` : null,
  );
</script>

<section class="rounded-2xl border border-border/70 bg-surface-1/75 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div>
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Discussions</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">Manage discussion definitions</h3>
      <p class="mt-1 max-w-2xl text-sm text-text-secondary">
        Configure multi-participant deliberations. Global definitions are available everywhere;
        project-scoped definitions take precedence for threads inside their project path.
      </p>
    </div>
  </div>
</section>

{#if loadError}
  <div role="alert" class="mt-4 rounded-xl border border-error/40 bg-error/12 px-3 py-2 text-xs text-error">
    {loadError}
  </div>
{/if}

<div class="mt-5 grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,280px)_minmax(0,1fr)]">
  <aside class="lg:max-h-[70vh] rounded-2xl border border-border/60 bg-surface-1/55 p-4 backdrop-blur-sm">
    <DiscussionListPanel
      discussions={discussions}
      selectedKey={selectedKey}
      onSelect={handleSelect}
      onNew={handleNew}
      loading={loading}
    />
  </aside>

  <div class="min-w-0">
    {#if mode.kind === 'new'}
      <DiscussionEditor
        initial={draftForNew}
        isNew={true}
        onSaved={handleSaved}
      />
    {:else}
      <DiscussionEditor
        initial={mode.original}
        isNew={false}
        onSaved={handleSaved}
        onDeleted={handleDeleted}
      />
    {/if}
  </div>
</div>
