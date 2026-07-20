<script lang="ts">
  // Fixture for svelte-patch-event-slot.test.ts: a minimal component with
  // a delegated click handler. Clicking the button routes through svelte's
  // handle_event_propagation, which stores the event in the module-level
  // `last_propagated_event` slot — the retention under test.
  let { onClicked }: { onClicked: () => void } = $props();
</script>

<button data-testid="event-slot-target" onclick={() => onClicked()}>target</button>
