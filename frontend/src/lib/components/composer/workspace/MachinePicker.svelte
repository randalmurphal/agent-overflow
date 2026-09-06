<script lang="ts">
  // A draft stays on its repository when changing computers. If the target
  // has no registered checkout, ask for that folder explicitly.

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
  import { getProject, projectSiblingOn } from '../../../stores/projects.svelte';
  import { flipPaneDraftPlaceholder } from '../../../stores/threadCreation.svelte';
  import { setPaneBackend, setSelectedBackend } from '../../../stores/selectedBackend.svelte';
  import {
    attachedBackendEntry,
    backendDisplayName,
    backendReachable,
    getAttachedBackends,
    threadMachine,
  } from '../../../stores/attachedBackends.svelte';
  import { HOME_BACKEND, type BackendKey } from '../../../transport/backendKey';
  import { rememberProjectTarget } from '../../../stores/projectTargets';
  import AddProjectModal from '../../sidebar/AddProjectModal.svelte';
  import type { Project } from '../../../types/models';
  import { addToast } from '../../../stores/toast.svelte';
  import { userFacingError } from '../../../utils/userFacingError';
  import { canOfferConversationTransfer, openConversationTransfer, supportsConversationTransfer } from '../../../stores/conversationTransfers.svelte';
  import { hasScope } from '../../../transport/scopes';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let triggerEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let switching = $state(false);
  let addOn = $state<BackendKey | null>(null);
  let addForThread: string | null = null;

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
  let canTransfer = $derived(Boolean(pane.thread && isLocked && canOfferConversationTransfer(pane.thread) && hasScope('threads:operate', activeKey)));
  let selectable = $derived(!isLocked || canTransfer);

  function handleTrigger(): void {
    if (!selectable) return;
    open = !open;
  }

  function closeMenu(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl });
  }

  async function selectMachine(key: BackendKey): Promise<void> {
    if (!selectable || switching) return;
    if (!backendReachable(key) || !hasScope('threads:operate', key)) return;
    const thread = pane.thread;
    if (!thread) return;
    if (key === activeKey) {
      closeMenu();
      return;
    }
    if (isLocked) {
      closeMenu();
      openConversationTransfer(thread, key);
      return;
    }
    switching = true;
    try {
      const entry = attachedBackendEntry(key);
      if (!entry) throw new Error('That machine is no longer attached');
      const project = thread.projectId ? projectSiblingOn(thread.projectId, key)?.project : undefined;
      if (!project) {
        addForThread = thread.id;
        addOn = key;
        return;
      }
      await switchToProject(project, key);
    } catch (err) {
      console.error('Failed to switch draft machine:', err);
      addToast('error', userFacingError(err));
    } finally {
      switching = false;
      closeMenu();
    }
  }
  async function switchToProject(project: Project, key: BackendKey): Promise<void> {
    if (await flipPaneDraftPlaceholder(pane, project)) {
      setPaneBackend(pane.paneId, key);
      setSelectedBackend(key);
      rememberProjectTarget(project, key);
    }
  }

  async function useAddedProject(project: Project): Promise<void> {
    const key = addOn;
    addOn = null;
    if (key === null || pane.thread?.id !== addForThread || isLocked) return;
    switching = true;
    try {
      await switchToProject(project, key);
    } catch (err) {
      addToast('error', userFacingError(err));
    } finally {
      switching = false;
    }
  }

</script>

{#if pane.thread}
  <button
    bind:this={triggerEl}
    type="button"
    onclick={handleTrigger}
    disabled={!selectable || switching}
    aria-haspopup={selectable ? 'menu' : undefined}
    aria-expanded={selectable ? open : undefined}
    data-workspace-location
    data-testid="machine-picker-trigger"
    data-locked={!selectable || undefined}
    class={[
      composerTriggerClasses,
      // Same read as the locked project picker: a label, not a broken
      // control.
      !selectable ? '!opacity-100 !cursor-default hover:!bg-transparent' : '',
    ].join(' ')}
  >
    <Icon icon={MonitorIcon} size={12} strokeWidth={2} class="opacity-70" />
    <span class="truncate max-w-[160px] text-fg">{activeLabel}</span>
    {#if selectable}
      <Icon icon={ChevronDown} size={12} strokeWidth={2} class="opacity-60" />
    {/if}
  </button>

  {#if selectable}
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
          {@const viewOnly = !hasScope('threads:operate', entry.id)}
          {@const transferUnavailable = isLocked && entry.id !== activeKey && (!supportsConversationTransfer(entry.id) || !hasScope('threads:operate', entry.id))}
          <MenuItem
            label={backendDisplayName(entry)}
            description={!reachable ? 'Unreachable' : viewOnly ? 'View only' : transferUnavailable ? 'Update required' : isLocked && entry.id !== activeKey ? 'Move or copy conversation…' : undefined}
            checked={entry.id === activeKey}
            disabled={!reachable || viewOnly || transferUnavailable}
            title={!reachable
              ? 'This machine cannot be reached right now'
              : viewOnly
                ? 'This connection can view threads but cannot create or move them'
                : undefined}
            onSelect={() => void selectMachine(entry.id)}
          />
        {/each}
      </Menu>
    </Popover>
  {/if}
{/if}


{#if addOn !== null}
  <AddProjectModal
    open={true}
    initialBackend={addOn}
    onClose={() => { addOn = null; }}
    onCreated={(project) => void useAddedProject(project)}
    onDuplicate={(id) => {
      const project = getProject(id)?.project;
      if (project) void useAddedProject(project);
    }}
  />
{/if}
