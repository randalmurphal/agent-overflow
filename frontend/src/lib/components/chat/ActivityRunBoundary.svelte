<script lang="ts">
  // The "N earlier" / "N later" edge of an activity run's mount window.
  //
  // Both edges exist because the window can sit anywhere in the run: it
  // starts at the tail, and a jump into the run relocates it around the
  // target.
  //
  // N counts every member past the edge, whether the pane holds it or the
  // server does (docs/architecture/timeline-window-pages.md §6): the reader
  // is told how much of the run is there, not how much of it happens to be
  // loaded. Mounting past the loaded span is a round trip, and the button
  // says so while it waits.
  //
  // A real button, but not the only way past the window: scrolling to the top
  // of the window pages the earlier edge in on its own (`onClipScroll` in
  // ActivityRun.svelte), so browsing back through a long run is one continuous
  // gesture. What the button adds is jumping a chunk without scrolling for it,
  // and it is the only affordance the "N later" edge has — that edge resolves
  // by returning to the clip's bottom, which releases the window pin.

  let {
    count,
    edge,
    pending = false,
    onclick,
  }: {
    /** Rows hidden past this edge. The caller only renders when it is > 0. */
    count: number;
    edge: 'earlier' | 'later';
    /** The next chunk is being fetched; the button waits rather than re-asks. */
    pending?: boolean;
    onclick: () => void;
  } = $props();
</script>

<button
  type="button"
  class="flex w-full cursor-pointer items-center gap-2 bg-transparent py-1 text-left text-[0.6875rem] text-fg-hint hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-default disabled:hover:text-fg-hint"
  class:mb-1={edge === 'earlier'}
  class:mt-1={edge === 'later'}
  disabled={pending}
  aria-busy={pending}
  {onclick}
  data-testid="activity-run-{edge}"
>
  <span aria-hidden="true">· · ·</span>
  <span>{pending ? 'Loading' : count} {edge}</span>
</button>
