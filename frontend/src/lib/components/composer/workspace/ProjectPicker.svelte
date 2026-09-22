<script lang="ts">
  // Project menu behind the chat header's project segment (the header
  // draws the trigger and passes it as `anchor`; see
  // chat/ChatHeaderWorkspace.svelte). Lets the user re-target a fresh
  // draft thread to a different project without going back to the sidebar.
  //
  // Lock policy: the picker is interactive only while the pane shows a
  // draft — either an unmaterialized placeholder, or a materialized
  // thread with `isDraft=true` (no items persisted yet). Once the user
  // sends, the backend returns `isDraft=false`, and the picker becomes
  // a static label showing the project the thread belongs to. Switching
  // projects on a thread with messages would mean re-targeting messages
  // mid-conversation, which isn't a thing we support.
  //
  // Project changes carry the unsent composer and its selected settings.
  // The emptied source follows the ordinary empty-draft cleanup policy.

  import type { ThreadPane } from '../../../stores/thread.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';
  import type { PopoverPlacement } from '../../../utils/popoverGeometry';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import { getProject, projectEntries, projectSpansBackends } from '../../../stores/projects.svelte';
  import {
    switchDraftProject,
  } from '../../../stores/threadCreation.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { setPaneBackend } from '../../../stores/selectedBackend.svelte';
  import {
    attachedBackendEntry,
    backendDisplayName,
    hasMultipleBackends,
  } from '../../../stores/attachedBackends.svelte';
  import { projectBackend } from '../../../transport/entityIndex';
  import { HOME_BACKEND } from '../../../transport/backendKey';
  import { userFacingError } from '../../../utils/userFacingError';

  interface Props {
    pane: ThreadPane;
    /** The host's trigger: the menu anchors to it and focus returns to it. */
    anchor: HTMLElement | undefined;
    placement?: PopoverPlacement;
  }

  let { pane, anchor, placement = 'bottom-start' }: Props = $props();

  let open = $state(false);
  let switching = $state(false);

  // Picker is interactive while the pane is sitting on a draft (either
  // unmaterialized placeholder or materialized but no-items-yet).
  // Project re-targeting on a populated thread is not supported.
  let isLocked = $derived(
    !pane.thread || (!pane.hasDraftPlaceholder && pane.thread.isDraft !== true),
  );

  let activeProjectId = $derived(pane.thread?.projectId ?? null);
  let activeProjectName = $derived.by(() => {
    if (!activeProjectId) return '';
    return getProject(activeProjectId)?.project.name ?? '';
  });
  // One row per repository: a repo on two machines is one choice here and
  // a target choice in the machine picker beside it.
  let projects = $derived(projectEntries());
  // With several machines attached, two projects may share a name and
  // even a path; the row says which machine before it says where.
  let multiMachine = $derived(hasMultipleBackends());
  function projectDescription(projectId: string, path: string): string {
    if (!multiMachine || projectSpansBackends(projectId)) return path;
    const entry = attachedBackendEntry(projectBackend(projectId) ?? HOME_BACKEND);
    return entry ? `${backendDisplayName(entry)} · ${path}` : path;
  }

  export function label(): string { return activeProjectName; }
  export function locked(): boolean { return isLocked; }
  export function openPicker(): void {
    if (!open && !isLocked && !switching) open = true;
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl: anchor });
  }

  async function selectProject(projectId: string): Promise<void> {
    if (isLocked || switching) return;
    if (projectId === activeProjectId) {
      closeMenu();
      return;
    }
    switching = true;
    try {
      const project = getProject(projectId)?.project;
      if (!project) throw new Error('Project not found');
      if (await switchDraftProject(pane, project)) {
        setPaneBackend(pane.paneId, projectBackend(projectId) ?? HOME_BACKEND);
      }
    } catch (err) {
      console.error('Failed to switch draft project:', err);
      addToast('error', userFacingError(err));
    } finally {
      switching = false;
      closeMenu();
    }
  }
</script>

{#if activeProjectId}
  {#if !isLocked}
    <Popover
      {anchor}
      {open}
      onClose={closeMenu}
      {placement}
      role="none"
    >
      <Menu ariaLabel="Project" onClose={closeMenu}>
        {#if projects.length === 0}
          <div
            class="px-3 py-1.5 text-xs text-text-secondary/60"
            role="presentation"
            data-testid="project-picker-empty"
          >
            No projects
          </div>
        {:else}
          <div class="max-h-56 overflow-y-auto">
            {#each projects as pwc (pwc.project.id)}
              <MenuItem
                label={pwc.project.name}
                description={projectDescription(pwc.project.id, pwc.project.path)}
                checked={pwc.project.id === activeProjectId}
                onSelect={() => void selectProject(pwc.project.id)}
              />
            {/each}
          </div>
        {/if}
      </Menu>
    </Popover>
  {/if}
{/if}
