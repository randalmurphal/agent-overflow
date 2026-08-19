<script lang="ts">
  // The run map (RUN-MAP §1): one vertical line where position is progress,
  // solid is what happened, the marked node is now, and dashed is not yet.
  //
  // This component is orchestration only — attach the run's map entity, thread
  // the shared 1Hz clock into the pure projection, draw the frontier strip and
  // the wave list, and own the follow chip. Every rule about what a node MEANS
  // lives in `utils/workflowRunMap*`; every rule about scroll lives in
  // `runMapFollow.svelte.ts`. Nothing here decides either.

  import { untrack } from 'svelte';
  import WorkflowRunMapFrontierStrip from './WorkflowRunMapFrontierStrip.svelte';
  import WorkflowRunMapWave from './WorkflowRunMapWave.svelte';
  import { createRunMapFollow } from './runMapFollow.svelte';
  import { requireWorkflowsOverlayScroller } from './overlayScroller';
  import { runMapRefusalHeadline } from '../../utils/workflowRunMapStyle';
  import { createSharedNowClock } from '../chat/useRunningElapsed.svelte';
  import { buildRunMap, runMapViewIsLive, runMapWaveIsOpen } from '../../utils/workflowRunMap';
  import {
    attachWorkflowRunMap,
    peekWorkflowRunMap,
    peekWorkflowRunMapError,
  } from '../../stores/workflowRunMap.svelte';
  import { openWorkflowThreadById } from '../../stores/workflowThreads';
  import {
    getWorkflowRunMapExpansion,
    toggleWorkflowRunMapComposition,
    toggleWorkflowRunMapLane,
    toggleWorkflowRunMapWave,
  } from '../../stores/workflowsOverlay.svelte';

  interface Props {
    /** The run the overlay is looking at; the view covers its whole tree. */
    itemId: string;
  }
  let { itemId }: Props = $props();

  let rootEl = $state<HTMLElement | null>(null);

  // The store owns the RPC, the event patching, the retry curve and the
  // transport edge. This effect owns only the reference's lifetime, and it is
  // keyed on the entity id ALONE (getter-ctx rule).
  $effect(() => {
    const attachment = attachWorkflowRunMap(itemId);
    return () => attachment.release();
  });

  let view = $derived(peekWorkflowRunMap(itemId));
  let error = $derived(peekWorkflowRunMapError(itemId));

  // The clock gates on the VIEW, never on the model: the model is built from
  // the clock, so a predicate that read it would re-arm the ticker every tick.
  let live = $derived(view !== null && runMapViewIsLive(view));
  const clock = createSharedNowClock(() => live);

  let expansion = $derived(getWorkflowRunMapExpansion(itemId));
  // ONE projection per tick, for the whole surface. The expansion sets go IN
  // rather than a per-wave builder coming out: a builder per expanded wave
  // rebuilt the tree index and the frontier once per wave per second.
  let model = $derived(view !== null
    ? buildRunMap(view, clock.now, {
      expandedWaveIds: expansion.waves,
      expandedCompositionIds: expansion.compositions,
      expandedLaneIds: expansion.lanes,
    })
    : null);
  let followTarget = $derived(model?.followTarget ?? null);
  let nowKey = $derived(followTarget?.key ?? '');
  let loop = $derived(model?.loop ?? null);
  // Precomputed by the projection, like every other display string on the
  // surface: the summary has to distinguish "$X spent" from a total that is a
  // LOWER BOUND because some ledger rows could not be priced, and that
  // distinction is a projection rule, not a template one.
  let money = $derived(model?.moneyLabel ?? '');
  // The other two ceiling kinds, in their own units. Same rule as the money
  // line: a token or wall-clock bound is what will actually stop the run, and
  // the projection is where it is worded.
  let budget = $derived(model?.budgetLabel ?? '');
  // §4.2: a refusal is DATA, not failure — the RPC succeeded and the answer is
  // "never". It therefore renders as a state of its own rather than through the
  // error path, whose whole shape (a retry ladder behind it) is wrong for an
  // answer that cannot change.
  let refusal = $derived(model?.refusal ?? null);

  // The overlay body is the scroller and it is an ANCESTOR of this component,
  // so the frame that owns it hands it down (§9.9). Required, not optional: a
  // map with no scroller cannot place, jump, follow or compensate, and the
  // context read throws rather than letting all four quietly do nothing.
  const scrollerOf = requireWorkflowsOverlayScroller();

  // The resolved follow-target element, keyed on the target it was resolved
  // FOR. The controller asks for it on every scroll frame, resize and frontier
  // move, and the answer is a DOM query over the whole map.
  let cachedTargetKey = '';
  let cachedTargetEl: HTMLElement | null = null;

  /**
   * The element the follow target currently occupies: the marked row when its
   * wave is expanded, else the wave's own row — a folded wave is where a jump
   * has to land, since the row is all there is of it on screen.
   *
   * Only the marked row is cached. It is the hot answer and it is self-
   * invalidating (Svelte detaches the element when the row goes), whereas the
   * wave-row fallback is answered by "no marked row exists YET" — a marked row
   * appearing is exactly what must end it, and that is not visible from the
   * cached element.
   */
  function followTargetEl(): HTMLElement | null {
    const root = rootEl;
    const target = followTarget;
    if (root === null || target === null) return null;
    if (cachedTargetKey === target.key && cachedTargetEl?.isConnected === true) return cachedTargetEl;
    const marked = root.querySelector<HTMLElement>('[data-run-map-now="true"]');
    if (marked !== null) {
      cachedTargetKey = target.key;
      cachedTargetEl = marked;
      return marked;
    }
    cachedTargetKey = '';
    cachedTargetEl = null;
    for (const candidate of root.querySelectorAll<HTMLElement>('[data-wave-item-id]')) {
      if (candidate.dataset.waveItemId === target.waveItemId) return candidate;
    }
    return null;
  }

  // Plain variables, not `$state`: the follow controller reads them through
  // getters when it acts, and an effect that both read and wrote reactive
  // state here would re-enter itself.
  let placedFor = '';
  let openedRunning = false;
  let lastFollowKey = '';

  const follow = createRunMapFollow({
    scroller: scrollerOf,
    followTargetEl,
    followDefault: () => openedRunning,
  });

  // `untrack` on every controller call, without exception: the controller
  // reads the follow target through the getters above, and the model behind
  // them is rebuilt on every clock tick. A tracked call would therefore tear
  // down and reinstall eight listeners and a ResizeObserver once a second —
  // and a detach landing mid-gesture drops the touch sequence it was reading.
  $effect(() => untrack(() => follow.attach()));

  // Placement on open (§9.5) — placement, not scrolling, and only once per run.
  // Keyed on the VIEW rather than the model: the model is a fresh object every
  // tick, and "has the first answer landed" is a question about the view.
  $effect(() => {
    if (view === null || placedFor === itemId) return;
    placedFor = itemId;
    // §9.4: follow is ON for a running run and OFF for a parked or terminal
    // one, where the digest and the cause are the payload. The discriminator
    // is the follow TARGET, not the root run's own state — a campaign root is
    // `done` the moment it calls the next wave, while the tree it heads is
    // very much running, and reading the root would open a live run parked.
    untrack(() => {
      openedRunning = followTarget !== null && !followTarget.needsHuman;
      lastFollowKey = nowKey;
      follow.placeOnOpen();
    });
  });

  // §7, "transport gap": a refetch — the reconnect resync, or a patch that
  // could not be placed exactly — replaces the WHOLE view, and every wave in
  // the model with it. For a reader who is not following, that is the same
  // layout mutation a fold is (§9.7), and it rides the same anchor hold: the
  // recovery must not move their viewport. It cannot use `holdAnchor` because
  // the swap is not this component's own call to make — Svelte applies it on a
  // flush of its own — so the hold is measured in a PRE effect (before the DOM
  // changes) and released in the matching post effect.
  let releaseAnchor: (() => void) | null = null;

  $effect.pre(() => {
    void view;
    untrack(() => {
      // Nothing to hold before the first answer: there is no previous layout,
      // and placement (§9.5) has the last word about where an opening run sits.
      if (placedFor !== itemId) return;
      releaseAnchor = follow.captureAnchor();
    });
  });

  $effect(() => {
    void view;
    untrack(() => {
      releaseAnchor?.();
      releaseAnchor = null;
    });
  });

  // The frontier advanced (or a wave folded under it): the controller decides
  // whether that is a scroll write — this only reports the move. `nowKey` is a
  // string, so the derived stops propagating when the target holds still.
  $effect(() => {
    if (nowKey === lastFollowKey) return;
    lastFollowKey = nowKey;
    untrack(() => follow.onFollowTargetChanged());
  });

  // Map-initiated height changes ride the anchor hold, so a reader who is not
  // following never has the page move under them (§9.7). Every fold on the
  // surface goes through one of these three — a lap, a called run, a fan lane —
  // so none of them can grow the document outside a hold.
  function toggleWave(waveItemId: string): void {
    follow.holdAnchor(() => toggleWorkflowRunMapWave(itemId, waveItemId));
  }

  function toggleComposition(compositionItemId: string): void {
    follow.holdAnchor(() => toggleWorkflowRunMapComposition(itemId, compositionItemId));
  }

  function toggleLane(branchKey: string): void {
    follow.holdAnchor(() => toggleWorkflowRunMapLane(itemId, branchKey));
  }

  function openThread(threadId: string): void {
    if (!threadId) return;
    void openWorkflowThreadById(threadId);
  }
