<script lang="ts">
  import type { CommandOutputMeta, Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { formatTimeOfDay } from '../../utils/format';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { terminalInteractionLabelFromSummary } from './commandDisplay';
  import CommandOutput from './CommandOutput.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import Indicator from './Indicator.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  let commandOutputMeta = $derived<CommandOutputMeta | null>(
    item.payloadKind === 'command_output'
      ? (parseJsonObject(item.payloadMeta) as CommandOutputMeta | null)
      : null,
  );
  let shouldRenderCommandShell = $derived(item.payloadKind === 'command_output');
  let isRunning = $derived(item.status === 'running' || item.status === 'streaming');
  let time = $derived(formatTimeOfDay(item.createdAt));
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

<div>
  <TranscriptDisclosureHeader
    expanded={false}
    expandable={false}
    testId="terminal-interaction-row"
    class="rounded-[var(--radius-control)] px-1 py-1 text-[0.75rem] text-fg-muted"
    buttonClass="text-[0.75rem] text-fg-muted"
  >
    {#snippet icon()}<ToolKindIcon kind="clock" ariaLabel="wait" />{/snippet}
    {#snippet label()}<span data-testid="terminal-interaction-label">wait</span>{/snippet}
    {#snippet body()}<span class="min-w-0 truncate">{rowLabel}</span>{/snippet}
    {#snippet actions()}
      <ToolHeaderMeta
        statusSlotTestId="terminal-interaction-status-slot"
        timestamp={{ testId: 'terminal-interaction-time', value: item.createdAt, label: time }}
      >
        {#snippet status()}<Indicator state={isRunning ? 'running' : null} />{/snippet}
      </ToolHeaderMeta>
    {/snippet}
  </TranscriptDisclosureHeader>
  {#if shouldRenderCommandShell}
    <div class="ml-5">
      <CommandOutput
        {pane}
        item={item}
        meta={commandOutputMeta}
        payloadId={item.payloadId}
      />
    </div>
  {/if}
</div>
