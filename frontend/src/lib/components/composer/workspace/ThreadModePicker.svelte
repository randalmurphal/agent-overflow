<script lang="ts">
  // Thread mode trigger in the composer workspace strip. Mirrors
  // ProjectPicker's shape: interactive while the pane shows a draft (so
  // the user can flip the next-to-be-created thread between chat and
  // design), read-only label once the thread is committed (mode is
  // immutable post-creation by backend invariant in
  // internal/threadmode/threadmode.go's PostCreationModes).
  //
  // Switch flow: replace the current placeholder with one of the new
  // mode, same primitive ProjectPicker uses for project flips. Composer
  // draft text is keyed by pane.paneId (not thread id) so typed text
  // survives the placeholder swap.

  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  import Palette from '@lucide/svelte/icons/palette';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import { getProject } from '../../../stores/projects.svelte';
  import {
    flipPaneDraftPlaceholder,
    type DraftMode,
  } from '../../../stores/threadCreation.svelte';
  import { addToast } from '../../../stores/toast.svelte';
  import { userFacingError } from '../../../utils/userFacingError';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let switching = $state(false);

  // Mode is a property of the loaded thread. For chat/plan/discussion the
  // picker shows "Chat" — plan is a runtime-mode variant of chat, and
  // discussion threads render through DiscussionView so the composer
  // (and this picker) is not visible for them in practice.
  let currentMode = $derived<DraftMode>(
    pane.thread?.mode === 'design' ? 'design' : 'chat',
  );

  // Same lock predicate as ProjectPicker: interactive while sitting on a
  // draft (unmaterialized placeholder or materialized-but-no-items-yet),
  // locked otherwise. Backend invariant disallows mode flips on a
  // committed thread.
  let isLocked = $derived(
    !pane.thread || (!pane.hasDraftPlaceholder && pane.thread.isDraft !== true),
  );

  function handleTrigger(): void {
    if (isLocked) return;
    open = !open;
  }

  function closeMenu(): void {
    open = false;
    triggerEl?.focus();
  }

  async function selectMode(next: DraftMode): Promise<void> {
    if (isLocked || switching) return;
    if (next === currentMode) {
      closeMenu();
      return;
    }
    const projectId = pane.thread?.projectId;
    if (!projectId) {
      closeMenu();
      return;
    }
    switching = true;
    try {
      const project = getProject(projectId)?.project;
      if (!project) throw new Error('Project not found');
      // flipPaneDraftPlaceholder fetches fresh seed defaults (model,
      // effort, branch) before swapping the placeholder so the new
      // mode's toolbar isn't blank. Calling pane.startDraftPlaceholder
      // directly would drop the seeded values.
      await flipPaneDraftPlaceholder(pane, project, next);
    } catch (err) {
      console.error('Failed to switch draft mode:', err);
      addToast('error', userFacingError(err));
    } finally {
      switching = false;
      closeMenu();
    }
  }

  let currentIcon = $derived(currentMode === 'design' ? Palette : MessagesSquare);
  let currentLabel = $derived(currentMode === 'design' ? 'Design' : 'Chat');
</script>

{#if pane.thread}
  <button
    bind:this={triggerEl}
    type="button"
    onclick={handleTrigger}
    disabled={isLocked || switching}
    aria-haspopup={isLocked ? undefined : 'menu'}
    aria-expanded={isLocked ? undefined : open}
    data-testid="thread-mode-picker-trigger"
    data-locked={isLocked || undefined}
    class={[
      composerTriggerClasses,
      isLocked ? '!opacity-100 !cursor-default hover:!bg-transparent' : '',
    ].join(' ')}
  >
    <Icon icon={currentIcon} size={12} strokeWidth={2} class="opacity-70" />
    <span class="text-fg">{currentLabel}</span>
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
      <Menu ariaLabel="Thread Mode" onClose={closeMenu} minWidthClass="min-w-[140px]">
        <MenuItem
          label="Chat"
          checked={currentMode === 'chat'}
          onSelect={() => void selectMode('chat')}
        >
          {#snippet icon()}
            <Icon icon={MessagesSquare} size={12} strokeWidth={2} class="opacity-90" />
          {/snippet}
        </MenuItem>
        <MenuItem
          label="Design"
          checked={currentMode === 'design'}
          onSelect={() => void selectMode('design')}
        >
          {#snippet icon()}
            <Icon icon={Palette} size={12} strokeWidth={2} class="opacity-90" />
          {/snippet}
        </MenuItem>
      </Menu>
    </Popover>
  {/if}
{/if}
