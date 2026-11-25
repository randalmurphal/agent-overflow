<script lang="ts">
  // Workspace strip rendered INSIDE the composer card as the bottom-
  // most row, desktop only. The machine picker leads (mounted only once
  // a second backend is attached), then the branch picker, the env
  // (workspace/worktree) picker with its optional worktree branch-name
  // input when the user has staged a new worktree, and the usage chip
  // pinned to the right as the opposing "what has this cost" element.
  // The project is not here: it is the header's crumb before the title
  // (chat/ChatHeaderProject.svelte), on every platform.
  //
  // Under compact the strip does not mount at all. The chat header's
  // facts line (chat/ChatHeaderFactsLine.svelte) shows and changes the
  // machine, project, branch and worktree there, and the usage chip
  // moves to the right end of the activity rail (ActivityRail.svelte).
  //
  // Two hosts, one strip: the main composer mounts it live, and the agent
  // pane's read-only shell mounts it with `readonly` — the same chips
  // with the same values (a subagent runs in the same thread, so env and
  // branch are literally this pane's facts), rendered inert (`inert`
  // kills pointer and focus in one attribute), with the usage slot
  // showing the SUBAGENT's own spend instead of the thread chip.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import MonitorIcon from '@lucide/svelte/icons/monitor';
  import Folder from '@lucide/svelte/icons/folder';
  import FolderGit2 from '@lucide/svelte/icons/folder-git-2';
  import Icon from '../primitives/Icon.svelte';
  import ComposerWorkspaceLiveStrip from './ComposerWorkspaceLiveStrip.svelte';
  import { composerTriggerClasses } from './triggerClasses';
  import {
    attachedBackendEntry,
    backendDisplayName,
    hasMultipleBackends,
    threadMachine,
  } from '../../stores/attachedBackends.svelte';
  import { HOME_BACKEND } from '../../transport/backendKey';
  import { checkoutDisplay, checkoutIcon, paneCheckout } from '../../utils/checkout';
  import { sameNormalizedPath } from '../../utils/path';
  import { pathBasename } from '../../utils/pathDisplay';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';

  interface Props {
    pane: ThreadPane;
    /** Inert presentation for read-only hosts: no pointer, no focus. */
    readonly?: boolean;
    /**
     * Replaces the thread usage chip in the right slot (readonly hosts
     * show the scoped entity's own numbers, not the thread's). Empty
     * leaves the slot blank in readonly mode.
     */
    usageLabel?: string;
  }

  let { pane, readonly = false, usageLabel = '' }: Props = $props();
  // An ask's copy (Thread.askCopy) is watched and stopped, never steered:
  // the pickers would move it or check out the workspace it shares, so only
  // its usage shows.
  let askCopy = $derived(Boolean(pane.thread?.askCopy));

  // Static display derivations used only when `readonly` is true so AgentPane
  // never mounts interactive pickers or subscribes to workspaceChangeLock /
  // git branch events / composerPickerRegistry.
  let readonlyMachineLabel = $derived.by(() => {
    if (!readonly || !pane.thread || !hasMultipleBackends()) return '';
    const key = threadMachine(pane.thread.id, pane.thread.projectId) ?? HOME_BACKEND;
    const entry = attachedBackendEntry(key);
    return entry ? backendDisplayName(entry) : 'Computer';
  });
  let readonlyCheckoutDisplay = $derived.by(() =>
    readonly && pane.workspace ? checkoutDisplay(paneCheckout(pane)) : null,
  );
  let readonlyAtRoot = $derived(
    readonly
      ? sameNormalizedPath(pane.thread?.workspacePath ?? '', pane.thread?.projectPath ?? '')
      : true,
  );
  let readonlyEnvLabel = $derived(
    !readonly
      ? ''
      : readonlyAtRoot
        ? 'Base'
        : pathBasename(pane.thread?.workspacePath ?? '') || 'Worktree',
  );
</script>

{#if pane.thread && !isCompactLayout()}
  {#if readonly}
    <div
      class="flex min-w-0 items-center gap-2 border-t border-border-subtle px-3 py-1.5 text-[0.6875rem] text-fg-muted"
      data-testid="composer-workspace-strip"
      inert
    >
      <div class="flex min-w-0 flex-1 items-center gap-2">
        {#if hasMultipleBackends() && !askCopy}
          <span class="{composerTriggerClasses} min-w-0 max-w-full" data-testid="machine-picker-trigger">
            <Icon icon={MonitorIcon} size={11} strokeWidth={2} class="shrink-0 opacity-85" />
            <span class="truncate max-w-[140px]">{readonlyMachineLabel}</span>
          </span>
        {/if}
        {#if pane.workspace && !askCopy && readonlyCheckoutDisplay}
          <span class="{composerTriggerClasses} min-w-0 max-w-full" data-testid="branch-picker-trigger">
            <Icon icon={checkoutIcon(readonlyCheckoutDisplay.kind)} size={11} strokeWidth={2} class="shrink-0 opacity-85" />
            <span class="truncate max-w-[160px]">{readonlyCheckoutDisplay.label}</span>
          </span>
          <span class="{composerTriggerClasses} min-w-0 max-w-full" data-testid="env-picker-trigger">
            <Icon icon={readonlyAtRoot ? Folder : FolderGit2} size={11} strokeWidth={2} class="shrink-0 opacity-85" />
            <span class="truncate max-w-[140px]">{readonlyEnvLabel}</span>
          </span>
        {/if}
      </div>
      <div class="ml-auto shrink-0 whitespace-nowrap">
        {#if usageLabel}
          <span class="{composerTriggerClasses} tabular-nums" data-testid="workspace-strip-usage">
            {usageLabel}
          </span>
        {/if}
      </div>
    </div>
  {:else}
    <ComposerWorkspaceLiveStrip {pane} {askCopy} />
  {/if}
{/if}
