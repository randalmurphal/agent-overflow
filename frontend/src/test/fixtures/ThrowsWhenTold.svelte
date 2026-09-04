<script lang="ts">
  // ThrowsOnRender's gated sibling, for boundary RETRY tests: the throw is
  // decided by a callback the test owns, so a case can turn the failure off
  // and prove the boundary's reset re-renders the children rather than
  // painting the failure row again. A getter rather than a plain prop
  // because the retry re-creates this component, and what it must read is
  // the test's CURRENT answer.
  let { shouldThrow }: { shouldThrow: () => boolean } = $props();

  // The init-time read is the point: this stands in for a component whose
  // RENDER throws.
  // svelte-ignore state_referenced_locally
  if (shouldThrow()) throw new Error('gated render failure');
</script>

<div data-testid="boundary-child">child content</div>
