<script lang="ts">
  import type { DiscussionDefinition, DiscussionScope } from '../../types/discussion';
  import { createEmptyDiscussionDefinition } from '../../types/discussion';
  import { ListDiscussions } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import DiscussionListPanel from '../discussion/DiscussionListPanel.svelte';
  import DiscussionEditor from '../discussion/DiscussionEditor.svelte';
  import SettingsCallout from './SettingsCallout.svelte';

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
      addToast('error', `Failed to load discussions: ${errString(err)}`);
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

<div class="flex flex-col gap-6">
  {#if loadError}
    <SettingsCallout tone="error">{loadError}</SettingsCallout>
  {/if}

  <div class="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,280px)_minmax(0,1fr)]">
    <aside
      class="lg:max-h-[70vh] rounded-[var(--radius-control)] border border-border-subtle bg-surface-1/40 p-4"
    >
      <DiscussionListPanel
        {discussions}
        {selectedKey}
        onSelect={handleSelect}
        onNew={handleNew}
        {loading}
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
</div>
