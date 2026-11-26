<script lang="ts" module>
  /** A field's or a section's pin for the ticket's project, as its row or
   * header offers it. */
  export interface PinControl {
    readonly pinned: boolean;
    /** What the control says it does: "Pin Story points for web". */
    readonly label: string;
    /** Why it takes no press now; undefined when it does. */
    readonly blocked?: string;
    readonly toggle: () => void;
  }
</script>

<script lang="ts">
  // The pin of one ticket field or section for the project (docs/specs/
  // issues.md, "Ticket"): shown while pinned, revealed on hover or focus
  // of its row while not, and always under compact, where there is no
  // hover. Its row or header carries `group/pin`.
  import Pin from '@lucide/svelte/icons/pin';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';

  let { pin, testId }: { pin: PinControl; testId: string } = $props();
</script>

<span
  class="inline-flex shrink-0 {pin.pinned ? '' : 'opacity-0 focus-within:opacity-100 group-hover/pin:opacity-100 compact:opacity-100'}"
  data-pinned={pin.pinned ? '' : undefined}
>
  <IconButton label={pin.label} title={pin.blocked} size="xs" disabled={pin.blocked !== undefined} {testId} onClick={pin.toggle}>
    {#snippet children()}<Icon icon={Pin} size={12} strokeWidth={2} class={pin.pinned ? 'text-accent' : ''} />{/snippet}
  </IconButton>
</span>
