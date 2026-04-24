<script lang="ts">
  import Timer from 'lucide-svelte/icons/timer';
  import Icon from '../primitives/Icon.svelte';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import CommandOutput from './CommandOutput.svelte';

  let { item }: { item: Item } = $props();

  function parseCommandOutputMeta(raw: string | undefined): CommandOutputMeta | null {
    if (!raw) return null;
    try {
      return JSON.parse(raw) as CommandOutputMeta;
    } catch {
      return null;
    }
  }

  let commandOutputMeta = $derived<CommandOutputMeta | null>(
    item.payloadKind === 'command_output' && item.payloadId
      ? parseCommandOutputMeta(item.payloadMeta)
      : null,
  );

  /**
   * The Codex app-server emits one `TerminalInteractionNotification`
   * per poll of a backgrounded PTY. Each one becomes a single row —
   * matching Codex's TUI which renders "Waited for background
   * terminal" on every empty-stdin `write_stdin`. No collapse /
   * grouping here; that's a separate UX decision.
   */
</script>

<div class="mb-1.5">
  <div
    class="flex items-center gap-1.5 px-2 py-1 text-[11px] italic text-fg-subtle"
    data-testid="terminal-interaction-row"
  >
    <Icon icon={Timer} size={11} strokeWidth={2} class="opacity-70 shrink-0" />
    <span>{item.summary || 'Waited for background terminal'}</span>
  </div>
  {#if commandOutputMeta && item.payloadId}
    <div class="ml-5">
      <CommandOutput item={item} meta={commandOutputMeta} payloadId={item.payloadId} />
    </div>
  {/if}
</div>
