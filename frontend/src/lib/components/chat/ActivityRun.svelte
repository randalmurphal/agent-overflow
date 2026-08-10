<script lang="ts">
  // One maximal run of consecutive activity rows (tool calls, completions,
  // thinking, and the group containers that sit on the same rail), rendered
  // as a single timeline row.
  //
  // A header line that is always there, and under it a height-capped clip
  // that comes and goes. Expanded, the clip scrolls in place — a run shorter
  // than the cap renders exactly as it did before this component existed,
  // which is why no row-count threshold is needed. Collapsed, the header is
  // all that is left.
  //
  // With one overlap: a run the thread's defaults would collapse still keeps
  // its clip while it is LIVE, so collapsing a thread never means going blind
  // to what it is doing right now — and it keeps it after settling, too,
  // until the timeline's gate collapses it off-screen
  // (timelineActivityRunAutoCollapse.ts). Both are the registry's resolution
  // (`collapsedFor`), so `run.collapsed` is the whole answer here: this
  // component renders presence, it never decides it.
  //
  // The rail belongs to the run, not to its rows: one continuous border for
  // the block of rows, doubling as a second, larger collapse target. Per-row
  // borders would draw the same line but could not be clicked as one thing.
  // It spans the CLIP only — the header is the run's own boundary, and a
  // line beside it doubled that edge; instead the header's chevron sits
  // centred on the rail's x, reading as the line folded into its control.
  //
  // Only the run holding the live tail gets a scroll controller. Historical
  // runs never chase, so they need no ResizeObserver, no intent listeners,
  // no warm gate, and no composited layer — a controller each would tax
  // every run in the buffer for physics only one of them can use.

  import type { Snippet } from 'svelte';
  import { tick } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { withViewportBottomHeld } from '../../stores/threadPaneShared';
  import type { ActivityRunNode, TimelineNode } from '../../utils/subagentGrouping';
  import { timelineNodeItemId, timelineNodeKey } from '../../utils/subagentGrouping';
  import {
    activityRunRowIndexOfItem,
    activityRunWindowGrownNewer,
    activityRunWindowGrownOlder,
  } from '../../utils/activityRunWindow';
  import {
    activityRunCenteredScrollTop,
    activityRunChildElement,
    activityRunClipMaxHeight,
    activityRunAtBottom,
    activityRunRowFullyVisible,
    activityRunRowViewportTop,
    activityRunScrollTopHoldingRow,
    activityRunShouldMountEarlier,
    observeActivityRunExpansion,
  } from '../../utils/activityRunClip';
  import { readScrollMetrics, type ScrollMetrics } from '../../utils/scroll/overlayScrollbar';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';
  import {
    createUseStickToBottomController,
    type UseStickToBottomController,
  } from '../../utils/scroll/index.svelte';
  import {
    isLiveContentActive,
    LIVE_CONTENT_ACTIVE_HOLD_MS,
  } from '../../utils/liveContentActivity';
  import OverlayScrollbar from '../shared/OverlayScrollbar.svelte';
  import ActivityRunBoundary from './ActivityRunBoundary.svelte';
  import ActivityRunHeader from './ActivityRunHeader.svelte';

  let {
    pane,
    run,
    depth,
    live,
    renderNode,
  }: {
    pane: ThreadPane;
    run: ActivityRunNode;
    depth: number;
    /** This run holds the timeline's tail, so new activity lands here. */
    live: boolean;
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();

  let clipEl = $state<HTMLElement | undefined>();
  let contentEl = $state<HTMLElement | undefined>();
  let expandedPx = $state(0);

  // The run's identity as a PRIMITIVE. Every projection pass hands this
  // component a fresh node object, so an effect that reads `run.runId`
  // subscribes to the whole prop and re-runs on every appended row. For the
  // controller effect below that would mean tearing down and rebuilding the
  // spring, its observers, and its warm gate on every streamed activity row —
  // exactly the mid-turn controller churn the registry's stable `runId` exists
  // to prevent. A derived primitive recomputes but does not propagate while
  // its value is unchanged, so the effect sees the identity without the churn.
  let runId = $derived(run.runId);

  // The same treatment, for the same reason, on the two facts the effects
  // below branch on. Both arrive as plain reads of a prop the projection
  // replaces on every streamed row — `run.collapsed` off a fresh node object,
  // and `live` as a pass-through of one — so an effect reading either directly
  // re-runs every pass even though neither has changed. That was measurable,
  // not theoretical: it tore the controller down and rebuilt it mid-gesture,
  // dropping the arming that tells the run a reader is the one scrolling it.
  let collapsed = $derived(run.collapsed);
  let isLive = $derived(live);
  // And on the mount window's head, which the compensation below must react to
  // exactly when it moves and never on the passes where it has not.
  let mountedFrom = $derived(run.mountedFrom);

  // Built only for the live run, and only while it is live — a run that a
  // later one displaces has no use for a spring, and a controller per run
  // in the buffer would be a spring, an observer set, and intent listeners
  // each for physics only one of them can use.
  //
  // Same factory, spring constants, and glide compositing as the main pane,
  // so a streaming run feels identical to a streaming thread. It NEVER
  // calls `pane.attachScrollController`: that slot is single-occupancy and
  // belongs to the timeline.
  //
  // `externalContentGeometry` is deliberately unset. There is no
  // virtualizer inside a run, so the controller's own contentEl
  // ResizeObserver is the right geometry source (the ChannelView
  // precedent); the timeline leaves it set because its engine reports
  // geometry the RO would otherwise have to re-derive.
  //
  // `$state.raw` because the controller is compared by identity: a plain
  // `$state` proxies the object it holds, so `stick === controller` in the
  // teardown below was permanently false and the slot was never cleared.
  // Nothing here wants deep reactivity over a controller — its own state is
  // already reactive from the inside. The pane's own controller slot
  // (`thread.svelte.ts`) is `raw` for exactly this reason; it had the same
  // defect, found by the Svelte proxy-equality warning this suite emitted.
  let stick = $state.raw<UseStickToBottomController | null>(null);

  function createStick(): UseStickToBottomController {
    return createUseStickToBottomController({
      liveContentActive: () =>
        isLiveContentActive(
          performance.now(),
          pane.lastLiveContentAt,
          LIVE_CONTENT_ACTIVE_HOLD_MS,
        ),
    });
  }

  // Mount window: only `mountedRows` children starting at `mountedFrom` are
  // in the DOM. Without it a 200-row run would mount 200 rows the moment its
  // single timeline row entered the virtualizer's buffer — the DOM bound the
  // virtualizer provides at top level has to be re-established inside. The
  // window rests on the run's tail and relocates when a jump resolves into
  // the run (utils/activityRunWindow.ts).
  let mountedChildren = $derived(
    run.children.slice(run.mountedFrom, run.mountedFrom + run.mountedRows),
  );
  let hiddenEarlier = $derived(mountedFrom);
  let hiddenLater = $derived(
    run.children.length - run.mountedFrom - run.mountedRows,
  );

  // Run ids are minted per REGISTRY, so this one needs the pane scope more
  // than the item-keyed ids do: every pane's first run is `r1`, so two panes
  // collide even on different threads. Passed to the header rather than
  // rebuilt there (utils/chatDomIds.ts).
  let clipId = $derived(chatRowDomId(pane, 'activity-run', run.runId));
  let maxHeight = $derived(activityRunClipMaxHeight(expandedPx));

  // Held at the viewport's bottom edge: a run the reader just expanded opens
  // UPWARD, above the rows they are already reading, instead of shoving those
  // rows down the page — and collapsing gives that height back the same way.
  // The transaction also keeps the spring out of it, so an expand while stuck
  // at the bottom is instant rather than an animated ride across the delta.
  function toggle(): void {
    withViewportBottomHeld(pane.scrollController, () => {
      // The state on screen, not the registry's idea of it: a run with no
      // override renders open while it is live whatever the defaults say, so
      // asking the registry to invert its own answer would hand back the state
      // the reader is already looking at.
      pane.activityRuns.setCollapsed(run.runId, !collapsed);
    });
  }

  // Guards `mountEarlier` against overlap. Deliberately a plain local, not
  // `$state`: nothing renders from it, and a reactive read would invalidate
  // the template twice per chunk for no visible difference.
  let mountingEarlier = false;

  async function mountEarlier(): Promise<void> {
    // Narrowing, not a fallback: the boundary that calls this renders INSIDE
    // the clip, so the element is bound before any click can reach it.
    const clip = clipEl;
    // Two paths reach here — the boundary button and the scroll trigger below
    // — and either can arrive while the other is still awaiting its tick.
    // Overlapping mounts would each measure a `scrollHeight` the other is
    // about to change and compensate by the wrong amount, so the second waits
    // for the next scroll event instead. `hiddenEarlier` is re-checked because
    // the prop that feeds it does not update until that tick.
    if (!clip || mountingEarlier || hiddenEarlier <= 0) return;
    mountingEarlier = true;
    try {
      const beforeHeight = clip.scrollHeight;
      const beforeTop = clip.scrollTop;

      pane.activityRuns.setMountWindow(run.runId, activityRunWindowGrownOlder(run));
      await tick();

      // Manual prepend compensation. WebKit has no `overflow-anchor`, so
      // rows added ABOVE the viewport would otherwise push the reading
      // position down by exactly their height. Two reads and a write, after
      // the DOM has the new rows and before the user can see the frame.
      const grew = clip.scrollHeight - beforeHeight;
      if (grew > 0) clip.scrollTop = beforeTop + grew;
      // Read back before the settle observer sees the same growth: rows
      // arriving ABOVE the reader must not be mistaken for the run growing
      // under one who is resting on its last row. The follow state carries
      // through unchanged — compensation keeps the distance to the run's last
      // row exactly as it was, so it says nothing new about the reader.
      positionWritten(clip, followingBottom);
    } finally {
      mountingEarlier = false;
    }
  }

  // No compensation on this edge: rows appended BELOW the reading position
  // move nothing above it.
  function mountLater(): void {
    pane.activityRuns.setMountWindow(run.runId, activityRunWindowGrownNewer(run));
  }

  // Expanded payloads lift the cap by their own height (see
  // utils/activityRunClip.ts). Reading `mountedChildren` re-targets the
  // observers when the mounted set changes: a row can remount already
  // expanded from its lease, which mutates no attribute for the observer
  // inside to see. Streaming text growth changes neither, so this stays off
  // the hot path.
  $effect(() => {
    const clip = clipEl;
    if (!clip) {
      expandedPx = 0;
      return;
    }
    mountedChildren;
    return observeActivityRunExpansion(clip, (px) => {
      expandedPx = px;
    });
  });

  // Persisted per frame, not only at teardown: a thread switch clears the
  // registry synchronously with the data change, well before Svelte tears
  // this row down, so a teardown-only save would archive a position several
  // reads stale. One small write against a Map — the overlay scrollbar
  // already samples geometry on the same event.
  function saveInnerScroll(): void {
    const clip = clipEl;
    if (!clip) return;
    pane.activityRuns.saveScrollSnapshot(runId, {
      scrollTop: clip.scrollTop,
      // One fact — "the reader has left bottom-follow" — recorded from
      // whichever half of the run owns it. The live run's controller tracks it
      // as `escapedFromLock`; a run without one tracks it as the absence of
      // `followingBottom`. A run can change which half owns it (a live run is
      // displaced by a newer one), so the snapshot must not care which wrote it.
      escaped: stick ? stick.escapedFromLock : !followingBottom,
    });
  }

  // Whether activity is currently passing under the clip's upper edge, which
  // is the only state the top fade should paint in: a run resting at its first
  // row has nothing above to dissolve, and tinting that row would just make it
  // look dimmer than the rows below it. Sub-pixel slack because a fractional
  // row height can leave a scroller "at the top" reporting 0.4.
  let fadedTop = $state(false);
  const FADE_EPSILON_PX = 1;

  // Whether the clip should stay on the run's last row as content settles
  // under it — and, read the other way, whether the reader has stepped into
  // the run.
  //
  // A stored answer rather than a geometric one, and that is the whole point.
  // "Is it at the bottom right now" is the wrong question during a settle: the
  // clip is written to the bottom, the rows inside then grow, and the `scroll`
  // event from that write arrives AFTER the growth — reporting, correctly, a
  // position that is no longer at the bottom. Re-deriving from it dropped the
  // follow on the first row that resolved, which is why a run reopened by the
  // header's collapse-all landed near its top and stayed there. Growth moves
  // the bottom away from the reader; only the reader can decide to leave it.
  let followingBottom = $state(false);

  /**
   * Re-read what the clip's position looks like. Paint only — the fade is the
   * one thing that IS a pure function of the current geometry, so it is the
   * one thing read back from it after every move.
   */
  function syncPosition(clip: HTMLElement): ScrollMetrics {
    const metrics = readScrollMetrics(clip);
    fadedTop = metrics.scrollTop > FADE_EPSILON_PX;
    return metrics;
  }

  // Whether the reader is driving the clip's position right now.
  //
  // Arming the paging gate on a GESTURE rather than on the resulting geometry
  // is the same rule the scroll package states for the conversation: intent is
  // event-sourced, never inferred from where the surface ended up. Inferring
  // it here was wrong in the one case that matters — the mount write aims at
  // `scrollHeight`, but the rows inside are not measured yet, so it lands near
  // the top, and its own scroll event then read as "the reader scrolled to the
  // top of the window" and paged in a chunk nobody asked for. Whose
  // compensation then moved them there for real.
  let readerScrolling = false;

  function armReaderScroll(): void {
    readerScrolling = true;
  }

  /**
   * Arm on the gestures that scroll a clip: wheel, touch drag, and the keys a
   * focused row inside it responds to.
   *
   * An action rather than markup handlers because these observe a gesture
   * rather than answer one — the clip is a scroll surface, not a control, and
   * `onkeydown`/`ontouchmove` on a plain div would (correctly) ask for an ARIA
   * role it has no business claiming. All passive: nothing here can cancel the
   * scroll it is watching.
   */
  function readerGestures(el: HTMLElement) {
    const opts = { passive: true } as const;
    el.addEventListener('wheel', armReaderScroll, opts);
    el.addEventListener('touchmove', armReaderScroll, opts);
    el.addEventListener('keydown', armReaderScroll, opts);
    return {
      destroy() {
        el.removeEventListener('wheel', armReaderScroll);
        el.removeEventListener('touchmove', armReaderScroll);
        el.removeEventListener('keydown', armReaderScroll);
      },
    };
  }

  /**
   * Record a position THIS COMPONENT wrote, and whether the run should keep
   * chasing its tail from it.
   *
   * The arming drops because a write we made is not the reader scrolling — the
   * next gesture re-arms, so a run cannot page itself backwards through its own
   * history. `following` is stated by each write rather than measured, for the
   * reason `followingBottom` exists: at the instant of a write the rows inside
   * are routinely unmeasured, so the geometry cannot answer it yet.
   */
  function positionWritten(clip: HTMLElement, following: boolean): void {
    readerScrolling = false;
    followingBottom = following;
    syncPosition(clip);
  }

  // One handler for everything the clip's position drives. Cheap by
  // construction: three numeric compares and a Map write, and the `$state`
  // write only invalidates anything on the frame the answer changes.
  function onClipScroll(): void {
    const clip = clipEl;
    if (!clip) return;
    const metrics = syncPosition(clip);
    // The two reader-owned decisions, both behind the same gate. Leaving the
    // run's last row is a decision, so it is read from the geometry only when
    // the reader produced that geometry; and reaching the top of the window
    // pages the next chunk in, so browsing back through a long run is one
    // continuous scroll — the same contract the conversation's own load-older
    // gate offers. The boundary stays a button: it is the affordance for
    // jumping a chunk without scrolling for it, and the only one the "N later"
    // edge has.
    if (readerScrolling) {
      followingBottom = activityRunAtBottom(metrics);
    }
    // After the flag, so the snapshot archives this frame's answer rather than
    // the previous one's.
    saveInnerScroll();
    if (!readerScrolling) return;
    if (activityRunShouldMountEarlier(metrics, hiddenEarlier)) void mountEarlier();
  }

  // Controller lifetime and scroll-position persistence are ONE effect on
  // purpose. The saved snapshot carries the controller's escape flag, so
  // splitting them would make the saved value depend on which teardown
  // Svelte happened to run first.
  //
  // Inner position has to survive the virtualizer evicting this row:
  // without it a run the user had scrolled up inside snaps back to its
  // tail every time it leaves the buffer.
  $effect(() => {
    const clip = clipEl;
    const content = contentEl;
    if (!clip) return;

    const controller = isLive && content ? createStick() : null;
    stick = controller;
    if (controller && content) controller.attach(clip, content);

    const snapshot = pane.activityRuns.scrollSnapshot(runId);
    if (snapshot) {
      clip.scrollTop = snapshot.scrollTop;
    } else {
      // A run that has never been scrolled rests at its newest row — the
      // latest activity is the reason it is on screen.
      clip.scrollTop = clip.scrollHeight;
    }
    // The write above dispatches its `scroll` event asynchronously, so the
    // position is read back here rather than waiting for it: a run that mounts
    // already scrolled would otherwise paint one frame without its fade, and
    // the settle observer would spend that frame not knowing where it is.
    // A restored run follows its tail again only if the reader left it doing
    // that; a fresh one was just written to the bottom by definition.
    positionWritten(clip, snapshot ? !snapshot.escaped : true);

    // Escape is event-sourced, so it is carried into a new controller rather
    // than re-derived from the geometry just written. A pinned window is the
    // same fact recorded where a run without a controller could keep it: a
    // historical run a jump pinned has no `escapedFromLock` to save, so
    // becoming the live one would hand this fresh controller a clean flag and
    // the anchor effect below would release the pin the reader is standing on.
    const pinned = pane.activityRuns.windowAnchor(runId) !== null;
    if (controller && (snapshot?.escaped || pinned)) {
      controller.setEscapedFromLock(true);
    }

    return () => {
      pane.activityRuns.saveScrollSnapshot(runId, {
        scrollTop: clip.scrollTop,
        escaped: controller?.escapedFromLock ?? false,
      });
      controller?.detach();
      if (stick === controller) stick = null;
    };
  });

  // Holds a resting clip on its last row while its content settles.
  //
  // The position write above happens once, at the instant the clip mounts —
  // but the rows inside are not done at that instant. Payload bodies resolve,
  // highlight spans land, a row remounts already expanded from its lease and
  // lifts the cap. Every one of those grows the run AFTER the write, and
  // `scrollTop` does not follow on its own, so the reader is left partway up a
  // run they just opened. Visible immediately when several runs expand at
  // once (the header's collapse-all), because none of them is measured.
  //
  // Both boxes, because the two ways the gap opens are unrelated: the CONTENT
  // growing under a fixed clip, and the CLIP growing when cap inflation gives
  // it more room than its content needs.
  //
  // Only for a run with no controller. The live run's spring owns
  // bottom-following, with intent handling this cannot see; a second pinner
  // would fight it for the same pixels.
  //
  // This narrows the "a historical run does not chase when it grows"
  // tradeoff rather than reversing it: growth is followed only while the
  // reader is resting on the newest row, which is the one position where
  // following is what they are looking at. A run they scrolled inside still
  // never moves under them.
  $effect(() => {
    const clip = clipEl;
    const content = contentEl;
    if (!clip || !content || stick) return;
    const settle = new ResizeObserver(() => {
      if (!followingBottom) return;
      clip.scrollTop = clip.scrollHeight;
      // Our write, so it must not be mistaken for the reader arriving at the
      // top of a run whose content has not grown past the runway yet.
      readerScrolling = false;
    });
    settle.observe(clip);
    settle.observe(content);
    return () => settle.disconnect();
  });

  // Holds the reading position when the mount window's head ADVANCES, and
  // hands the follow back to the spring afterwards.
  //
  // A tail-following window starts at `children.length - rows`, so an appended
  // row drops one off the head in the same flush. That is not merely a
  // stationary-content problem, and it is why the run stopped gliding: the
  // clip's content shrinks by the dropped row and grows by the new one, so with
  // the two roughly the same height the TOTAL barely changes. The content
  // observer sees a delta near zero and reports nothing, no spring has anything
  // to chase, and the rows the reader is watching are displaced upward by a row
  // height in a single frame. A run glides through its first `mountedRows` rows
  // and starts teleporting from the next one on — which is exactly what a long
  // run of tool calls looked like, while thinking text growing 1→2→3 lines
  // (growth with no slide) kept gliding throughout.
  //
  // Two halves of one flush. `$effect.pre` sees the new window against the DOM
  // that still holds the old one, which is the only moment the departing rows
  // can be priced; the post-flush effect puts the anchor back and then states
  // the growth the observer could not see.
  //
  // ADVANCES only. A head that retreats is a chunk the reader paged in
  // (`mountEarlier`, which compensates its own prepend) or a jump relocating the
  // window (the focus effect, which places its own target) — compensating those
  // here would be a second write for one change.
  //
  // Declared BEFORE the focus effect so a jump wins: this holds a position the
  // reader did not ask to leave, and a jump is a position they did ask for.
  let headAdvance: { row: number; viewportTop: number } | null = null;
  let mountedHeadRow = -1;

  $effect.pre(() => {
    const row = mountedFrom;
    const previous = mountedHeadRow;
    mountedHeadRow = row;
    headAdvance = null;
    const clip = clipEl;
    if (!clip || previous < 0 || row <= previous) return;
    const viewportTop = activityRunRowViewportTop(clip, row);
    // The old and new windows do not overlap, so there is no shared row to
    // hold: a jump relocated this window wholesale and owns where it lands.
    // Also the path a remounting clip takes, whose own mount write positions it.
    if (viewportTop === null) return;
    headAdvance = { row, viewportTop };
  });

  $effect(() => {
    // The window, not the node: this must run on the same flush as the
    // measurement above and on no other.
    mountedFrom;
    const clip = clipEl;
    const advance = headAdvance;
    headAdvance = null;
    if (!clip || !advance) return;
    const target = activityRunScrollTopHoldingRow(clip, advance.row, advance.viewportTop);
    if (target === null) return;
    if (stick) {
      // Routed, not written. An untagged `scrollTop` write on the live run
      // reads as a reader gesture and escapes bottom-follow; `head-splice` is
      // the engine's own name for this change — content above the viewport
      // spliced out, anchor holds — and the resolver applies it verbatim.
      stick.applyEngineCompensation({
        kind: 'head-splice',
        delta: target - clip.scrollTop,
        target,
      });
      // The slide ate the runway the spring would have chased. After the
      // compensation the appended row sits below the clip's edge, but the
      // content's total height hardly moved, so the geometry observer has
      // nothing to report — and this is the seam for precisely that, geometry
      // the controller's own observers cannot see. The structural mark names
      // what changed, so the follow glides rather than snapping even once the
      // liveness stamp has lapsed (a completion pairing into a settled run).
      stick.markStructuralContentPending();
      stick.observe('live-content');
    } else {
      clip.scrollTop = target;
    }
    // Ours, so it is not the reader arriving anywhere; the follow state carries
    // through untouched, because holding the anchor says nothing new about them.
    positionWritten(clip, followingBottom);
  });

  // Jump resolution, inner half: a search hit, review jump, or tray row
  // whose item lives in this run has already relocated the mount window and
  // expanded the run (`revealActivityRunItem`) — what is left
  // is the scroll, which needs the mounted DOM this effect runs after.
  //
  // Declared AFTER the snapshot effect so it wins: a run mounting with both
  // a saved position and a pending jump has to land on the jump target.
  $effect(() => {
    const clip = clipEl;
    if (!clip) return;
    // The request is the dependency, not the node: a jump can target an item
    // the current window already holds, which changes nothing on the node.
    pane.activityRuns.revision;
    const request = pane.activityRuns.takeFocus(run.runId);
    if (!request) return;
    const row = activityRunRowIndexOfItem(run, request.itemId);
    // The item left the run between the request and this flush (a live-window
    // prune). Nothing to scroll to; the outer jump still put the run on
    // screen.
    if (row < run.mountedFrom || row >= run.mountedFrom + run.mountedRows) return;
    const el = activityRunChildElement(clip, row);
    if (!el) return;
    // Explicit navigation inside the run, so it escapes bottom-follow the
    // same way the outer timeline does — otherwise the next streamed chunk
    // would yank the reader off the item they jumped to.
    stick?.setEscapedFromLock(true);
    // A relocated window inherited an offset into rows that are no longer
    // mounted, so where the target sits under it is an accident — center it.
    // An unmoved window means the reader is already looking at these rows;
    // leave a target they can see where it is (utils/activityRunClip.ts).
    if (request.relocated || !activityRunRowFullyVisible(clip, el)) {
      clip.scrollTop = activityRunCenteredScrollTop(clip, el);
    }
    // The jump owns the position now, so the settle observer must not read the
    // run's next growth as licence to pull the reader off the target.
    positionWritten(clip, false);
  });

  // Whether the window follows the run's tail is a question about the READER,
  // so it is answered here, where the controller that tracks them lives.
  //
  // A tail-following window drops one head row for every row appended — an
  // implicit head trim. Under a reader who has scrolled up inside the clip
  // that is exactly wrong: the rows they are reading slide up by a row height
  // per append, and the one they were reading eventually unmounts from under
  // them. So while the controller is escaped the window pins to its own head
  // row, and new activity collects behind an "N later" boundary instead.
  //
  // Both directions are load-bearing. A pin left behind after the reader
  // returns to the bottom would strand a live run behind that boundary while
  // it kept streaming, so returning releases it and the run resumes
  // following. Declared AFTER the focus effect so a jump — which escapes
  // deliberately — has stated its intent before this reads it.
  //
  // Historical runs have no controller and no tail to follow; they never
  // slide, so there is nothing here for them to do.
  $effect(() => {
    // A controller exists only for a mounted clip, so its presence already
    // says the run has rows to pin — checking the collapse state on top of it
    // would now be wrong, since a collapsed run keeps its clip while live.
    const controller = stick;
    if (!controller) return;
    const head = run.children[run.mountedFrom];
    pane.activityRuns.setWindowAnchor(
      run.runId,
      controller.escapedFromLock && head ? timelineNodeItemId(head) : null,
    );
  });

  // No head TRIM, deliberately: the slice above is a window, not a high-water
  // mark, so a run that streams to 500 rows still mounts `mountedRows` of
  // them and growth cannot accumulate DOM on its own. Dropping a chunk the
  // user explicitly asked for would revert an explicit action — a short run
  // whose boundary is visible without scrolling would flash the rows in and
  // drop them in the same frame. What they asked for stays until the run
  // unmounts.
</script>

<div
  class="relative"
  data-testid="activity-run"
  data-rail="true"
  data-run-id={run.runId}
  data-collapsed={collapsed ? 'true' : 'false'}
  data-live={isLive ? 'true' : 'false'}
>
  <!-- Always, in both states. The run's summary is also its visible collapse
       control, so expanding cannot remove the thing the reader just clicked;
       and because the header never moves, a run collapsing off-screen changes
       nothing the engine's anchoring has to guess about.

       pl-[8.5px] centres the 12px chevron on the rail border below (drawn at
       14px + 1px wide): the header sits OUTSIDE the rail, its chevron marking
       the line's x, so collapsing reads as the rail folding up into its
       control. Padding rather than margin — the header is w-full, and a
       margin would push it past the row's box. -->
  <ActivityRunHeader
    {pane}
    {run}
    {clipId}
    expanded={!collapsed}
    onToggle={toggle}
    class="pl-[8.5px]"
  />
  {#if !collapsed}
    <!-- Rail offsets match what the per-row wrapper used to apply:
         ml-[14px] places the border 14px into the row column (under the
         chevron gutter) and pl-[18px] shifts the rows past the icon and
         label gutters. Applying them once here rather than per row is what
         makes the border one continuous line instead of N abutting ones —
         and wrapping only the clip is what keeps the header from sitting
         behind a second edge. -->
    <div class="relative ml-[14px] border-l border-border-subtle pl-[18px]">
      <!-- A second collapse target on the rail: a hit strip straddling the
           border, in the gutter where no content sits. Absolutely positioned so
           it consumes no width and cannot shift the row; it folds with the clip,
           because a collapsed run has no edge left to click — the header is the
           whole run then.

           Pointer-only, deliberately. It duplicates the header above, and an
           invisible duplicate is worth having for the mouse (the block of rows
           reads as one thing, so clicking its edge should fold it) and worth
           hiding from everything else: a keyboard user would land a focus ring
           on a transparent 16px strip, and a screen reader would hear the run's
           state announced twice from two buttons naming one region.
           `aria-hidden` with `tabindex="-1"` rather than either alone — hiding
           a focusable element is its own defect. The header is the accessible
           control, and it is always there. -->
      <button
        type="button"
        class="absolute inset-y-0 -left-2 w-4 cursor-pointer bg-transparent"
        onclick={toggle}
        tabindex="-1"
        aria-hidden="true"
        data-testid="activity-run-rail"
      ></button>

      <!-- Clip host. Exists so the overlay bar has a box exactly the clip's
           height to hang beside: measured against the run instead, it would span
           the header too and put the thumb at the wrong offset for every collapsed
           live run. -->
      <div class="relative">
        <!-- Fade clip. The top fade below is a 24px overlay strip; on a run
             shorter than that it would spill past the run's own bottom edge, so
             this box clips it to the visible height. `overflow-hidden` is
             unconditional: the clip inside is already a block formatting
             context, so it changes no margin. -->
        <div class="relative overflow-hidden">
          <!-- overflow-x is hidden on purpose: a wide preview inside a tool row
               must not raise a horizontal bar at run level, which would consume
               HEIGHT and shift every row below. overscroll-behavior stays auto —
               chaining out at the inner edge is wanted, and gesture correctness
               comes from attribution (utils/scroll/wheelAttribution.ts), not
               from blocking the chain. -->
          <div
            bind:this={clipEl}
            id={clipId}
            class="activity-run-clip overflow-y-auto overflow-x-hidden [overflow-anchor:none]"
            style:max-height={maxHeight}
            use:nestedScroll
            use:readerGestures
            onscroll={onClipScroll}
            data-testid="activity-run-clip"
          >
            <!-- Static will-change-transform: composited for the inner
                 controller's sub-pixel glide residue, same contract as
                 the pane's contentEl (see MessageTimeline). Deliberately
                 unconditional even though only the live run gets a
                 controller: liveness can land on an already-mounted run
                 (see the escape-flag comment in the controller effect),
                 and a class that appears or disappears on a mounted
                 element is a raster transition — the flicker class the
                 static hint exists to rule out. Cost: one composited
                 layer per mounted expanded run, live or not. -->
            <div bind:this={contentEl} class="will-change-transform">
              {#if hiddenEarlier > 0}
                <ActivityRunBoundary count={hiddenEarlier} edge="earlier" onclick={mountEarlier} />
              {/if}
              <!-- The wrapper carries the row's index because that is the only
                   handle a jump has on a non-leaf row: only leaves emit
                   `data-item-id`, and a hit inside a subagent card resolves to
                   the card. A plain div, so row margins keep collapsing exactly
                   as they did when these rows were the virtualizer's own. -->
              {#each mountedChildren as child, i (timelineNodeKey(child))}
                <div data-run-child={mountedFrom + i}>
                  {@render renderNode(child, depth)}
                </div>
              {/each}
              {#if hiddenLater > 0}
                <ActivityRunBoundary count={hiddenLater} edge="later" onclick={mountLater} />
              {/if}
            </div>
          </div>
          <!-- Top fade, the run's own copy of the conversation's: rows dissolve
               as they rise out of the clip instead of being cut by a hard edge.
               Same gradient OVERLAY technique for the same reason — a mask on the
               clip rasterizes a full clip-sized texture on every streaming
               repaint, while this is a strip that paints once (see
               MessageTimeline's TOP_FADE_PX). Paint-only either way: no effect on
               scrollHeight/clientHeight/scrollTop and no content-RO traffic, so
               it stays clear of the controller.

               Shorter than the conversation's 32px — a run shows a handful of
               rows, and a third of one is enough to read as a dissolve without
               dimming the row behind it. It needs no scrollbar-safe inset:
               `right-0` is the clip's own right edge, and the overlay bar hangs
               outside it. -->
          <div
            aria-hidden="true"
            class="pointer-events-none absolute top-0 right-0 left-0 h-6 transition-opacity duration-150"
            class:opacity-0={!fadedTop}
            style:background="linear-gradient(to bottom, var(--surface-0), transparent)"
            data-testid="activity-run-top-fade"
            data-faded={fadedTop ? 'true' : 'false'}
          ></div>
        </div>
        <!-- A zero-width native bar makes the scroll package's geometric
             scrollbar-gutter hit test impossible, so a drag states its intent
             instead of having it inferred.
             The handlers are unconditional: a bar drag is the reader scrolling
             whether or not this run holds the live tail, and the controller half
             is what is optional (`stick?.`). Handing `undefined` for a historical
             run made a drag the one gesture that armed nothing, so dragging to
             the top of a run's window paged nothing in. -->
        <OverlayScrollbar
          target={clipEl}
          content={contentEl}
          ariaLabel="Scroll activity run"
          ownerDrivenPosition={() => !!stick && !stick.escapedFromLock}
          onUserScrollStart={() => {
            armReaderScroll();
            stick?.setEscapedFromLock(true);
          }}
          onUserScrollEnd={(atBottom) => {
            if (atBottom) stick?.markAtBottom();
          }}
        />
      </div>
    </div>
  {/if}
</div>
