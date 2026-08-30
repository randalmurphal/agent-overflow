# Settle-moment scroll flicker: root-cause analysis

> ## 🟢 2026-07-01 (later): the STREAMING settle flicker fully root-caused from capture `bug-report-20260701T201655Z.jsonl`; fixes applied (virtua manual-scroll patch + fractional height caching; working tree, pending in-app confirmation). Closes the "streaming stutter is a separate, still-open strand" left by the idle-vibration entry below.
>
> **Confirmed mechanism, every link evidence-backed.**
>
> 1. Every streaming beat, the stick controller writes `scrollEl.scrollTop`
>    directly (`useStickToBottom.svelte.ts` `writeProgrammaticScrollTop`, the
>    single chokepoint): sync pins, spring ticks, forceStick.
> 2. **virtua@0.49.1 classifies those writes as USER scroll-downs.** The store's
>    `$update(ACTION_SCROLL)` latches a scroll direction unless its internal
>    manual flag was set first, and only virtua's own `scrollTo`/`scrollBy`/
>    `scrollToIndex` set that flag: all async convergence loops, unusable for
>    sync per-beat pins. While the latched direction is "down",
>    `$getRange(bufferSize)` drops the entire above-viewport overscan:
>    ~39 rows (1800px buffer) unmount in a burst. Visible rows survive; only
>    the buffer band cycles.
> 3. Pin writes / spring ticks (~73ms cadence) keep resetting virtua's 150ms
>    scrollend debounce, so the drop persists 156–811ms per beat; at scrollend
>    the ~39 rows remount in ONE flush (capture: destroyed keys == remounted
>    keys, 38/38).
> 4. Each remounting row re-arms the cold-mount `min-height` floor from the
>    row-height cache, whose heights were `Math.round`ed at two points (the
>    reservation's RO measure entry in `timelineRowGeometry.ts` and
>    `normalizeTimelineRowHeight` in `threadRowUiState.svelte.ts`). The
>    round-up residues sum to **+12–13px of totalSize for one frame**; the
>    floors release as the real RO measures each row → −12–13px → scrollTop
>    clamp-back = the visible twitch. All 5 blips in the capture are ±12–13px.
> 5. The pre-floor build had identical churn but no floors. It was invisible. The
>    floors are the *amplifier*; the misclassified-write buffer churn is the
>    *disease*.
>
> **Evidence chain:** `timeline.row.geometry` trace instrumentation (action
> counts; mass destroy→gap→mount bursts; triggers = composer ±20 resizes,
> revision bumps, row resize) → virtua core/adapter source analysis → real-
> Chromium spike bisection (pin-only write reproduces the destroy burst;
> growth-only is clean) → direct `createVirtualStore` drive (unmarked down
> write: range [135,199]→[174,199]; manual-marked write: stays [135,199]).
>
> **Note on the older "Virtua remount: REFUTED" section far below:** that
> refuted a full-`Virtualizer` remount (`listRef` rebind). That is still true; the
> capture shows zero rebinds. Buffer-row unmount/remount *inside* a mounted
> Virtualizer is a different mechanism and is the confirmed disease.
>
> **Fixes (working tree).**
>
> - **Root:** `patches/virtua@0.49.1.patch` exposes
>   `markProgrammaticScroll()` on the Svelte `VirtualizerHandle`, the same
>   internal ACTION_MANUAL_SCROLL marking virtua's own scroll APIs use,
>   callable synchronously. `useStickToBottom` gained an
>   `onBeforeScrollTopWrite` option invoked before EVERY programmatic write
>   (virtua clears the mark at scrollend, hence per-write); MessageTimeline
>   wires it to the handle. Marked writes: no direction latch → no buffer
>   drop → no remount churn. Side benefit: virtua no longer flips
>   `pointer-events: none` on the container during streaming pins.
> - **Amplifier:** the row-height cache stores exact fractional heights
>   (`Math.round` removed from the RO measure entry and
>   `normalizeTimelineRowHeight`; width keys stay integer). Any remaining
>   remount path re-floors with zero residue.
>
> **Tests:** `src/test/integration/virtua-patch-buffer-retention.browser.test.ts`
> (real Chromium: unmarked pin write drops the buffer, the patch's drop-rule
> tripwire; marked write keeps it mounted, the fail-without/pass-with for the
> fix); `useStickToBottom.svelte.test.ts` "onBeforeScrollTopWrite fires
> before every programmatic scrollTop write";
> `messageTimelineVirtuaMarking.test.ts` (the component wiring seam:
> a controller pin write must reach the bound handle's
> `markProgrammaticScroll`); fractional round-trips in
> `timelineRowGeometry.test.ts` + `threadRowUiState.svelte.test.ts`.
>
> **2026-07-01 (addendum): the floors (the historical amplifier) are now
> DELETED.** With the root fix above in place, a capture experiment on the
> only remaining candidate scenario (scroll-away/return remounts) showed the
> floor system doing zero real work: floors-OFF was outcome-identical to
> floors-ON on the stock build, and every apparent "hold" was a sub-pixel
> fractional-compare artifact. The reservation state machine
> (`timelineRowGeometry.ts`), the row-height cache in
> `threadRowUiState.svelte.ts`, and the `timeline.row.geometry` trace taps
> are gone; `remountReturn.browser.test.ts` pins the remount-path outcomes.
> Margin containment (`[data-row-geometry-content]` flow-root) and the
> width-observer single-source invariant (`scrollSurfaceWidth.ts`) survive
> the deletion. See
> [`scroll-rearchitecture-plan.md`](scroll-rearchitecture-plan.md) §3.

---

