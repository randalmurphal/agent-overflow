<script lang="ts">
  import Clock from 'lucide-svelte/icons/clock';
  import Icon from '../primitives/Icon.svelte';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { terminalInteractionLabelFromSummary } from './commandDisplay';
  import CommandOutput from './CommandOutput.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  let commandOutputMeta = $derived<CommandOutputMeta | null>(
    item.payloadKind === 'command_output'
      ? (parseJsonObject(item.payloadMeta) as CommandOutputMeta | null)
      : null,
  );
  let shouldRenderCommandShell = $derived(item.payloadKind === 'command_output');
  let isRunning = $derived(item.status === 'running' || item.status === 'streaming');
  let rowLabel = $derived.by(() => {
    if (isRunning) return 'Waiting for background terminal';
    return terminalInteractionLabelFromSummary(item.summary) || 'Waited for background terminal';
  });

  /**
   * The Codex app-server emits `TerminalInteractionNotification` when
   * write_stdin targets a backgrounded PTY. Empty polls reuse one live
   * wait row until the backend flushes it; non-empty input renders as a
   * separate redacted interaction marker.
   */
</script>

<div class="mb-1.5">
  <div
    class="flex items-center gap-2 px-1 py-1 text-[12px] text-fg-muted"
    data-testid="terminal-interaction-row"
  >
    <Icon icon={Clock} size={13} strokeWidth={2} class="shrink-0 opacity-75" />
    <span class="min-w-0 truncate">{rowLabel}</span>
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
