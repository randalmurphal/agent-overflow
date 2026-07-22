<script lang="ts">
  // Test-only host for ChatMarkdown.settleRelex.test.ts. Holds the
  // pathRefs value in $state and passes it down the same way
  // AssistantMessage's $derived does, so a same-reference update is a
  // reactive no-op and only a genuine identity change invalidates the
  // extension chain — the discrimination testing-library's `rerender`
  // cannot express (it swaps the whole props object each call).
  import ChatMarkdown from './ChatMarkdown.svelte';
  import type { PathRef } from '../../types/models';

  let {
    source,
    initialRefs,
  }: { source: string; initialRefs: PathRef[] } = $props();

  // $state.raw, not $state: a deep-proxied array would never compare
  // equal to the raw reference a caller re-assigns, manufacturing an
  // invalidation production doesn't have (AssistantMessage's pathRefs
  // is a plain $derived value — memoized identity, no proxy).
  // svelte-ignore state_referenced_locally -- initial-value capture is
  // the point; later updates arrive via setRefs.
  let refs = $state.raw(initialRefs);

  export function setRefs(next: PathRef[]): void {
    refs = next;
  }
</script>

<ChatMarkdown {source} pathRefs={refs} />
