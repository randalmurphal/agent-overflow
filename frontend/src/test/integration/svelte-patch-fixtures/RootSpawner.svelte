<script lang="ts">
  // Calls `spawn` during component init/render — the shape that made
  // store-level $effect.root entries inherit this component's context
  // (and thereby pin its props, scopes, and DOM) before the
  // ownerless-roots patch hunk.
  interface Props {
    spawn: () => void;
    title: string;
  }
  let { spawn, title }: Props = $props();
  // svelte-ignore state_referenced_locally
  spawn();
</script>

<div>{title}</div>
