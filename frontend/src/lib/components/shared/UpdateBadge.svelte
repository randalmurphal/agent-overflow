<script lang="ts">
  // Small accent dot signalling a pending self-update, rendered next to the
  // Settings entry points (sidebar footer button and the Updates tab). Callers
  // gate visibility on hasPendingUpdate(); this component owns only the shared
  // dot styling and accessible label so the two call sites can't drift.
</script>

<span
  class="ml-1.5 inline-block h-1.5 w-1.5 rounded-full bg-accent align-middle"
  aria-label="Update available"
></span>