> ## 🟢 2026-07-01: idle full-viewport VIBRATION root-caused from a real capture; fix applied (`IDLE_REPIN_DEADBAND_PX`, working tree, pending in-app confirmation). Supersedes the entire `MAX_STREAM_DETACH_PX` / `SETTLE_IDLE_MS` narrative below.
>
> **Scope.** This entry covers the idle **full-screen vibration** ("the entire
> viewport just vibrates / quick flickers on-off, and as more items come in it
> can go away"). It is a *different* defect from the streaming settle-flicker the
> rest of this doc chases, and it has a clean, confirmed, idle-scoped fix. The
> stashed controller apparatus the 2026-06-30 note describes as live
> (`MAX_STREAM_DETACH_PX`, the `SETTLE_IDLE_MS` settled-idle pin,
> `committedBottomTarget`, `contentRO.settledIdlePin`, `springDetachClamp`) was
> **reverted**. The shipped controller is HEAD + only the deadband below.
>
> **Confirmed mechanism (capture `bug-report-20260701T012813Z.jsonl`, 19 256
> events; WSLg, fractional DPR).** At idle, with the spring settled
> (`springToken === 0`), pinned at the bottom, `animationMode ≠ 'spring'` so
> `springGateOpen()` is false and the positive-delta path takes the direct
> sync-pin else-branch, the content box height flips ±2px every ResizeObserver
> delivery:
> ```
> cRO d=+2 prev=16904 next=16905 sH=17110 tgt=15782 → WRITE contentRO.positiveDelta req=15782
> scrEv sTop=15780 resizeDiff=2
> cRO d=-2 prev=16905 next=16904 sH=17109 tgt=15781 → WRITE contentRO.negativeDelta req=15781
> … repeats ~every 170ms, forever, until a new item moves total height off the X.5 boundary …
> ```
> `entry.contentRect.height` is fractional and lands on an X.5 boundary under a
> fractional device-pixel-ratio; the sub-pixel total flips ±~1.8px, so the bottom
> target (`scrollHeight − clientHeight`) flips 15781↔15782. There is **no noise
> floor on height**: only an exact `delta === 0` is filtered (line ~1500),
> whereas width has `CONTENT_REFLOW_WIDTH_EPSILON_PX = 0.5`. So `positiveDelta`
> (1456×) and `negativeDelta` (1416×) re-pin `scrollTop` on every wobble frame;
> each write perturbs fractional layout, the RO re-fires the opposite delta, and
> the cycle sustains itself. It "goes away as items come in" because real growth
> moves total height off the X.5 boundary.
>
> **Why the stashed deadband leaked.** The stash added a *parallel*
> `settledIdlePin` path (127 writes in the same capture) but never gated the
> actual `positiveDelta` / `negativeDelta` writers (2 872 writes). The vibration
> routed straight past it.
>
> **Fix (applied to the working tree: HEAD + this only; NOT yet committed,
> pending in-app confirmation, exactly like every sibling fix in this doc).**
> `useStickToBottom.svelte.ts`: gate the
> real writers. `idlePinWithinDeadband = springToken === 0 && |scrollTop − target|
> ≤ IDLE_REPIN_DEADBAND_PX (4)`, folded into both `positiveWillPin` and
> `negativeWillPin`. Keyed on **distance-from-target**, not delta magnitude.
> Real growth moves the target ≥ a line height (gap ≫ 4) and pins normally; only
> the ≤4px fractional wobble (idle-flip pins sit at gap ≤2 in >99% of the capture)
> is suppressed. Gated on `springToken === 0` so it is **idle-scoped by
> construction**: during streaming the spring holds its token across inter-chunk
> gaps (arrival keeps it sentinel-alive while `animationMode === 'spring'`), so
> this never trips mid-chase. Unit coverage is one true fail-without/pass-with
> regression test plus one upper-bound guard, in `useStickToBottom.svelte.test.ts`:
> "stops re-pinning an idle sub-pixel oscillation at the bottom" (±2px × 12 cycles
> ⇒ ≤2 writes; **24 without the gate**, the regression) and "still pins line-sized
> growth beyond the deadband" (+24px ⇒ pins to the new bottom with **and** without
> the gate, a guard that the deadband stays sub-line, not a fail-without test).
> NOTE: unit tests here run in happy-dom and cannot render the perceptual
> vibration; per this doc's own `MAX_STREAM_DETACH_PX` lesson, in-app confirmation
> is still owed before this is trusted.
>
> **The streaming stutter/lag is a SEPARATE, still-open strand.** The same
> capture's active-streaming segments (3 516 `spring.tick` writes across ~10
> chases) are **clean**: `maxTarget` non-monotonic decreases 0/373, `scrollHeight`
> decreases 0/373, backward `scrollTop` motion 0/3 274, width static
> (`widthChanged` 2/5 117). The ±2px flip and the width-reflow strand are both
> **absent while content is growing**. They need growth to stop. So this idle fix
> does **not** address the mid-stream stutter (`springToken ≠ 0`); that needs a
> capture taken *during* active streaming while the stutter is felt. Do not
> conflate the two.
>
> Everything below (the 2026-06-30 note and older) is retained for its DATA and as
> the record of the reverted bound/settled-idle-pin approaches. The
> `MAX_STREAM_DETACH_PX` / `SETTLE_IDLE_MS` machinery it describes is **not in the
> shipped controller**.

---

