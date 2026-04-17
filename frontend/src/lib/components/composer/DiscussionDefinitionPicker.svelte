<script lang="ts">
  // Dropdown for picking a discussion definition when creating a new thread.
  // Optional — users can create regular threads by leaving it on "None".
  // When a discussion is selected, the new-thread flow calls StartDiscussion
  // after CreateThread so the parent thread is promoted into discussion mode
  // and participant child threads are spawned.
  //
  // Matches forge's UnifiedThreadPicker behavior (discussions are a first-
  // class thread-creation choice) but as a separate field alongside the
  // provider picker — our data model keeps provider and discussion
  // orthogonal, since the parent thread always needs its own provider/model.

  import type { DiscussionDefinition } from '../../types/discussion';
  import { ListDiscussions } from '../../stores/bindings';

  interface Props {
    /** null = no discussion (regular thread). */
    selectedName: string | null;
    /** Workspace / project path, used to fetch the right project-scoped set. */
    projectPath: string;
    onSelect: (name: string | null) => void;
    disabled?: boolean;
  }

  let { selectedName, projectPath, onSelect, disabled = false }: Props = $props();

  let definitions: DiscussionDefinition[] = $state([]);
  let loading = $state(false);
  let error: string | null = $state(null);
  let loadGeneration = 0;

  // Refresh whenever the form opens or the project path changes.
  $effect(() => {
    const generation = ++loadGeneration;
    loading = true;
    error = null;
    void (async () => {
      try {
        const [project, global] = await Promise.all([
          projectPath
            ? (ListDiscussions('project') as Promise<DiscussionDefinition[] | null>)
            : Promise.resolve([]),
          ListDiscussions('global') as Promise<DiscussionDefinition[] | null>,
        ]);
        if (generation !== loadGeneration) return;
        // Dedupe by id. The backend's project/global lists can overlap
        // when a project-scoped definition shares an id with a global
        // one; Svelte keyed each blocks reject duplicate keys, and the
        // user only wants one entry per definition either way.
        const merged = [...(project ?? []), ...(global ?? [])];
        const byId = new Map<string, DiscussionDefinition>();
        for (const d of merged) {
          if (!byId.has(d.id)) byId.set(d.id, d);
        }
        definitions = Array.from(byId.values());
      } catch (err) {
        if (generation !== loadGeneration) return;
        error = err instanceof Error ? err.message : String(err);
        definitions = [];
      } finally {
        if (generation === loadGeneration) loading = false;
      }
    })();
  });

  // Split by scope for grouped rendering. Project-scoped definitions that
  // match the current project come first; everything else falls into the
  // global bucket.
  let projectDefs = $derived(
    definitions.filter((d) => d.scope === 'project' && (d.projectId ?? '') === projectPath),
  );
  let globalDefs = $derived(definitions.filter((d) => d.scope !== 'project'));

  function handleChange(e: Event): void {
    const value = (e.target as HTMLSelectElement).value;
    onSelect(value === '' ? null : value);
  }
</script>

<div class="space-y-1.5" data-testid="discussion-definition-picker">
  <div class="flex items-baseline justify-between">
    <span class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">
      Discussion
    </span>
    <span class="text-[10px] text-text-secondary/60">optional</span>
  </div>

  <select
    value={selectedName ?? ''}
    onchange={handleChange}
    disabled={disabled || loading}
    data-testid="discussion-definition-select"
    aria-label="Discussion definition"
    class="w-full text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 disabled:opacity-60 disabled:cursor-not-allowed"
  >
    <option value="">
      {loading
        ? 'Loading discussions…'
        : definitions.length === 0
          ? 'No discussions defined'
          : 'None — regular thread'}
    </option>
    {#if projectDefs.length > 0}
      <optgroup label="Project">
        {#each projectDefs as def (def.id)}
          <option value={def.name}>{def.name}</option>
        {/each}
      </optgroup>
    {/if}
    {#if globalDefs.length > 0}
      <optgroup label="Global">
        {#each globalDefs as def (def.id)}
          <option value={def.name}>{def.name}</option>
        {/each}
      </optgroup>
    {/if}
  </select>

  {#if error}
    <p class="text-[10px] text-error" data-testid="discussion-definition-error">{error}</p>
  {/if}
</div>
