<script lang="ts">
  // Test-only harness. Conditionally mounts <ChatMarkdown> so a single
  // top-level vitest render() can observe mount → unmount → remount
  // behavior without re-evaluating the module (vitest's Svelte
  // transform re-evaluates the module per top-level render() — see
  // the note in ChatMarkdown.cacheHits.test.ts — so a fresh render()
  // call cannot observe module-level state populated by an earlier
  // mount). Toggling the `show` prop with `rerender(...)` exercises
  // the same render root and preserves module-level caches across
  // the mount transition.

  import ChatMarkdown from './ChatMarkdown.svelte';

  let { source, show }: { source: string; show: boolean } = $props();
</script>

{#if show}
  <ChatMarkdown {source} pathRefs={[]} />
{/if}
