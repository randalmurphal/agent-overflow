<script lang="ts">
  // Retention-probe helper: a minimal component with a delegated click
  // handler. Dispatching a click on the button routes through svelte's
  // handle_event_propagation, overwriting the module-level
  // `last_propagated_event` slot so a previously dispatched event (and
  // the detached tree its `target` anchors) stops being retained.
</script>

<button data-testid="event-slot-flush" onclick={() => {}}>flush</button>
