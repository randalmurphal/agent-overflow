<script lang="ts">
  // Terminal chips staged for the next send, each expandable to its full
  // captured output. Sibling of ComposerAttachmentRow: same slot in the
  // card, same "staged for this message" role.

  import ComposerTerminalChip from './ComposerTerminalChip.svelte';
  import type { TerminalChip } from '../../types/draft';

  interface Props {
    chips: TerminalChip[];
    onRemove: (id: string) => void;
    /**
     * Hides the row without unmounting it, so which chips the user had
     * expanded survives a prompt taking over the composer card and
     * handing it back.
     */
    visible?: boolean;
  }

  let { chips, onRemove, visible = true }: Props = $props();

  let expandedChips = new Set<string>();
  let expandedVersion = $state(0);

  function handleToggleChip(id: string) {
    if (expandedChips.has(id)) {
      expandedChips.delete(id);
    } else {
      expandedChips.add(id);
    }
    expandedVersion++;
  }

  function isChipExpanded(id: string): boolean {
    void expandedVersion;
    return expandedChips.has(id);
  }
</script>

{#if visible && chips.length > 0}
  <div
    class="flex flex-col gap-1 border-b border-border-subtle px-4 py-2"
    data-testid="terminal-chip-row"
  >
    {#each chips as chip (chip.id)}
      <ComposerTerminalChip
        {chip}
        expanded={isChipExpanded(chip.id)}
        onToggle={handleToggleChip}
        onRemove={onRemove}
      />
    {/each}
  </div>
{/if}
