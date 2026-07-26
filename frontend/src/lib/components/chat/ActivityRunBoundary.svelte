<script lang="ts">
  // The "N earlier" / "N later" edge of an activity run's mount window.
  //
  // Both edges exist because the window can sit anywhere in the run: it
  // starts at the tail, and a jump into the run relocates it around the
  // target.
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
    onclick,
  }: {
    /** Rows hidden past this edge. The caller only renders when it is > 0. */
    count: number;
    edge: 'earlier' | 'later';
    onclick: () => void;
  } = $props();
</script>

<button
  type="button"
  class="flex w-full cursor-pointer items-center gap-2 bg-transparent py-1 text-left text-[0.6875rem] text-fg-hint hover:text-fg-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
  class:mb-1={edge === 'earlier'}
  class:mt-1={edge === 'later'}
  {onclick}
  data-testid="activity-run-{edge}"
>
  <span aria-hidden="true">· · ·</span>
  <span>{count} {edge}</span>
</button>
