<script lang="ts">
  // Discussions submenu inside the ModelProviderMenu. Replaces the old
  // DiscussionDefinitionPicker whose `<select>` chrome was bolted onto
  // the thread-create form. Now that discussions live inside the
  // composer's model menu, each definition renders as a MenuItem and
  // selecting one promotes the current thread into discussion mode via
  // StartDiscussion.
  //
  // Scoping: project-scoped discussions (matching the pane's project
  // path) are listed before global ones, mirroring how the old picker
  // grouped them.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { DiscussionDefinition } from '../../../types/discussion';
  import { ListDiscussions, StartDiscussion } from '../../../stores/bindings';
  import { addToast } from '../../../stores/toast.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../../primitives/MenuSectionHeader.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let definitions: DiscussionDefinition[] = $state([]);
  let loading = $state(false);
  let error: string | null = $state(null);
  let loadGeneration = 0;

  let projectPath = $derived(pane.thread?.projectPath ?? pane.thread?.workspacePath ?? '');

  // Load on first mount AND whenever the pane's project path changes so
  // the project-scoped bucket reflects the active thread.
  $effect(() => {
    // Track the dep explicitly so Svelte re-runs on project changes.
    const _ = projectPath;
    const generation = ++loadGeneration;
    loading = true;
    error = null;
    void (async () => {
      try {
        const [project, global] = await Promise.all([
          projectPath
            ? (ListDiscussions('project') as Promise<DiscussionDefinition[] | null>)
            : Promise.resolve<DiscussionDefinition[]>([]),
          ListDiscussions('global') as Promise<DiscussionDefinition[] | null>,
        ]);
        if (generation !== loadGeneration) return;
        // Merge + dedupe by id — the backend may surface the same row in
        // both scopes depending on how the user seeded it.
        const merged: DiscussionDefinition[] = [...(project ?? []), ...(global ?? [])];
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

  let projectDefs = $derived(
    definitions.filter((d) => d.scope === 'project' && (d.projectId ?? '') === projectPath),
  );
  let globalDefs = $derived(definitions.filter((d) => d.scope !== 'project'));

  async function startDiscussion(name: string): Promise<void> {
    if (!pane.thread) return;
    try {
      await StartDiscussion(pane.thread.id, name);
      addToast('info', `Started discussion "${name}"`);
    } catch (err) {
      console.error('StartDiscussion failed:', err);
      addToast('error', `Failed to start discussion: ${err}`);
    }
  }
</script>

{#if loading}
  <div
    class="px-3 py-2 text-xs text-text-secondary/60"
    data-testid="discussions-submenu-loading"
    role="presentation"
  >
    Loading discussions…
  </div>
{:else if error}
  <div
    class="px-3 py-2 text-xs text-error"
    data-testid="discussions-submenu-error"
    role="presentation"
  >
    {error}
  </div>
{:else if definitions.length === 0}
  <div
    class="px-3 py-2 text-xs text-text-secondary/60"
    data-testid="discussions-submenu-empty"
    role="presentation"
  >
    No discussions defined
  </div>
{:else}
  {#if projectDefs.length > 0}
    <MenuSectionHeader label="Project" />
    {#each projectDefs as def (def.id)}
      <MenuItem label={def.name} onSelect={() => startDiscussion(def.name)} />
    {/each}
  {/if}
  {#if globalDefs.length > 0}
    <MenuSectionHeader label="Global" />
    {#each globalDefs as def (def.id)}
      <MenuItem label={def.name} onSelect={() => startDiscussion(def.name)} />
    {/each}
  {/if}
{/if}
