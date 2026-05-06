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
  import type { Thread } from '../../../types/models';
  import { GetThread, ListDiscussionsForThread, StartDiscussionByID } from '../../../stores/bindings';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../../primitives/MenuSectionHeader.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import Star from 'lucide-svelte/icons/star';

  interface Props {
    pane: ThreadPane;
    /**
     * Called synchronously when a discussion is picked, BEFORE the
     * async StartDiscussion round-trip begins. Lets the parent menu
     * collapse the full stack on selection (otherwise the bubble-
     * based collapse gets isolated by the popover portal-to-body and
     * only the submenu would close, leaving the root menu open until
     * an outside click).
     */
    onSelect?: () => void;
    isFavorite?: (id: string) => boolean;
    onToggleFavorite?: (definition: DiscussionDefinition) => void;
  }

  let { pane, onSelect, isFavorite = () => false, onToggleFavorite }: Props = $props();

  let definitions: DiscussionDefinition[] = $state([]);
  let loading = $state(false);
  let error: string | null = $state(null);
  let loadGeneration = 0;

  let projectPath = $derived(pane.thread?.projectPath ?? pane.thread?.workspacePath ?? '');

  // Load on first mount AND whenever the pane's project path changes so
  // the project-scoped bucket reflects the active thread.
  $effect(() => {
    const threadID = pane.thread?.id ?? '';
    if (threadID === '') {
      definitions = [];
      loading = false;
      error = null;
      return;
    }
    // Track deps explicitly so Svelte re-runs on thread/project changes.
    const _ = projectPath;
    const generation = ++loadGeneration;
    loading = true;
    error = null;
    void (async () => {
      try {
        const scoped = (await ListDiscussionsForThread(threadID)) as DiscussionDefinition[] | null;
        if (generation !== loadGeneration) return;
        // Merge + dedupe by id — the backend may surface the same row in
        // both scopes depending on how the user seeded it.
        const merged: DiscussionDefinition[] = scoped ?? [];
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

  async function startDiscussion(def: DiscussionDefinition): Promise<void> {
    if (!pane.thread) return;
    const threadId = pane.thread.id;
    // Collapse the menu immediately on click. The async StartDiscussion
    // work continues in the background — matches the Codex/Claude
    // model-picker UX where the menu disappears on pick and the
    // toast/state update lands a moment later.
    onSelect?.();
    try {
      await StartDiscussionByID(threadId, def.id);
      // StartDiscussion does NOT emit `thread:updated`, so we refresh the
      // thread manually — matching DiscussionStartFlow. Without this the
      // ChatHeader, ModeCycleButton, and DiscussionView all keep showing
      // the prior mode until the user reloads.
      try {
        const refreshed = (await GetThread(threadId)) as Thread;
        syncThread(refreshed);
      } catch (refreshErr) {
        console.error('Failed to refresh thread after StartDiscussion:', refreshErr);
      }
      addToast('info', `Started discussion "${def.name}"`);
    } catch (err) {
      console.error('StartDiscussion failed:', err);
      addToast('error', `Failed to start discussion: ${errString(err)}`);
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
      {@const favorite = isFavorite(def.id)}
      <MenuItem
        label={def.name}
        onSelect={() => startDiscussion(def)}
        actionLabel={favorite ? `Remove ${def.name} from favorites` : `Add ${def.name} to favorites`}
        actionPressed={favorite}
        actionPosition="start"
        onAction={onToggleFavorite ? () => onToggleFavorite(def) : undefined}
      >
        {#snippet action()}
          <Icon icon={Star} size={13} strokeWidth={1.8} class={favorite ? 'fill-current' : ''} />
        {/snippet}
      </MenuItem>
    {/each}
  {/if}
  {#if globalDefs.length > 0}
    <MenuSectionHeader label="Global" />
    {#each globalDefs as def (def.id)}
      {@const favorite = isFavorite(def.id)}
      <MenuItem
        label={def.name}
        onSelect={() => startDiscussion(def)}
        actionLabel={favorite ? `Remove ${def.name} from favorites` : `Add ${def.name} to favorites`}
        actionPressed={favorite}
        actionPosition="start"
        onAction={onToggleFavorite ? () => onToggleFavorite(def) : undefined}
      >
        {#snippet action()}
          <Icon icon={Star} size={13} strokeWidth={1.8} class={favorite ? 'fill-current' : ''} />
        {/snippet}
      </MenuItem>
    {/each}
  {/if}
{/if}