> ## 🔴 2026-06-30: the `MAX_STREAM_DETACH_PX` bound was IMPLEMENTED, tested in-app, and REVERTED; replaced by the settled-idle onset-detach pin (`SETTLE_IDLE_MS`), pending in-app confirmation. Why the bound was wrong, and what replaced it.
>
> In-app result (user, streaming a real turn): "no spring scroll at all anymore,
> just jumping back and forth … if something comes in during a spring scroll it
> instantly snaps … never able to spring scroll." The bound made streaming
> **significantly worse**. It is reverted (the contentRO positive-delta branch is
> back to plain `startSpringIfNeeded()`).
>
> **The data below (spring-lag detachment, the 0→33px onset, the up-to-85px
> mid-stream detaches) is CORRECT. The conclusion it drew ("so the fix is a
> bound") is WRONG.** The error: it measured `distanceFromBottom` (a magnitude)
> and treated every detached frame as the same defect, then bounded them all. But:
>
> - **The visible settle-flicker and ordinary mid-stream growth are the SAME
>   controller event.** A positive-delta chunk lands while the spring is at/near
>   rest; the spring tick runs on rAF, which fires BEFORE the contentRO callback
>   within a frame, so the catch-up lands one frame late and the onset paints
>   detached by ~the chunk. This happens on *every* chunk, settle or not.
> - **They differ only PERCEPTUALLY, and the controller cannot see the
>   difference.** At settle the screen is still (recId `…842131`: `dfb=0` held for
>   ~6 s, then a +38px chunk → `dfb=33` → eases, a visible flicker). Mid-stream
>   the screen is in continuous motion (recId `…875401`: `scrollTop` advancing
>   +20–30px/frame with sustained `dfb` 20–85, which the user calls **smooth**).
>   Same onset detach; motion masks it in one case and not the other.
> - **Therefore magnitude is NOT the discriminator**: an 85px mid-stream detach
>   reads as smooth, an 18px settle detach reads as a flicker. A bound on detach
>   magnitude / spring velocity / `springToken` fires on BOTH (they are the same
>   state at chunk-time) and snaps the smooth mid-stream follow. That is precisely
>   what the in-app test showed. `scrollTop` is strictly monotonic in both
>   recordings (no stale write), so there is nothing to "correct". Only motion
>   context differs.
>
> **The real discriminator is temporal, and it IS observable:** "was the view
> settled-and-idle (no scroll motion for the last ~K frames) when this chunk
> landed?", not the spring's instantaneous kinematics. At a true settle the
> controller has not moved `scrollTop` for many frames; mid-stream it is moving it
> every frame. The fix must key on that. Likely shape: when a positive-delta chunk
> lands while the view is settled-idle-at-bottom, sync-pin to the new bottom
> (instant-mode behaviour: no detach frame, imperceptible because the screen was
> already still at the bottom); when the spring is actively moving, leave it to
> spring (motion masks the onset).
>
> **IMPLEMENTED (2026-06-30) as the settled-idle onset-detach pin.** A motion
> clock (`lastScrollMotionAt`, stamped in `writeProgrammaticScrollTop` only when a
> write actually moves `scrollTop`) measures how long the rendered position has
> been still. In the contentRO positive-delta branch, a committed growth landing
> after `≥ SETTLE_IDLE_MS` (50ms ≈ 3 frames) of stillness pins straight to the new
> bottom (`contentRO.settledIdlePin`); otherwise it hands off to the spring as
> before. Because the pin itself counts as motion, a fast burst pins only its
> first chunk and the next chunk re-engages the spring, so the glide is preserved
> for continuous streaming. The pin can never snap a moving spring: it fires only
> after the position has been still ≥ K frames, i.e. the spring is parked, so
> there is nothing mid-glide to interrupt. This keys on the OBSERVABLE (rendered
> stillness), which is exactly what the reverted bound's kinematic scope
> (velocity / `springToken`) could not see.
>
> **Tradeoff being validated in-app:** this reverses the deliberate "spring from
> rest on positive delta when warm AND mode=spring" design (test 2885) for the
> settled case: the settled / isolated / tail chunk instant-pins (no glide)
> instead of springing. That is the price of killing the flicker: the from-rest
> spring's first frame IS the flicker, so the only way to keep the glide on that
> chunk is to keep the flicker. Confirm the tradeoff is acceptable, and that the
> flicker is gone for BOTH the idle-then-chunk case AND the short-gap streaming
> tail (where the K-frame detector is fragile, so tune `SETTLE_IDLE_MS`).
> `MAX_STREAM_DETACH_PX` and the bound-era tests are dead, removed once confirmed.
>
> **Why none of the test/review machinery caught it:** the unit tests modelled a
> single chunk landing in a hand-constructed sentinel-alive state and asserted the
> bound *fired*; one test ("holds the bound at every onset across a continuous
> multi-chunk stream") asserted snap-on-every-chunk as *correct*. It encoded the
> bug as a passing test. The `stubGeometry` mock cannot render perceptual
> smoothness, so "snapping looks bad" is invisible to it. The only check that
> could catch it, driving real streaming in-app, was gated to the end and not
> run until the user ran it. Lesson: a fix premised on a perceptual claim must be
> validated perceptually before the unit-test scaffold is built on it.
>
> Everything below is retained for its DATA (accurate) and as the record of the
> rejected "bound" approach. Do not re-implement the bound without resolving the
> perceptual-discriminator problem above.

---

# Settle-moment scroll flicker: root-cause analysis

> ## ⚠️ RESIDUAL after the flow-root fix: REAL driver is spring-lag detachment (capture `195825Z`, thinking block)
>
> The margin fix below (A) and the width-strand fix (a5a5d032) both **hold** in
> capture `bug-report-20260630T195825Z.jsonl`: **zero `timeline.margin.diverge`**,
> **zero** width-reflow `contentRO`. Yet the thinking-block settle flicker
> persists. **An earlier read of this capture blamed the external-write gate
> dropping virtua's above-viewport `$fixScrollJump` in spring mode. That is
> WRONG and is superseded by the `recFrame`/60Hz data below.** The suppressed
> jumps are real but tiny (±8px sub-pixel jitter) and are NOT the visible motion.
>
> **Data-locked mechanism (`user.bugReportRecFrame` per-frame state + 60Hz
> `scroll.write`):** the visible motion is the **spring easing lagging
> bottom-pinned content growth**, measured directly by `distanceFromBottom`
> (`scrollHeight − clientHeight − scrollTop`):
> - At each prose/think chunk the bottom **detaches**: e.g. recId `…842131`
>   t=18405→18506, `scrollHeight +38`, `scrollTop +5` (spring-eased) ⇒
>   **`distanceFromBottom` 0→33 in one frame**, then eases 33→16→8→3→0 over ~4
>   recFrames (~390ms). 60Hz-confirmed at the write level: `contentRO Δ=+56 then
>   −18` (net +38 markdown re-render), first `spring.tick` already at distBot=34,
>   easing 34→22→13→7→4→2→0. The **sharp onset** (content grows instantly, spring
>   slow-starts from rest at stiffness 0.08 ⇒ ~6% of the gap closed on frame 1) is
>   the "goes down ½–1 line"; the ease-back is "presents properly".
> - Across **both** recordings, **44/160 frames** have `distanceFromBottom > 0`,
>   up to **85px** (recId `…875401`, continuous streaming, where the spring never
>   catches the bottom before the next chunk). Magnitudes 1–85px (median ≈ ½–1
>   line), matching the user's estimate.
> - **No shrink** anywhere (`scrollHeight` monotonic except one trivial −4), so
>   the documented shrink→native-clamp→`spring.oscillationSnap` chain (body below)
>   is **NOT** firing. `oscillationSnap` does not appear in the capture at all.
> - **No real write ever decreases scrollTop** (every `scroll.write` caller has
>   `maxΔ(scroll−)=0`). The "down" is not a downward write. It is scrollTop
>   **failing to keep up** with upward growth.
>
> **Discriminator (matches think/prose-flicker, tool/bash-don't exactly):**
> `animationModeForScroll` (`MessageTimeline.svelte:436` → `latchedSpringMode`)
> returns `'spring'` while live prose/reasoning advanced within
> `SPRING_MODE_HOLD_MS`, else `'instant'`. **Tool rows deliberately do not stamp
> `lastLiveContentAt`** → instant → the contentRO branch sync-pins
> `scrollTop=bottom` every delta → `distanceFromBottom` stays 0 → no detach.
> Prose/think → spring → eases → detaches.
>
> **Why the spring exists (the tension):** it chases the deadband-stable
> *committed* bottom (not raw), so it **absorbs the +56/−18 markdown re-render
> oscillation** and smooths big deltas that would look jerky if hard-pinned. Its
> unavoidable cost is transient lag = the detach. Smoothing a step requires
> lagging it; you cannot have both zero-lag and glide for a big one-frame content
> jump. So the fix is a **bound**, not removal of the spring.
>
> **Fix layer:** the spring/contentRO in `useStickToBottom`: **bound the maximum
> at-bottom streaming detachment** so `distanceFromBottom` during an at-bottom
> spring chase can never exceed ~1 line (clamp scrollTop up to
> `target − MAX_STREAM_DETACH_PX` when the eased position would detach further),
> and/or kill the slow-start by seeding first-tick velocity on a chase that
> resumes from rest. Pending advisor review + in-app retest before any edit.
>
> **IMPLEMENTED + regression-verified (pending in-app retest):** synchronous
> pre-paint pin in the contentRO positive-delta branch, scoped to an
> **IN-FLIGHT streaming-spring SLOW-START**: `animationMode === 'spring' &&
> springToken !== 0 && |velocity| < ARRIVAL_VELOCITY_THRESHOLD`.
> `MAX_STREAM_DETACH_PX = 8`. rAF fires before RO within a frame, so only a
> synchronous RO-callback correction (not stiffer constants / seeded velocity)
> can kill the onset frame.
>
> **SCOPE CORRECTION 1 (the three-way record, kept whole so it is not
> re-thrashed):** the scope went through three forms; only the third is right.
>
> 1. `springToken === 0` (chase from a full cancel). **WRONG.** The `195825Z`
>    trace REFUTES it: across the continuous-streaming burst `springToken` was a
>    **constant 2** (sentinel-alive, never cancelled), and every flicker onset was
>    a `spring.arrive`-zeroed slow-start with `springToken !== 0`, so this scope
>    would have bounded **none** of the real flicker (it only ever catches the
>    first chunk after a >500ms idle, which the thinking-block capture happened to
>    be, a vacuous pass).
> 2. `|velocity| < ARRIVAL_VELOCITY_THRESHOLD` alone. **OVER-REACHES.** Velocity
>    is the right *kinematic* test for "will slow-start", but from-rest and
>    sentinel-alive are kinematically identical at the onset frame (velocity 0),
>    so velocity-alone ALSO snaps the turn's **first** from-rest chase, the smooth
>    follow the user explicitly called fine, with zero flicker evidence (both the
>    trace and the user locate the flicker at the post-settle "settle", not at the
>    onset of streaming). Empirically it broke ~7 pre-existing from-rest design
>    tests (e.g. `'engages on positive delta when warm AND mode=spring'`,
>    `'springs same-width live growth after the width-reflow settle window'`).
> 3. `springToken !== 0 && |velocity| < ARRIVAL_VELOCITY_THRESHOLD`. **RIGHT.** An
>    in-flight spring that already caught up to a settled bottom and is now
>    slow-starting AGAINST it: the disturbance-after-stillness the user sees
>    flicker. `springToken` is the only signal separating from-rest (leading edge
>    of sustained follow: keep smooth) from sentinel-alive (re-detach after
>    settle: bound). Adding the `springToken !== 0` clause dropped the suite from 12 → 5
>    failures and **self-healed** the ~7 from-rest design tests with no edits.
>
> Two regression tests lock the discriminator: `'caps the onset detach while
> sentinel-alive …'` (state `springToken !== 0`, velocity≈0, **verified to FAIL**
> under a `springToken === 0` scope and with the bound removed) and
> `'engages the spring from rest …'` (state `springToken === 0`, **verified to
> FAIL** if the scope drops the `springToken !== 0` clause back to velocity-only).
> A from-rest-only test passes under all three scopes and cannot protect the fix;
> the pair is needed.
>
> **The one open assumption (in-app retest gate):** the from-rest exemption rests
> on "the turn's first chunk doesn't flicker." Plausible, unproven. The retest
> MUST include the first chunk of a turn and a short isolated turn (one quick
> burst then silence). If those flicker, the exemption is wrong and the fallback
> is velocity-only: delete the single `&& springToken !== 0` clause.
>
> **SCOPE CORRECTION 2 (`!structuralGlide` dropped: structural mounts ARE the
> worst flicker):** an earlier draft ALSO excluded structural-append glides
> (`springStartedFromStructuralAppend || structuralAppendSpringUntil > now`),
> reasoning a deliberate jump-to-new-content should not be snapped. The `195825Z`
> trace REFUTES that too: the **largest** onsets in the burst were
> `+ESTIMATED_ROW_SIZE` (56px) new-row mounts (`structuralAppendSpringPending =
> true`) in **spring** mode. An in-turn row appearing while text streams is the
> *worst* flicker, not an exception to leave un-bounded.
> `activeTurnStructuralSignature` (`MessageTimeline.svelte:354`) keys on tail-row
> **identity**, so those marks are genuine mounts, not content deltas. The
> exclusion was therefore dropped. The ONLY structural case still excluded is the
> deliberate **instant-mode** glide (`markStructuralContentPending` lets a spring
> run under `animationMode === 'instant'` to animate a new command row in), and
> that is excluded for free by the `animationMode === 'spring'` guard, no
> `structuralGlide` term needed. In-app retest must drive **continuous prose**
> (the sentinel-alive path) AND a turn where rows append mid-stream (tool calls
> appearing while text streams).
>
> **Why bounding the structural `+estimate` is safe (estimate→correct
> auto-clamp):** when a new row mounts, virtua paints `+ESTIMATED_ROW_SIZE` then
> corrects to the measured height a beat later. The pin bounds the `+estimate`
> onset to `committed − 8`; in production the `−correction` (a `scrollHeight`
> shrink) auto-clamps `scrollTop` from the bounded estimate down to the real
> bottom in the **same pre-paint frame**. The browser clamps scrollTop the
> instant `scrollHeight` shrinks (the premise of the sentinel-oscillation block at
> `useStickToBottom…:1751`), so real post-correction overshoot ≈ 0. Instant mode
> already sync-pins to the same provisional estimate for tool rows and they are
> flicker-free in production. The synchronous-pin-then-correct path is proven
> smooth. `ESTIMATED_ROW_SIZE = 56` (`MessageTimeline.svelte:88`) is itself under
> `SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX` for a ≥1-line row (`56 − 16 = 40 <
> 50`), so even absent the auto-clamp the symmetric spring absorbs the correction
> without a snap. The unit mock's stub scrollTop setter re-clamps only on
> *write*, not when `geom.scrollHeight` is assigned, so **has-a-correction tests
> model the auto-clamp explicitly** (the mock now re-clamps scrollTop on a
> scrollHeight shrink) while **pure-growth tests assert the bounded sync-pin
> directly**; the one residual the source cannot settle, whether the estimate
> paint and its correction coalesce into the *same* frame, is what instant-mode
> tool rows already answer affirmatively, and the in-app retest is the final
> arbiter.

---

Status: **ROOT CAUSE CONFIRMED · FIX APPLIED** (pending in-app probe retest):
capture `183539Z` (live `timeline.margin.diverge` probe) + source-verified +
mechanism reproduced and fix verified in real Chromium (see **Fix** below). The
flicker is a **row-level bottom margin that
collapses *out* of `[data-row-index]`** and is **trapped by virtua's
`contain: layout style` item wrapper**: virtua re-measures the trapped margin
while it reflows around a growing streaming row, so the margin oscillates in
virtua's measured total (driving `contentRO` and the `spring.oscillationSnap`)
while the row's own content-box never changes, so `row.resize` stays silent.

Confirmed culprits, each snap-coincident with `diverge == the margin` and
matched to source:
- **error card `mb-4` = 16px** (`TimelineLeaf.svelte:102`): `error:*` rows.
- **user message `.group mb-5` = 20px** (`UserMessage.svelte:141`): `user:*` rows.

**Severity = stacking.** When several margin-bearing rows re-measure in the
same frame their collapse-out margins add: `16 + 20 → the +32/+33 snaps`
(observed at `183539Z` −3.50s/−2.33s, error row#215 *and* user row#216 firing
together); a lone row gives `+12/+13`. That is exactly the user's "mild always /
severe once the DOM fills" split: more margin-bearing rows accumulated near the
streaming tail ⇒ larger combined transient ⇒ severe flicker.

**NOT prose markdown.** The committed/volatile `:has()` seam was refuted (23px
*internal* margin → would hit `row.resize`; observed transient is silent on
`row.resize`). Assistant prose rows end in a `mb-0` footer and carry no
collapse-out bottom margin; the `text:*` divergences seen are small (~6px) and a
separate, minor residual. Superseded leads, dead by measurement: virtua-remount
driver; "spring at ~14fps / WSLg-environmental" (`SPRING_TICK_TRACE_SAMPLE=12` ×
165Hz sampling artifact); the `:has()` seam. The older sections below document
the (still-correct) clamp/snap *chain*; only the *source* was wrong.

Open (trigger detail, not blocking the fix): the precise reason virtua
re-measures a *static* upstream row's trapped margin inconsistently mid-reflow
(RO delivery race vs. layout). The fix doesn't depend on it. Eliminating the
collapse-*out* so virtua and `row.resize` measure each row identically removes
the divergence regardless of the trigger.

## Fix (applied): contain the collapse-out margin at the row wrapper

`[data-row-geometry-content]` (the per-row measurement anchor in
`MessageTimeline.svelte`) now sets **`display: flow-root`**, establishing a
block formatting context at the row level. Each row's trailing bottom margin
(error `mb-4`, user `.group mb-5`, notification/retry `mb-1.5`, api-error
`mb-3`) is now **contained inside the row's content box** instead of collapsing
out through the all-plain wrapper chain to be trapped by virtua's
`contain: layout` item wrapper. virtua's measured total and the per-row
content-box `ResizeObserver` (`row.resize`) now measure the **same** height.
There is no escaped margin left to oscillate → no `contentRO` transient → no
`spring.oscillationSnap`. It is a single display-only declaration: it adds no
horizontal padding/border, so the width-bucket keying the geometry reservation
depends on is unaffected. One chokepoint covers **all** leaf row types (instead
of per-component `mb→pb` edits) and structural group rows too, since they share
the wrapper.

### Empirical verification (real Chromium: the mechanism, not the symptom)

The bug is a single-frame measurement invariant: *a row's trailing bottom
margin must not escape its content box.* A harness mirroring the exact wrapper
chain (`contain:layout` outer = virtua stand-in → plain `[data-row-index]` →
geometry-content → `mx-auto px-6` → leaf root → 20px-bottom-margin child),
measured in Playwright/Chromium:

| Tree | virtua sees | row.resize (content-box) | divergence | stray top |
|------|------------|--------------------------|-----------|-----------|
| **before fix** (geometry-content plain) | 120 | 100 | **20**: margin escapes | 0 |
| **after fix** (`flow-root`) | 120 | 120 | **0** ✓ | 0: flush preserved |
| after fix, *un-reset* 12px top margin | 132 | 132 | 0 | **12** |

The "before" row reproduces the captured `rowΔ=0, wrapperΔ=margin` shape
exactly; the fix zeroes it. The third row demonstrates the **coupling to the
markdown edge resets** (`app.css` `.markdown-body > :first-child > :first-child
{ margin-top: 0 }`): a BFC traps a child's *top* margin as stray top space, so
flush survives only because that margin is already zeroed. The coupling is
documented in both files (the app.css comment and the MessageTimeline comment).

### Verification still owed (in-app)

The harness proves mechanism + fix + flush in isolation. The proof that the
**actual** flicker is gone is the live `timeline.margin.diverge` probe: a
severe-flicker repro that previously dumped 14 snap-coincident `diverge` records
must now produce **zero**. Retest in the running app with UI render tracing on
before committing. The probe is kept (dev-only, gated on
`isUiRenderTraceEnabled()`, consistent with the `row.resize` trace) as the
standing oracle for this bug class.

## Two distinct mechanisms

The user perceives one "flicker" but the deliberate per-type captures show two
different bugs, split by whether the row's height is clamped:

### A. Prose (`assistant_text`): a collapse-*out* margin trapped by virtua's BFC wrapper  *(CONFIRMED + FIXED: margins named above; fix in the Fix section)*

- **Signature (hard data, snap-anchored; `174725Z` 7/7 snaps in last 40s,
  `174447Z` @ −3.30s):** every visible snap is preceded ~6–8ms by a `contentRO`
  **−12** and followed at the snap by `contentRO **+12**`, a **net-zero
  ~12px** pair (consistently 11–13, *not* variable), with **no `row.resize`**
  within ±10ms (the row's content-box never changes) and, on 5/7 snaps, **no
  `timeline.state`** (no structural change). The genuine streaming growth
  (`row.resize +21/+43/+9`) arrives **~100ms later**, separate from the
  transient. `spring.oscillationSnap` recovers the same ~12px.
- **Why it's NOT the `:has()` seam (refuted):** the seam rule
  (`app.css:358`, `.md-committed:has(>p:last-child) + .md-volatile:has(>p:first-child)`)
  sets `margin-top` on `.md-volatile`, an **internal sibling margin** between
  two children of `.markdown-body`, so it counts in `[data-row-index]`'s
  content-box and **would fire `row.resize`**. It is also `1.65em ≈ 23px`, not
  12px. Both facts contradict the signature. The edge-margin reset
  (`> :first-child/:last-child`) is also refuted on magnitude: last-block bottom
  margins max at `0.25rem`=4px, h1's `0.75rem` top is the *first* block (stable
  during streaming), and on prose every edge block is `p {margin:0}` (no-op).
- **What the data licenses (locked):** virtua's item wrapper carries
  `contain: layout style` (an independent formatting context: see
  `virtua/lib/index.cjs`, the item style object). That **traps child margins
  that collapse out**. Our diagnostic `row.resize` observes `[data-row-index]`
  *content-box* (`messageTimelineTrace.ts:112`, `entry.contentRect.height`);
  virtua observes its own wrapper, also content-box. A margin that **collapses
  out** of `[data-row-index]` (through all-plain ancestors: `.px-6` has no
  vertical padding, nothing forms a BFC until virtua's wrapper) is therefore
  **absent from `row.resize`** but **present in virtua's total → `contentRO`**.
  That divergence is the whole signature. So the source is *a ~12px (0.75rem)
  margin at the top or bottom edge of the streaming row that toggles for one
  frame during the volatile↔committed re-split.*
- **RESOLVED (margins named):** the live `timeline.margin.diverge` capture
  (`183539Z`) named the collapse-out margins (error card `mb-4`, user message
  `.group mb-5`, notification/retry `mb-1.5`, api-error `mb-3`), each
  snap-coincident with `diverge == that margin`. They are real settled-DOM row
  margins (not a transient re-split state); they were invisible in the static
  walk only because they collapse *out* of `[data-row-index]` and so never touch
  its content box. Contained at the row wrapper by the `flow-root` fix (see the
  Fix section at the top).
- **"Firing a ton now, not fresh after restart" (candidate, not proven):**
  simplest explanation is just *more prose streamed → more volatile↔committed
  commits → more transients*; no perf-degradation story required. (The earlier
  "`:has()` re-eval cost scales with DOM size" note is dropped: unproven and
  tied to the refuted seam.)

### B. Think (`thinking`): imperative tail-pin goes stale on a width re-wrap → internal window jump  *(RESOLVED: handoff cause refuted; stale-pin confirmed + fixed)*

- **Signature (`173937Z`):** a visible think-block flicker with **zero
  scroll-surface trace**: no oscillation-snap, no `contentRO`, no `row.resize`
  in the ~5s around it. A think row is height-clamped at 3 lines
  (`TailClampedText`, `max-h-[3lh] overflow-hidden`), so streaming never grows
  its box, never moves the spring; the flicker is *internal* to the clamped
  span. Caveat: that trace only instruments the scroll surface, never the inner
  window's geometry or the body width, so its zero is **consistent with** an
  internal jump but is **not** positive evidence. Do not cite it as proof in
  either direction.
- **First cause considered (`liveTail→summary` handoff): REFUTED for the
  flicker under investigation, but the handoff DOES jump in a different way.**
  The earlier theory: at settle the body switches from the live smoother
  `liveTail` to the tail-trimmed `summary`, and if their last 3 lines differ
  the `scrollTop = scrollHeight` pin lands on a different `scrollHeight`. The
  pure function settles it: `liveTail = revealed` (full) and
  `summary = trimToTailRunes(revealed, 400)` share the **same suffix**, so the
  last 3 lines can never diverge *as character strings*. (This is the
  divergence check §B itself demanded. It came back negative for THIS bug.)
  **Amendment (2026-08-04):** the same-suffix argument holds for characters,
  not for the **line partition** under greedy wrapping: where a line breaks
  depends on the string's *start*, so the identical suffix can wrap into a
  different set of lines when the string above it is removed. The swap
  therefore re-wraps the visible 3 lines at settle: not the height/pin jump
  §B chased, but a real, user-visible reflow of the text in place. Fixed by
  retaining the full live tail across a content-consistent settle
  (`threadStreamingReveal.svelte.ts`, `settledTailSummaries`, served while
  the row's `summary` still matches what was retained, dropped on overwrite,
  offscreen prune, or budget eviction), so the collapsed body never swaps
  strings in front of the reader.
- **Confirmed cause: imperative tail-pin stale on a width re-wrap.**
  `TailClampedText` bottom-pinned its 3-line window with an
  `$effect: scrollTop = scrollHeight` whose only dependency was `text`. With
  `whitespace-pre-wrap`, a WIDTH change (the scroll-spring width-reflow
  oscillation, the a5a5d032 strand) re-wraps the body and grows its content
  height with **no text delta** to re-run the pin → `scrollTop` is stale → the
  newest line scrolls out of the clamped window until the next delta snaps it
  back. Worst at spring start (width settling), quiet once it settles, matching
  "stutter at the start, smooth after it expands to 3 lines." `assistant_text`
  has no clamped window and no imperative pin, so it cannot exhibit this,
  matching the thinking-vs-text differential. (A longer thread amplifies it:
  more spring travel during catch-up means more width oscillation, hence more
  stale-pin frames, matching "needs a long conversation; fine on a fresh
  thread." The fix removes the mechanism, so amplitude no longer matters.)
- **Evidence (positive, real Chromium):** `tailClampedText.browser.test.ts`: a
  width-only re-wrap leaves the newest line **231px below** the 3-line window
  with the old pin, and visible with the fix (fails-without / passes-with).
- **Fix:** replace the imperative pin with a CSS flex bottom-anchor
  (`justify-content: flex-end` on the clamped box), re-evaluated by the layout
  engine on every reflow, width re-wraps included. No `$effect`, no forced
  `scrollHeight` read per delta. `TailClampedText.svelte`.
- **Standing oracle:** `startReasoningTailJumpTrace` (messageTimelineTrace.ts)
  emits `timeline.thinking.tailJump` only on a re-wrap-without-delta that drops
  the tail below the window. Silent with the fix; against the pre-fix build it
  confirms the trigger fires live. The in-app confirmation §B owed.

**Sequencing (kept separate):** prose (A) and think (B) are two separate bugs in
two delicate, separately-documented areas, landed as two changes with two
validations, not bundled. A: margin collapse-out contained at the row wrapper
(`flow-root`). B: imperative tail-pin replaced by a CSS flex bottom-anchor. Both
carry real-Chromium regression guards (`rowMarginContainment.browser.test.ts`,
`tailClampedText.browser.test.ts`) and a standing in-app oracle.

---

## Original analysis (prose clamp/snap chain: still accurate)

> **Superseded framing below.** The sections from here down were written while
> the *source* margin was still unidentified and the bug was parked. The
> clamp → native-scrollTop-clamp → `oscillationSnap` **chain** they document is
> still accurate and load-bearing. But the "source OPEN / why parked / resume
> recipe to find the element" framing is **obsolete**: the source is named
> (mechanism A above) and the fix is applied (Fix section at the top). Read the
> chain for mechanism; ignore the "parked / still hunting" language. Mechanism B
> (think-flicker, above) remains a *separate* unconfirmed bug.

Symptom (user): during think / normal-prose streaming, when the spring scroll
"settles" at the bottom, the view flickers: "it finishes then flickers almost
every time." Worse with fast/heavy streaming. **Does not occur on tool calls or
edit commands.**

## Proven mechanism (frame-by-frame, capture `bug-report-20260629T152935Z`)

The visible flicker is a `spring.oscillationSnap` write of **16–32px**, the
only scroll writes >2px in the whole flicker window. These writes are
**fully traced** (only `spring.tick` is sampled: see the footgun below), so
this sequence is real, not sampled:

```
-186ms  W[spring.arrive] 11510->11511         spring arrives, goes sentinel-alive
                                               (sentinelEntryTarget = 11511)
 -10ms  scrollEvent top=11479  sh=12807        scrollHeight DROPPED 32px (12839->12807)
  -9ms  RO d=-31 (12634->12602) tgt=11479       contentEl SHRANK 31px; browser
                                               CLAMPED scrollTop 11511 -> 11479
   0ms  W[spring.oscillationSnap] 11479->11511  content restored, target back to
                                               sentinelEntryTarget; snap +32px
   0ms  RO d=31 (12602->12634) tgt=11511        scrollHeight restored (12807->12839)
```

End to end:

1. Spring arrives at the bottom → goes **sentinel-alive**, recording
   `sentinelEntryTarget = <bottom>`.
2. The streaming tail's measured height **transiently shrinks ~31px** within one
   frame.
3. `scrollHeight` drops → the **browser natively clamps `scrollTop`** down ~32px
   (a synchronous native operation that bypasses the property-descriptor write
   gate).
4. The height **restores** the next frame → target returns to
   `sentinelEntryTarget`.
5. The spring tick sees `current ≠ target` with `target === sentinelEntryTarget`
   → fires `snapOscillationToBottom('spring.oscillationSnap')`, +32px.
6. On screen: a fixed line jumps **down 32px (clamp frame) then up 32px (snap
   frame)** = the settle flicker.

### Why the controller cannot fix it

Once content genuinely shrinks, the browser clamp is synchronous and the clamp
frame paints before any recovery can run (rAF/RO callbacks are ordered such that
the recovery snap lands ≥1 frame later). On-screen math: to keep a fixed line
stationary on the clamp frame would require `scrollTop` > the new max, which the
browser forbids. **No `scrollTop` value can hide a 32px round-trip when the
content box itself moved.** The `snapOscillationToBottom` recovery is correct and
load-bearing (it rescues `scrollTop` from the clamp). Its FOOTGUN comment is
explicit: *fix the driver, not the snap.*

### Why think/prose only, never tool/edit

Think and prose stamp `pane.lastLiveContentAt` → `animationMode === 'spring'` →
the spring stays **sentinel-alive** at the bottom → `sentinelEntryTarget` is set
→ the oscillation snap can arm. Tool calls / edit commands do **not** stamp
`lastLiveContentAt` → `animationMode === 'instant'` → the spring cancels
(`springToken === 0`) → `sentinelEntryTarget` is never set → the same transient
shrink (if any) produces **no snap** → no flicker. The content-type specificity
is a *scroll-mode* gate, not (only) a render-path difference.

## What the driver is: and is not

The transient shrink:

- is **margin-inclusive** (virtua's `cachedSize`/total) but has **no matching
  content-box `timeline.row.resize`**. That tracer is complete, not sampled, and
  observes `contentRect.height` (content-box). So the shrink is **not** a tracked
  row's content growing/shrinking. → suspect a **margin transient** or a
  **virtua remeasure glitch**, not row content.
- is **sub-100ms**: the 10Hz `recFrame` sampler shows every per-row
  `contentHeight`/`cachedSize`, `cachedSizeSum`, `virtuaScrollSize`, and
  `scrollHeight` strictly **monotonic** (0 reversals). The `-31/+31` lives
  entirely between samples.

Ruled out as the render path: **not** the markdown committed/volatile seam. The
captured row is a **think** block (`ReasoningTailRow → TailClampedText`, a 3-line
`max-h-[3lh]` clamp fed by the monotonic live-tail smoother), **not**
`ChatMarkdown`'s two-instance split. **Not** the geometry reservation
(`min-height`): during forward streaming each chunk's signature is unique, so
`cachedTimelineRowHeight` misses and the reservation is *released*
(`minHeightPx: null`). **Not** a think-row top margin (`cacheVsWrapper === 0`).

## Refuted / retracted leads (with evidence)

### 1. Virtua remount: REFUTED

Hypothesis: virtua transiently remounts/remeasures a tail row, momentarily
dropping its explicit container height (the codebase's documented
`sentinelOscillationStranded` "replaced element" class). A dev-only
`timeline.row.mount` / `timeline.row.unmount` diagnostic was added to the
existing row tracer to test it.

Capture `bug-report-20260629T163002Z` (with the diagnostic active): over a
24-min session, 4788 mounts / 4747 unmounts, but those are ordinary **virtua
windowing** (rows entering/leaving the rendered window). Only **9** same-`itemId`
unmount→mount pairs within 120ms across the *entire* session, and **none within
±90ms of any `oscillationSnap`**. Remount does not drive this flicker. The
diagnostic was reverted after it resolved the question.

### 2. "Spring runs at ~14fps / WSLg-environmental": RETRACTED (trace artifact)

In `163002Z`, traced `spring.tick` writes were spaced a steady **~73ms** apart
(89% in 66–80ms, 0% < 33ms). This *looked* like a 13.7fps spring and led to an
environmental (software-rendered WebKitGTK under WSLg throttling rAF) theory.

It is an artifact of trace sampling. `spring.tick` is traced **1 in
`SPRING_TICK_TRACE_SAMPLE` (= 12)** writes
(`useStickToBottom.svelte.ts:181` at capture time; now
`utils/scroll/index.svelte.ts`). On the user's **165Hz** display,
`165 / 12 = 13.75Hz = 73ms` between *traced* ticks, exactly the observed
cadence. A bare `requestAnimationFrame` fps probe in the same WebKitGTK/WSLg
measured **165fps idle AND during active streaming** (worst frame gap 12ms),
proving rAF is not throttled and frames are not main-thread-bound. The spring
solver is time-compensated (`integrationFrames` multi-step catch-up) and runs
smooth at native refresh. **No environmental / GPU-compositing work is
warranted**. `/dev/dxg` + d3d12/zink mesa drivers are present and the platform
is healthy.

> ### ⚠️ Telemetry footgun: bank this
> **Never infer spring frame cadence from the spacing of traced `spring.tick`
> writes.** Only `spring.tick` is sampled (1-in-12); on a 165Hz panel the
> sampled spacing is ~73ms and masquerades as a 14fps stutter. To recover the
> real rate: `traced_spacing × SPRING_TICK_TRACE_SAMPLE` ≈ frame interval, or
> just run a bare rAF probe. All *other* `scroll.write` callers
> (`spring.arrive`, `spring.overshoot`, `spring.oscillationSnap`) and
> `scroll.contentRO` / `scroll.scrollEvent` are fully traced. Only
> `spring.tick` is decimated.

## Status of capture `163002Z`

Stripped of the sampling artifact, the capture has **no visible-magnitude
defect**: spring smooth (165fps), rAF healthy, every `oscillationSnap` only
**+1px** (sub-pixel), zero frame drops. The "stutter" the user reported was most
likely the dormant flicker in a mild moment, or perceptual. There is no hard
mechanical signature in this capture.

The +1px snaps are **not** a scaled-down repro worth instrumenting: a 1px
transient is indistinguishable from benign sub-pixel reflow noise crossing a
pixel boundary, and capturing per-element heights on the snap path would mean a
synchronous layout read at the exact delicate moment the `a5a5d032` incident
warns against (re-introducing the forced-reflow oscillation loop).

## Why parked  *(superseded: source named + fix applied; see the Fix section at top)*

- The mechanism is understood; the only open question is **which element**
  transiently shrinks ~31px.
- Naming it requires the 32px flicker **live** to measure, and it is dormant on
  the dev machine (it did not fire once across the 24-min `163002Z` session).
- A blind fix (e.g. "pin the tail row height across the transient") without
  naming the element is a guess and cannot carry the fails-without / passes-with
  regression test this repo requires.
- Parking a real, fully-documented, currently-dormant bug is the correct call
  over shipping an unvalidated fix dressed as a root cause.

## Resume recipe  *(superseded: fix applied; retained as historical capture-signature reference)*

The flicker is **already fully captured by existing un-sampled telemetry**. No
new diagnostic is needed to *confirm* a recurrence:

- **Signature:** a `spring.oscillationSnap` `scroll.write` of **≥16px**
  coincident (same frame) with a `scroll.contentRO` delta **≤ −16px**, while
  sentinel-alive (`sentinelEntryTarget ≥ 0`).

Only **once it is reproducing live**, name the element with a **one-shot, gated**
snapshot:

- Arm a *single* tail-subtree height snapshot triggered **exclusively** on the
  large-negative `contentRO` delta (≤ −16px) while sentinel-alive.
- Use **attribute reads only** (`dataset`, `querySelector`), with **no**
  `getBoundingClientRect` / `offsetHeight` / `scrollHeight` forced reads, per the
  `applyParams` / `snapOscillationToBottom` footgun comments.
- Breadcrumb: `152935Z`'s −31 had **no** content-box `timeline.row.resize`, so
  the shrink is margin-inclusive / structural: measure margins and virtua's
  per-row `cachedSize`, not the content box.

Do **not** add heavier instrumentation while the bug is dormant.

Fix layer (once the element is named), per `frontend/CLAUDE.md`: a parser-height
blip → the `svelte-streamdown` patch; a host/CSS transient → the host wrapper; a
virtua remeasure → the row geometry / virtua integration.
