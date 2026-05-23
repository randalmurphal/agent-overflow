<script lang="ts">
  // Project trigger in the composer workspace strip. Lets the user
  // re-target a fresh draft thread to a different project without
  // going back to the sidebar.
  //
  // Lock policy: the picker is interactive only while the pane shows a
  // draft — either an unmaterialized placeholder, or a materialized
  // thread with `isDraft=true` (no items persisted yet). Once the user
  // sends, the backend returns `isDraft=false`, and the picker becomes
  // a static label showing the project the thread belongs to. Switching
  // projects on a thread with messages would mean re-targeting messages
  // mid-conversation, which isn't a thing we support.
  //
  // Switch flow: replace the current placeholder with one for the new
  // project. Any materialized draft for the previous project stays in
  // the sidebar (its composer-draft row keeps it visible) — same
  // behavior as clicking the sidebar pencil on multiple projects in
  // succession.

  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import FolderIcon from 'lucide-svelte/icons/folder';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import { getProject, getProjects } from '../../../stores/projects.svelte';
  import type { DraftMode } from '../../../stores/threadCreation.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { userFacingError } from '../../../utils/userFacingError';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let switching = $state(false);

  // Drafts are mode-keyed. The active tab decides which mode a switch
  // creates a draft in — matches the "+ New" pencil behavior so the
  // composer picker and the sidebar pencil produce identical thread
  // shapes for the same selection.
  let draftMode = $derived<DraftMode>(pane.activeTab === 'design' ? 'design' : 'chat');

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
  let projects = $derived(getProjects());

  function handleTrigger(): void {
    if (isLocked) return;
    open = !open;
  }

  function closeMenu(): void {
    open = false;
    triggerEl?.focus();
  }

  function selectProject(projectId: string): void {
    if (isLocked || switching) return;
    if (projectId === activeProjectId) {
      closeMenu();
      return;
    }
    switching = true;
    try {
      const project = getProject(projectId)?.project;
      if (!project) throw new Error('Project not found');
      pane.startDraftPlaceholder(project, draftMode);
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
  <button
    bind:this={triggerEl}
    type="button"
    onclick={handleTrigger}
    disabled={isLocked || switching}
    aria-haspopup={isLocked ? undefined : 'menu'}
    aria-expanded={isLocked ? undefined : open}
    data-testid="project-picker-trigger"
    data-locked={isLocked || undefined}
    class={[
      composerTriggerClasses,
      // The shared trigger style dims disabled buttons; on a locked
      // (post-send) thread we want the project name to read as a normal
      // label, not "this control is broken". Locking still removes the
      // hover affordance + chevron — the opacity override here just
      // preserves the text colour.
      isLocked ? '!opacity-100 !cursor-default hover:!bg-transparent' : '',
    ].join(' ')}
  >
    <Icon icon={FolderIcon} size={12} strokeWidth={2} class="opacity-70" />
    <span class="truncate max-w-[160px] text-fg">{activeProjectName || 'Project'}</span>
    {#if !isLocked}
      <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
    {/if}
  </button>

  {#if !isLocked}
    <Popover
      anchor={triggerEl}
      {open}
      onClose={closeMenu}
      placement="top-start"
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
                description={pwc.project.path}
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