</script>

<div class="relative" bind:this={rootEl} data-testid="workflow-run-map" data-item-id={itemId}>
  {#if error}
    <p class="rounded-md border border-error/40 px-2 py-1 text-xs text-error" data-testid="workflow-map-error">
      {error}
    </p>
  {/if}

  {#if model === null}
    {#if !error}
      <p class="py-2 text-xs text-fg-muted" data-testid="workflow-map-loading">Loading run map…</p>
    {/if}
  {:else if refusal !== null}
    <!--
      A refused map has no waves and never will. The headline says what it means
      for this surface; the backend's sentence names the run it happened to,
      and is already written for a reader (R2: no code, no path, no type).
    -->
    <div
      class="rounded-md border border-border-subtle bg-surface-1/40 px-3 py-2"
      data-testid="workflow-map-refusal"
      data-refusal-code={refusal.code}
    >
      <p class="text-xs font-medium text-fg">{runMapRefusalHeadline(refusal.code)}</p>
      <p class="mt-0.5 text-[0.6875rem] text-fg-muted">{refusal.message}</p>
    </div>
  {:else}
    {#if followTarget !== null || loop !== null || money !== '' || budget !== ''}
      <WorkflowRunMapFrontierStrip target={followTarget} {loop} {money} {budget} nowMs={clock.now} />
    {/if}

    <ol class="run-map-spine run-map-spine-wide" data-testid="workflow-map-waves">
      {#each model.waves as wave (wave.key)}
        <!--
          `data-wave-expanded`, not `data-expanded`: the latter is a namespace
          the vendored streamdown's fullscreen rule reaches into (DIVERGENCE
          entry 16), and a run map inside a markdown surface must not inherit
          a layout rule about mermaid diagrams.
        -->
        <li
          data-testid="workflow-map-wave"
          data-wave-item-id={wave.itemId}
          data-wave-expanded={runMapWaveIsOpen(wave)}
        >
          <WorkflowRunMapWave
            {wave}
            {nowKey}
            onOpenThread={openThread}
            onToggleWave={toggleWave}
            onToggleComposition={toggleComposition}
            onToggleLane={toggleLane}
          />
        </li>
      {/each}
    </ol>

    {#if model.waves.length === 0}
      <p class="py-2 text-xs text-fg-muted" data-testid="workflow-map-empty">This run has nothing to show yet.</p>
    {/if}
  {/if}

  <!--
    The jump chip sits OUTSIDE the scrolled content (§9.10, the
    ScrollToBottomButton lesson). The overlay's scroller is an ancestor, not
    this component's own element, so "outside" is expressed as a zero-height
    sticky layer at the map level: it never scrolls away with the content and
    never takes part in the map's flow.
  -->
  <div class="pointer-events-none sticky bottom-3 z-10 flex h-0 justify-end pr-1">
    {#if follow.chipVisible}
      <button
        type="button"
        class="pointer-events-auto -translate-y-full rounded-full border border-border-subtle bg-surface-2 px-2.5 py-1 text-[0.6875rem] font-medium text-accent shadow-sheet"
        onclick={() => follow.engage()}
        data-testid="workflow-map-follow"
      >
        now ▸
      </button>
    {/if}
  </div>
</div>
