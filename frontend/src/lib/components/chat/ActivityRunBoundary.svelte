<script lang="ts">
  // The "N earlier" / "N later" edge of an activity run's mount window.
  //
  // Both edges exist because the window can sit anywhere in the run: it
  // starts at the tail, and a jump into the run relocates it around the
  // target. A boundary is the only way past the window, so it is a real
  // button rather than a scroll-triggered auto-load — mounting rows the
  // reader did not ask for is what the window exists to prevent.

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
