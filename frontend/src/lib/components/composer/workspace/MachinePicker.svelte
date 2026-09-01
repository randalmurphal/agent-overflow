<script lang="ts">
  // Machine trigger in the composer workspace strip: WHICH attached
  // backend the draft in this pane is for. Mounted only when more than
  // one backend is attached (spec §10, 2026-09-01 ruling), so a
  // single-backend app never sees it.
  //
  // The label names the machine that owns the pane's project. Picking
  // another machine flips the draft to the SAME repository there when the
  // entry spans it (a target choice, wave 7d), else to that machine's
  // first project. An unreachable machine stays in the list, dimmed, and
  // cannot be picked — never a silent failover elsewhere.
  //
  // Lock policy mirrors ProjectPicker: interactive while the pane shows a
  // draft; a static label once the thread has messages, because a thread
  // does not move between machines.

  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import MonitorIcon from '@lucide/svelte/icons/monitor';
  import Icon from '../../primitives/Icon.svelte';
  import { composerTriggerClasses } from '../triggerClasses';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import Popover from '../../primitives/Popover.svelte';
  import { restorePickerFocus } from '../../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../../utils/popoverOwnership';
  import Menu from '../../primitives/Menu.svelte';
  import MenuItem from '../../primitives/MenuItem.svelte';
  import { getProjects, projectSiblingOn } from '../../../stores/projects.svelte';
  import { flipPaneDraftPlaceholder } from '../../../stores/threadCreation.svelte';
  import { setPaneBackend, setSelectedBackend } from '../../../stores/selectedBackend.svelte';
  import {
    attachedBackendEntry,
    backendDisplayName,
    backendReachable,
    getAttachedBackends,
    threadMachine,
  } from '../../../stores/attachedBackends.svelte';
  import { projectBackend } from '../../../transport/entityIndex';
  import { HOME_BACKEND, type BackendKey } from '../../../transport/backendKey';
  import { addToast } from '../../../stores/toast.svelte';
  import { userFacingError } from '../../../utils/userFacingError';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let switching = $state(false);

  let isLocked = $derived(
    !pane.thread || (!pane.hasDraftPlaceholder && pane.thread.isDraft !== true),
  );

  let backends = $derived(getAttachedBackends());
  let activeKey = $derived.by((): BackendKey => {
    const thread = pane.thread;
    if (!thread) return HOME_BACKEND;
    return threadMachine(thread.id, thread.projectId);
  });
  let activeLabel = $derived.by(() => {
    const entry = attachedBackendEntry(activeKey);
    return entry ? backendDisplayName(entry) : 'Machine';
  });

  function handleTrigger(): void {
    if (isLocked) return;
    open = !open;
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl });
  }

  async function selectMachine(key: BackendKey): Promise<void> {
    if (isLocked || switching) return;
    const thread = pane.thread;
    if (!thread) return;
    if (key === activeKey) {
      closeMenu();
      return;
    }
    switching = true;
    try {
      const entry = attachedBackendEntry(key);
      if (!entry) throw new Error('That machine is no longer attached');
      // The same repo on the chosen machine when the entry spans it (a
      // TARGET choice, wave 7d); else that machine's first project.
      const project = (
        (thread.projectId ? projectSiblingOn(thread.projectId, key) : undefined)
        ?? getProjects().find((pwc) => (projectBackend(pwc.project.id) ?? HOME_BACKEND) === key)
      )?.project;
      if (!project) {
        addToast('info', `${backendDisplayName(entry)} has no projects yet`);
        return;
      }
      // Staged before the flip: the flip's own RPCs take the `selected`
      // route and must already know which machine the draft is for.
      setPaneBackend(pane.paneId, key);
      setSelectedBackend(key);
      await flipPaneDraftPlaceholder(pane, project);
    } catch (err) {
      console.error('Failed to switch draft machine:', err);
      addToast('error', userFacingError(err));
    } finally {
      switching = false;
      closeMenu();
    }
  }
</script>

{#if pane.thread}
  <button
    bind:this={triggerEl}
    type="button"
    onclick={handleTrigger}
    disabled={isLocked || switching}
    aria-haspopup={isLocked ? undefined : 'menu'}
    aria-expanded={isLocked ? undefined : open}
    data-testid="machine-picker-trigger"
    data-locked={isLocked || undefined}
    class={[
      composerTriggerClasses,
      // Same read as the locked project picker: a label, not a broken
      // control.
      isLocked ? '!opacity-100 !cursor-default hover:!bg-transparent' : '',
    ].join(' ')}
  >
    <Icon icon={MonitorIcon} size={12} strokeWidth={2} class="opacity-70" />
    <span class="truncate max-w-[160px] text-fg">{activeLabel}</span>
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
      <Menu ariaLabel="Machine" onClose={closeMenu}>
        {#each backends as entry (entry.id)}
          {@const reachable = backendReachable(entry.id)}
          <MenuItem
            label={backendDisplayName(entry)}
            description={reachable ? undefined : 'Unreachable'}
            checked={entry.id === activeKey}
            disabled={!reachable}
            title={reachable ? undefined : 'This machine cannot be reached right now'}
            onSelect={() => void selectMachine(entry.id)}
          />
        {/each}
      </Menu>
    </Popover>
  {/if}
{/if}
