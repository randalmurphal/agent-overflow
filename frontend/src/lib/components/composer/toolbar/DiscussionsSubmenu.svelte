<script lang="ts">
  // Discussions submenu inside the ModelProviderMenu. Replaces the old
  // DiscussionDefinitionPicker whose `<select>` chrome was bolted onto
  // the thread-create form. Now that discussions live inside the
  // composer's model menu, each definition renders as a MenuItem and
  // selecting one promotes the current thread into discussion mode via
  // StartDiscussion.
  //
  // Pure presentation: ModelProviderMenu owns the ListDiscussionsForThread
  // fetch (via ensureDiscussions, called on every menu open) so it can
  // decide whether to render the "Discussions" entry at all — the entry
  // is hidden when zero definitions exist rather than opening onto an
  // empty submenu. This component only renders the `definitions`/`error`
  // props it's handed; it does not fetch.
  //
  // Scoping: project-scoped discussions (matching the pane's project
  // path) are listed before global ones, mirroring how the old picker
  // grouped them.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import type { DiscussionDefinition } from '../../../types/discussion';
  import type { Thread } from '../../../types/models';
  import { GetThread, StartDiscussionByID } from '../../../stores/bindings';
  import { syncThread } from '../../../stores/panes.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { errString } from '../../../utils/errors';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import MenuSectionHeader from '../../primitives/MenuSectionHeader.svelte';
  import Icon from '../../primitives/Icon.svelte';
  import Star from '@lucide/svelte/icons/star';

  interface Props {
    pane: ThreadPane;
    definitions: DiscussionDefinition[];
    error: string | null;
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

  let { pane, definitions, error, onSelect, isFavorite = () => false, onToggleFavorite }: Props = $props();

  let projectPath = $derived(pane.thread?.projectPath ?? pane.thread?.workspacePath ?? '');

  let projectDefs = $derived(
    definitions.filter((d) => d.scope === 'project' && (d.projectId ?? '') === projectPath),
  );
  let globalDefs = $derived(definitions.filter((d) => d.scope !== 'project'));

  async function startDiscussion(def: DiscussionDefinition): Promise<void> {
    if (!pane.thread) return;
    // Collapse the menu immediately on click. The async StartDiscussion
    // work continues in the background — matches the Codex/Claude
    // model-picker UX where the menu disappears on pick and the
    // toast/state update lands a moment later.
    onSelect?.();
    try {
      const threadId = pane.threadId;
      if (!threadId) {
        addToast('info', 'Start the thread before adding a discussion.');
        return;
      }
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

{#if error}
  <div
    class="px-3 py-2 text-xs text-error"
    data-testid="discussions-submenu-error"
    role="presentation"
  >
    {error}
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
