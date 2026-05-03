<script lang="ts">
  import Timer from 'lucide-svelte/icons/timer';
  import Icon from '../primitives/Icon.svelte';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { commandTextForItem, terminalInteractionLabelFromSummary } from './commandDisplay';
  import CommandOutput from './CommandOutput.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  let commandOutputMeta = $derived<CommandOutputMeta | null>(
    item.payloadKind === 'command_output'
      ? (parseJsonObject(item.payloadMeta) as CommandOutputMeta | null)
      : null,
  );
  let commandText = $derived(commandTextForItem(item, commandOutputMeta));
  let shouldRenderCommandShell = $derived(item.payloadKind === 'command_output' || commandText !== '');
  let rowLabel = $derived(
    shouldRenderCommandShell
      ? terminalInteractionLabelFromSummary(item.summary)
      : item.summary || 'Waited for background terminal',
  );

  /**
   * The Codex app-server emits one `TerminalInteractionNotification`
   * per poll of a backgrounded PTY. Each one becomes a single row —
   * matching Codex's TUI which renders "Waited for background
   * terminal" on every empty-stdin `write_stdin`. No collapse /
   * grouping here; that's a separate UX decision.
   */
</script>

{#if item.payloadKind === 'command_output'}
  <CommandOutput
    {pane}
    item={item}
    meta={commandOutputMeta}
    payloadId={item.payloadId}
    showCompletionBadge={Boolean(item.payloadId)}
  />
{:else}
  <div class="mb-1.5">
    <div
      class="flex items-center gap-1.5 px-2 py-1 text-[11px] italic text-fg-subtle"
      data-testid="terminal-interaction-row"
    >
      <Icon icon={Timer} size={11} strokeWidth={2} class="opacity-70 shrink-0" />
      <span>{rowLabel}</span>
    </div>
    {#if shouldRenderCommandShell}
      <div class="ml-5">
        <CommandOutput
          {pane}
          item={item}
          meta={commandOutputMeta}
          payloadId={item.payloadId}
          showCompletionBadge={Boolean(item.payloadId)}
        />
      </div>
    {/if}
  </div>
{/if}
