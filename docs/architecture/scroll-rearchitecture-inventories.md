# Scroll Re-Architecture — Full Inventory Outputs

Supporting evidence for `scroll-rearchitecture-plan.md`. Verbatim final
reports of the six investigation agents run against baseline `f42dc6e6`
(2026-07-01). Line references are to that commit. Prune once the plan's
stages have consumed them.


---

## A1 — useStickToBottom controller (45 mechanisms)

Inventory complete. I read the full 2884-line controller at HEAD f42dc6e6 (working tree matches HEAD), the 54-line latch, ran pickaxe provenance on every named mechanism, and verified all production call sites. Below is the structured inventory.

# useStickToBottom.svelte.ts — mechanism inventory (HEAD = f42dc6e6)

## A. Geometry bands & classification constants

**1. STICK_TO_BOTTOM_OFFSET_PX = 70** (L82, used L910-914)
WHAT: visual near-bottom band; drives `isNearBottomState` → chip visibility and the negative-delta repin's geometric branch. WHY: chip flicker prevention for users hovering near bottom (upstream port, d2666f5a). COUPLING: `refreshIsNearBottom()` called after every write, every scroll event, contentRO; feeds public `isAtBottom` getter (L2852) and `negativeWillPin` (L1585). VERDICT: KEEP — pure UX band, unrelated to churn.

**2. AUTO_FOLLOW_BOTTOM_EPSILON_PX = 4** (L88)
WHAT: re-stick tolerance — a DOWN scroll landing within 4px of bottom flips back to sticky (L2009); also the gate's `domAlreadyPinned`/`requestedMovesAwayFromBottom` tests (L2562-2565). WHY: virtua row-height estimation + browser scrollTop rounding lands 1-3px short during streaming; matches react-virtuoso default. VERDICT: KEEP — browser rounding is still live; but note it's dual-purposed into the gate carve-out (that use dies with the gate).

**3. IDLE_REPIN_DEADBAND_PX = 4** (L100, gate at L1577-1589)
WHAT: while `springToken===0` and scrollTop within 4px of target, a nonzero contentRO height delta is treated as fractional-DPR wobble and the re-pin is skipped. WHY: db77ba0d — idle viewport-vibration limit cycle (fractional-DPR X.5-boundary height flip → moving target → self-sustaining ±2px cycle; capture in settle-flicker-analysis.md). COUPLING: gates both `positiveWillPin` and `negativeWillPin`; deliberately equals #2. VERDICT: SIMPLIFY/UNKNOWN — f42dc6e6 also landed "cache fractional row heights" (timelineRowGeometry side) which may have removed the DPR-flip driver; the deadband may now be a redundant second defense. Needs one idle-thread capture on fractional-DPR display to decide.

**4. CONTENT_REFLOW width epsilon + settle window** (L104-108; detection L1489-1492; uses L1528, L1717-1733, L1781-1784, gate L2582-2585)
WHAT: width change >0.5px marks the next 250ms as "layout correction, not content" — forces sync-pin even in spring mode, and lets virtua's compensation through the write gate. WHY: 07248c5f — pane/sidebar/window reflow re-wraps prose; spring-chasing a half-viewport reflow from a stale bottom looked broken. Width and height arrive in separate RO deliveries, hence the window. VERDICT: KEEP — reflow is real and unrelated to the fixed churn.

## B. Warm-up / quiescence gate

**5. QUIET_MS=100 / FAILSAFE_MS=2500 / SETTLED_QUIET_MS=16 / WARMUP_SETTLE_EPSILON_PX=8** (L206-266; state L718-741; `beginWarmup` L765-783, `bumpQuietTimer` L800-827, `quietWindowForGeometry` L790-794, `markWarm` L754-763)
WHAT: after attach()/restore-forceStick/armWarmup, `warm=false` until contentRO is quiet for QUIET_MS (or SETTLED_QUIET_MS when the consumer's `quietContextSignal` says async typesetting finished AND the last height delta ≤8px), failsafe at 2500ms. Consumers hide content while `!isWarm`. WHY: the e00723f8 "80LoC-spring-delete regression" (mount-time virtua remeasure + Streamdown typesetting spring-chased a thread restore visibly); adaptive shortening from acc42fb8/a2dca3d9; geometry-gating from 8bf8b97f (the "lands right, flickers, lands right again" idle-thread regression — virtua estimate→measure cascade fires wider apart than 16ms). Quiet timer armed only on first RO evidence (L765-783) so an in-flight slice fetch with no contentEl can't open the hide-gate early (4d7e6d11-adjacent). COUPLING: `warm` gates springGateOpen callers (L1727), the descriptor gate tier 3 (L2528), notifyLiveContentMaybeGrew's spring path (L2370); MessageTimeline's cascade-hide reads `isWarm` (MessageTimeline.svelte:649). VERDICT: KEEP — virtua's estimate→measure mount cascade exists regardless of the marking fix. (Under merged virtualization+scroll this becomes internal state, but the mechanism survives.)

**6. quietContextSignal option + notifyQuietContextSignalChanged()** (L516-535, L829-850)
WHAT: consumer-supplied "streamdown settled" predicate read per-fire; the notify method re-arms a live quiet timer with the shortened window. WHY: acc42fb8 — 100ms reveal latency on thread switch when we have first-hand evidence typesetting is done. VERDICT: KEEP.

## C. Spring chase

**7. Spring tuning (DEFAULT_SPRING, SIXTY_FPS_INTERVAL_MS, SPRING_MAX_CATCHUP_STEPS=3)** (L120-138)
WHAT: 60Hz spring recurrence, stiffness 0.08 (up from upstream 0.05), bounded catch-up per rAF. WHY: upstream tail too slow for fast streaming (dc0d644c/9b109072); stall-bounding prevents teleports after a heavy frame. VERDICT: KEEP.

**8. Frame-rate-independent fractional integration** (L1307-1365)
WHAT: dt-scaled steps (`Math.pow(damping, stepFraction)`, proportional stiffness) so 120Hz writes every frame. WHY: 9b109072 "smooth mac timeline follow" — 120Hz displays saw every other frame held. VERDICT: KEEP.

**9. RETAIN_ANIMATION_DURATION_MS=350 + sentinel mode** (L139-156; sentinel branch L1432-1460; `sentinelEntryTarget` L694-702)
WHAT: after arrival, if `animationMode()==='spring'`, spring re-rAFs WITHOUT writing (token stays nonzero) so the external-write gate and negative-delta carve-out stay engaged across >350ms gaps (shiki loads, parseIncompleteMarkdown rebalances). WHY: bcd597c2 — without it, springToken=0 dead windows let virtua $fixScrollJump / negative sync-pin snap 1-2 lines mid-stream. COUPLING: the load-bearing `SPRING_MODE_HOLD_MS(500) > RETAIN(350)` cross-file constraint with the latch; `sentinelEntryTarget` seeds both oscillation snaps (#11, #19). VERDICT: SIMPLIFY — the sentinel's *gate-holding* half exists only because virtua writes directly (Q1); its *oscillation-recovery bookmark* half defends browser auto-clamp, which no routing fixes. Under a routed-delta design the "keep rAF alive just to keep a token nonzero" pattern deletes.

**10. Momentum carry (SPRING_CARRY_VELOCITY_CEILING=4)** (L162-186, L1398-1416)
WHAT: at catch-up, keep upward velocity ≤4 px/frame within the retain window; shed larger/downward remnants. WHY: ad11be4b — line-quantized growth caused slow-start lurches per wrap; ceiling prevents re-introducing the big→small snap that diff===0 zeroing fixed (e14ada31 pinned remnants 8/14/28). VERDICT: KEEP — pure kinematics UX, still valid. (2026-07 update: carry is now a clamp-not-zero rule at the same ceiling, plus a shaped glide profile: hard `SPRING_MAX_VELOCITY_PX_PER_FRAME=18` cap, `SPRING_DECEL_ENVELOPE_RATIO=0.11` deceleration envelope (speed ∝ remaining; caps small-quantum peaks and shapes the ease-out), `SPRING_MIN_VELOCITY_PX_PER_FRAME=1.6` glide floor that keeps the integer-quantized tail from rendering as 1px-step judder, and a `SPRING_SETTLE_TAPER_RATIO=0.4` bounded settle over the last ~4px; see the constants block in spring.ts.)

**11. Spring-tick oscillation snap** (L1342-1343, `snapOscillationToBottom` L1229-1256)
WHAT: sentinel-idle spring whose target returns to sentinel-entry value while scrollTop is stranded (browser clamped during the dip) snaps instantly instead of chasing zero net change. WHY: 5aab3c34 — content height -N/+N from "async Streamdown typesetting / virtua row remount"; browser clamp is native and bypasses the descriptor gate. FOOTGUN comment (L1239-1249): this is one-shot clamp recovery — if it fires per-frame, the driver is elsewhere (cites timelineRowGeometry applyParams forced-layout incident). VERDICT: UNKNOWN — "virtua row remount" as a dip source was plausibly the now-fixed buffer-drop churn; the streamdown-oscillation and browser-clamp legs are still live. Needs a post-fix streaming capture counting `spring.oscillationSnap` events.

**12. Arrival thresholds + exact-arrival readback acceptance** (L159-160; `arrivalReadbackAcceptedTarget` machinery L879-904, uses L1315-1320, L1332, L1451, L2391)
WHAT: engines that reject exact max scrollTop by 1 CSS px (fractional DPR) would otherwise be re-written the same rejected target forever; the controller records "browser accepted within 1px of T" and stops re-writing. WHY: 9b109072 (mac) — repeated rejected writes = jank + needless RO pressure. VERDICT: KEEP — browser rounding behavior, untouched by any fix.

**13. Overshoot guard w/ instant-snap threshold (SPRING_OVERSHOOT_INSTANT_SNAP_THRESHOLD_PX=50)** (L194-204, L1616-1655)
WHAT: overshoot past target snaps instantly when no spring in flight OR magnitude >50px; small overshoots mid-chase damp through the symmetric spring. WHY: b91c4389 — unconditional snap caused visible up-down oscillation per chunk from parseIncompleteMarkdown token-close/reopen shrinks; large overshoots (virtua applyJump mis-corrections, collapse) must still snap. VERDICT: KEEP, possibly SIMPLIFY — the small-overshoot leg (streamdown rebalance) is still live; the "virtua applyJump mis-correction" leg shrinks under routing.

**14. springGateOpen() shared predicate** (L1258-1271) — !stop && !paused && isAtBottom && !escaped && !reducedMotion && (mode==='spring' || structural window). Shared so contentRO and startSpringIfNeeded can't drift. VERDICT: KEEP (becomes the resolver's decide step).

**15. Structural-append one-shot (markStructuralContentPending / STRUCTURAL_APPEND_SPRING_WINDOW_MS=250 / springStartedFromStructuralAppend)** (L118, L693-694, L1276-1278, L2425-2427; gate exception L2594)
WHAT: next near-term growth is spring-eligible even while mode==='instant'; chase cancels after arrival (doesn't enter sentinel). WHY: 9b109072 + 62c79246 — command/tool row batches snapped instead of animating; unrelated idle reflows must not become spring-eligible. COUPLING: consumed in springGateOpen; excepted in the gate's instant-mode pass-through. VERDICT: KEEP — UX feature, not compensation.

## D. Write path & self-classification

**16. writeProgrammaticScrollTop chokepoint** (L1026-1090) + **writeScrollTop scroll-behavior suppressor** (L1092-1101)
WHAT: THE single funnel for every controller write: fires `onBeforeScrollTopWrite` (virtua marking), flips `controllerOwnsScrollTopWrite` around the assignment (descriptor-gate bypass, try/finally), tags `ignoreScrollToTop` with browser-rounded readback, records a scroll-event token, updates restick baseline, refreshes near-bottom, sampled trace. WHY: single-writer discipline + self-tagging (d2666f5a onward; perf shape from 33d6ea48/dc0d644c). VERDICT: KEEP — this IS the embryo of the "controller as only writer" target; a resolver keeps exactly this and deletes callers-racing-callers.

**17. onBeforeScrollTopWrite hook** (L536-564, fired L1055)
WHAT: sync pre-write callback; chat wires it to patched virtua `markProgrammaticScroll()`. WHY: f42dc6e6 — the root-cause fix; unmarked pin writes classified as user scroll-downs made virtua drop its above-viewport buffer every streaming beat (settle-flicker-analysis.md). Contract: must not throw; fires even on no-op writes (benign over-retention). VERDICT: KEEP — this is the fix itself. Under "virtua delta routed through controller" it would evolve into the routing seam rather than delete.

**18. ignoreScrollToTop exact tag** (L605, set L1070, consumed L1906-1911)
WHAT: one-shot exact-value tag consumed synchronously by the next scroll event. WHY: original upstream-port mechanism (d2666f5a) to keep the controller's own writes from reading as user scrolls. VERDICT: SIMPLIFY — coexists redundantly with #19 (`exactTagged || tokenTagged`, L1911); a resolver with one write/frame needs only one expected-value check.

**19. Programmatic scroll-event tokens (TTL=500ms, dup budget=4, MAX=128)** (L115-117, L1103-1139)
WHAT: FIFO of {top, expiresAt, remaining} tokens; each write mints one; scroll handler consumes matching top, up to 4 duplicate events per token. WHY: 9b109072 — multiple controller writes per frame each dispatch async scroll events; the single tag (#18) only matches the last write; WebKit can deliver several events for one write. VERDICT: KEEP while multiple write sites exist per frame; SIMPLIFY under a per-frame resolver (one write → one token, budget survives for coalesced-event engines).

**20. External scrollTop descriptor write gate** (state L610-672; install/uninstall L2464-2688)
WHAT: replaces `scrollTop` accessor on scrollEl while attached. Non-controller writes cascade through tiers: (a) controller-owned → pass (L2497); (b) runExternalScroll window → pass (L2505); (c) !warm/escaped/paused/!atBottom → pass — virtua's mount-cascade compensation must land (L2528); (d) **anchor-redirect carve-out**: DOM already pinned && virtua requests meaningfully below bottom → redirect write to `targetScrollTop()` (L2560-2577); (e) width-reflow window → pass (L2582); (f) mode==='instant' (minus structural chase) → pass (L2594); (g) springToken===0 → pass (L2611); (h) **magnitude pass-through**: requested jump > clientHeight → pass (L2615-2647); (i) else DROP (suppression trace L2657). WHY, per tier: base gate 483e4339/78bc3213 (canonical capture: 24 untagged $fixScrollJump jumps in one long-stream session; spring.arrive@29360 → untagged scroll@29387 = mid-stream 1-2 line snap); tier-g from bug-report-20260524T200233Z (15/15 suppressions at springToken=0 = thread-switch flicker); tier-d + tier-h from 8bf8b97f (bug-report-20260622T041049Z: one suppressed +2276px virtua write → ~1s 2300px spring chase); drop-vs-redirect subtlety: swallowed writes fire no scroll event, desyncing virtua's DOM-derived offset model (revert-to-top regression). COUPLING: reads warm, isAtBottomState, escapedFromLockState, pauseDepth, springToken, springStartedFromStructuralAppend, contentReflowSettleUntil, externalScrollIgnoreUntil, animationMode(). VERDICT: **DELETE-CANDIDATE under the target architecture** — this entire ~230-line mechanism plus its 5 carve-outs exists ONLY because virtua writes scrollTop directly; routing $fixScrollJump's delta through the controller replaces all nine tiers with one decision. Until routing lands: KEEP (cause fully live — the marking fix changed virtua's *classification*, not its *writes*).

## E. Scroll-event interpretation & user intent

**21. handleScroll: sync tag consumption + tagged fast-path** (L1892-1924)
WHAT: captures scrollTop, consumes tag/token/external-window; tagged events return before any allocation. WHY: correctness (one-event suppression, L1903-1905) + perf (33d6ea48 — hundreds of throwaway timer allocs/sec during streaming). VERDICT: KEEP.

**22. Deferred 1ms intent check + resizeDifference RO-race guard** (L1952-2026; clear timer L1799-1811; RESIZE_CLEAR_PADDING_MS=1)
WHAT: untagged scroll events defer 1ms so a concurrent RO can stamp `resizeDifference`; resize-correlated events are layout, not intent — unless live down-intent proves the user is re-sticking. WHY: upstream mirror; "most importantly virtua's $fixScrollJump… would otherwise look like user scroll movement" (L1969-1976); Bug-A capture-at-event-time fix L1895-1902 (long Opus threads: bottom moved during the 1ms window → false re-stick negative). VERDICT: SIMPLIFY — the virtua-shaped justification dies with routing, but layout-induced scroll events also come from browser clamp and ChannelView (no virtua), so the guard shrinks, not deletes.

**23. resizeCorrelatedUntaggedScrollBudget** (L667, set L1545-1551, consumed L1939-1940, cleared L1808/L2341)
WHAT: a one-event budget treating the next untagged scroll after a content resize as layout even if the resizeDifference clear raced ahead. WHY: 5355a9e3 — virtua emits one untagged scroll jump as part of the same measurement correction. VERDICT: DELETE-CANDIDATE under routing (the untagged write becomes controller-owned/tagged); KEEP until then.

**24. Recent-down-intent re-stick consent (RECENT_DOWN_INTENT_WINDOW_MS=250, version counter)** (L112, L656-658, L917-928, L975-1005; consumed L1981-2009)
WHAT: wheel-down/key-down/touch-up while escaped opens a 250ms window; re-stick requires window + genuinely-downward scroll (`lastObservedScrollTopForRestick` baseline, L668-672 / 8ba60036) + landing ≤4px from bottom; versioned so stale deferred closures can't consume a newer intent. Immediate re-stick if already at bottom (at-boundary wheel lockout: browser fires no scroll event at max scroll, L998-1004). WHY: a2efc785 (auto-follow stall after wheel-down during RO cascade), afce64c0, 8ba60036, b1e4cca8, 203e2ada (momentum/stale-scrollend gestures). VERDICT: KEEP — user-gesture disambiguation against layout clamps is independent of the fixed churn; version counters are a symptom of multi-timer state (Q3) and shrink under a resolver.

**25. Scrollbar drag session** (L113, L659-663, L930-961; armed L1859-1889; consumed L1962-1967, L1984-1986)
WHAT: pointerdown in the scrollbar gutter arms a session (pointerup/pointercancel/blur/30s failsafe ends it); drag-up escapes, drag-down-to-bottom re-sticks — scrollbar drags produce no wheel/key events. WHY: 48d81531. VERDICT: KEEP.

**26. Escape paths (wheel/key/touch/pointer/selection)** (L1828-1857 wheel; L1859-1889 pointer incl. middle-click; L2030-2051 key/touch; module `mouseDown` + `isSelectingInside` L268-307, spring pause L1295-1300, deferred escape L1998-2004)
WHAT: upward input escapes; selection crossing the scroller pauses/escapes. WHY: core intent model (upstream + e8c8d6e6, 07e53fb2). Touch axis inversion documented L2042-2047. VERDICT: KEEP.

## F. Consent/one-shot gates

**27. Restore-snap consent (armRestoreSnap / forceStick reason:'restore')** (L704-716, L2091-2098, gate L2238-2249, consume in markAtBottom L2297, deliberately NOT cleared in detach L2832-2843)
WHAT: `forceStick({reason:'restore'})` no-ops unless the thread-switch entry point armed consent; any escape intent / animateScrollTo / stopScroll / user-forceStick clears it. WHY: 24f7ef02 — the seq-509 trace bug: a restoreToBottom firing 17s after a wheel-escape slammed the user to bottom. The detach-preservation quirk exists because attach() calls detach() between the consumer's `$effect.pre` arm and the restore `$effect`. VERDICT: KEEP — defends consumer-effect lifecycle staleness, not scroll churn; a resolver design could subsume it by making restore an explicit one-call API (Q4).

**28. runExternalScroll / tagExternalProgrammaticScroll (EXTERNAL_SCROLL_TAG_CLEAR_MS=100)** (L114, L2109-2126; gate tier-b L2505)
WHAT: wraps virtua `scrollToIndex` etc. in a 100ms ignore window + optional escape; also passes writes through the descriptor gate. `preserveIntent` variant is the seam MessageTimeline's `notifyHostLayoutSettled` uses (pane-move offset rewrite). WHY: external writers the controller can't perform. VERDICT: KEEP; SIMPLIFY under full routing (external navigation becomes a controller-performed scroll and the time-window heuristic dies).

**29. pauseAutoScroll lease + release re-pin** (L2429-2462)
WHAT: depth-counted suspension; releasing the last lease re-pins if still sticky. WHY: sidebar resize / terminal toggle shrink the chat column mid-lease (scrollLeaseDuringTransition.ts:31). VERDICT: KEEP.

## G. Notify seams & pins

**30. notifyContentMaybeGrew** (L2349-2366) + **instantPinAfterExternalGeometryChange** (L2330-2347)
WHAT: out-of-content geometry changed (composer height) → instant pin if sticky; stamps `resizeDifference=1` BEFORE the write so the resulting scroll event reads as layout. WHY: composer RO seam (both ChatView and ChannelView). VERDICT: KEEP.

**31. notifyLiveContentMaybeGrew** (L2368-2423)
WHAT: live transcript advanced without a usable RO delta; honors animationMode/structural marks: at-target → exact-arrival write only; spring-eligible gap → chase; small overshoot mid-chase → let the symmetric spring damp (bump retain only); else instant pin. WHY: daf6ffe4/62c79246 — structural nudges snapped or double-wrote during spring follow. VERDICT: KEEP; collapses naturally into a resolver's single decide step.
(Note: `notifyHostLayoutSettled` and `preserveTimelineWindowAnchor` from the PaneScrollController interface (threadPaneShared.ts:143-150) are implemented in MessageTimeline's wrapper, NOT in this file — sibling scope; the controller-side primitive they ride on is #28.)

**32. contentRO first-fire pin** (L1505-1525) — initial mount snaps to bottom synchronously if sticky. KEEP.

**33. contentRO delta===0 early-out** (L1532-1543) — same-height remeasures, padding-var churn. KEEP (hot path).

**34. Negative-delta re-stick with intent disjunct + spring carve-out** (L1735-1796)
WHAT: shrink re-pins when `isAtBottomState || isNearBottomState`; the isAtBottomState disjunct defends virtua's cascade shifting scrollTop hundreds of px (c85dc226 "half-screen jump on uncached loads"); mid-chase the sync write is suppressed and only `lastTargetChangedAt` bumps (0bd9a4e2 — virtua's +ESTIMATE/-CORRECTION pair racing the spring). VERDICT: KEEP the intent disjunct (estimate cascade still real); the spring carve-out is a two-writers-in-one-frame artifact that a resolver deletes by construction (Q3).

**35. Synchronous sentinel oscillation recovery in contentRO** (L1657-1697)
WHAT: same recovery as #11 but inside the RO delivery — rAF fires BEFORE RO per HTML rendering order, so the spring-tick snap is one frame late; this closes the stranded frame. WHY: 3697555a, bug-report-20260615T182227Z (above-viewport image row remeasure, ~37px jolt). VERDICT: UNKNOWN — the dip source (above-viewport remount) overlaps heavily with the now-fixed buffer-drop churn; live-window fold eviction can still cause legitimate remounts. Same capture-experiment as #11 decides both.

## H. Snap/navigation APIs

**36. forceStick** (L2226-2280) — user/restore snap; restore reason re-arms warmup; resets springStopRequested. KEEP.
**37. markAtBottom** (L2282-2303) — flag-only snap for empty timelines / pre-established geometry; consumes restore consent. KEEP.
**38. animateScrollTo** (L2170-2224 + helpers L1179-1207) — eased controlled scroll owning its writes. VERDICT: **DELETE-CANDIDATE — zero production callers** (grep: only the controller's own test file; the doc comment "Used by handleLoadOlder / scrollToItem" is stale — those use runExternalScroll/pauseAutoScroll now).
**39. stopScroll** (L2100-2104) — VERDICT: **DELETE-CANDIDATE — zero production callers**; trivially `setEscapedFromLock(true)` + clears.
**40. setEscapedFromLock** (L2054-2089) — escape flip; true-path cancels spring/animation and clears restore consent (clear-then-arm ordering contract with consumers, L444-451). KEEP.
**41. preserveScrollAnchor** (L2128-2168) — disclosure toggles keep the clicked anchor stationary for escaped users, stay pinned for sticky users; used by ~14 chat row components via preservePaneScrollAnchor. KEEP.

## I. Support & diagnostics

**42. prefers-reduced-motion (cached MediaQueryList)** (L1162-1177; consumed L1269, L2187) — accessibility: no spring, no eased animation; overshoot still snaps (L1644-1646). KEEP.
**43. Trace surface** (L59-73 thunk; sampled spring writes L1015-1025, L1036-1043; ~25 labeled sites; dev hook `window.__stickState` L2735-2788) — dev-only, double-gated closures (Rolldown doesn't DCE the alloc); sampling from 33d6ea48 (5% of 10MB rotation was spring ticks). KEEP — this is the diagnostic backbone every cited capture came from.
**44. writeCaller attribution** (L1007-1014) — module-mutable diagnostic set before each write; no semantics. KEEP (folds into resolver's single write site trivially).
**45. attach/detach lifecycle** (L2691-2846) — gate installed before any RO can fire (L2696-2699); detach restores descriptor first; full state reset EXCEPT restoreSnapArmed (#27). KEEP.

---

# Design questions

## Q1 — exists ONLY because virtua also writes scrollTop (deletable under routed $fixScrollJump)

Evidence-ranked:
- **#20 the entire descriptor gate** (L610-672 + L2464-2688, ~290 lines incl. comments) — its own header says the sole purpose is filtering virtua's direct `$fixScrollJump` writes (virtua@0.49.1 core/index.js:259-266). Every tier, both carve-outs (anchor-redirect L2560, magnitude L2615), and both cited bug-reports (20260524T200233Z, 20260622T041049Z) are consequences of drop-vs-pass decisions on a writer the controller can't see semantically. A routed delta callback gets the *reason* (above-viewport compensation, N px) and makes one decision.
- **#9 sentinel mode's gate-holding half** — comment L1437-1441: "keep the external write gate… engaged. Without this… the dead window lets virtua $fixScrollJump… snap." The zombie-rAF-loop exists to keep `springToken !== 0` visible to the gate.
- **The SPRING_MODE_HOLD_MS > RETAIN_ANIMATION_DURATION_MS cross-file invariant** (latch L26-37, controller L150-156) — self-enforcing only because sentinel survival requires `animationMode==='spring'` so the gate stays closed; dissolves with the gate.
- **#23 resizeCorrelatedUntaggedScrollBudget** — "virtua can emit one untagged scroll jump" (L1546-1550); routed writes are tagged by construction.
- **#34's spring carve-out** (suppress own negative sync-pin mid-chase) — indirectly: it exists because the RO pin and the spring are two racing responses to virtua's estimate/correction pair.
- Partially: #22's virtua clause, #13's applyJump clause, #35/#11 to the extent dips come from virtua-internal remounts.

## Q2 — exists ONLY because programmatic writes were indistinguishable from user scrolls (partially addressed by f42dc6e6)

Precise framing: the marking fix changed *virtua's* classification of *controller* writes; nothing in this file classified differently before/after. What the fix removed is the **downstream churn** (buffer drop → remount → RO storm → height oscillation) that several defenses were tuned against:
- **#11 / #35 oscillation snaps** — both cite "virtua row remount" dips; if the remounts were mostly the misclassification buffer-drop, these fire rarely now. UNKNOWN pending capture.
- **#3 IDLE_REPIN_DEADBAND** — the ±2px idle cycle was DPR-rounding, but f42dc6e6's paired fractional-row-height caching may have removed the flip driver; the deadband may be a second belt on a fixed suspender.
- **#19's duplicate budget of 4** — plausibly tuned while each pin write triggered virtua reaction scroll events; a budget of 1-2 may suffice now. UNKNOWN.
- NOT in this class: #18/#19/#21 as such (the DOM scroll event stream is inherently writer-anonymous — self-tagging is needed forever), #24/#25 (user gestures vs layout clamps — browser clamp is native).

## Q3 — multi-point reads/writes within one frame; orderings that matter

1. **rAF-before-RO within a frame** (documented L1668-1676): spring tick writes against a pre-RO target; the RO then re-decides. This ordering is the entire reason #35 duplicates #11. A per-frame resolver running at one point removes the duplicate — but must pick a phase: post-RO (RO callbacks feed observations; resolve in the same microtask or a queued rAF) or it re-creates the one-frame-stale paint.
2. **Up to two writes per contentRO delivery**: overshoot snap (L1653) then positive/negative pin (L1732/L1785) — plus `refreshIsNearBottom` lifted above both so trace and gate read the same geometry (L1553-1559). Resolver collapses to one write; observable change: the intermediate overshoot-snapped scrollTop never exists.
3. **Same-frame independent writers**: contentRO pin, composer-RO `notifyContentMaybeGrew` pin, `pauseAutoScroll` release re-pin, spring tick — each calls writeScrollTop independently; `ignoreScrollToTop` is last-write-wins, which is exactly why token list #19 exists. One write/frame → one token.
4. **lastTargetChangedAt bumped at 4 sites** (L1728, L1794, L2400, L2414) — order-sensitive with the spring tick's retain check in the same frame.
5. **Cross-timer version captures**: `recentDownIntentVersion`, `scrollbarDragSessionVersion`, `downIntentVersionAtEvent` (L1941-1944) exist because intent state mutates between the scroll event and its 1ms-deferred interpretation. A same-frame resolve eliminates the deferral and the versioning — BUT the 1ms defer is also how RO-vs-scroll ordering ambiguity is absorbed (L1958-1959); the resolver must keep an equivalent "was this scroll layout-correlated" input.
6. **resizeDifference stamped before write** in instantPin (L2330-2335) vs stamped in RO before pin (L1545) — two producers of the same classifier read by handleScroll; ordering with the write matters both times.
7. **Descriptor-gate reads mid-frame**: `currentScrollTop` captured once (L2561) and reused at L2643 with a proof comment that no write intercedes — fragile hand-verified frame-ordering of exactly the kind a resolver formalizes.

## Q4 — minimal public API

Current surface: 4 getters + 17 methods (L317-494). Production usage evidence says the true surface is:
- **Reads**: `isSticky`, `isAtBottom`, `escapedFromLock`, `isWarm` (all used).
- **Lifecycle**: `attach`, `detach`.
- **Intent**: `setEscapedFromLock`, `forceStick(user|restore)`, `markAtBottom`, `armRestoreSnap` — mergeable: `armRestoreSnap`+`forceStick('restore')`+`markAtBottom`'s consent-consume are one thread-restore transaction split across three calls purely because consumer `$effect` timing separates arm from consume; a resolver API could expose `restoreToBottom()` idempotent-with-consent instead (3 methods → 1).
- **Observations**: `notifyContentMaybeGrew`, `notifyLiveContentMaybeGrew`, `markStructuralContentPending`, `notifyQuietContextSignalChanged` — in a resolver these become one `observe(kind)` feed.
- **Warmup**: `armWarmup`, `skipWarmup` (both used; MessageTimeline.svelte:463, 1481).
- **Leases/anchors**: `pauseAutoScroll`, `preserveScrollAnchor`, `runExternalScroll`.
- **Dead**: `animateScrollTo`, `stopScroll` — zero non-test callers; delete now regardless of re-architecture.
- Pane seam (threadPaneShared.ts:139-152) already proves how small the cross-store surface is: 3 required methods + 3 optionals + 1 getter.
Minimal resolver surface ≈ attach/detach, 4 reads, escape(bool), stickToBottom(), restoreToBottom(), observe(contentGrew|liveGrew|structural|quietSignal), warmup(arm|skip), lease(), preserveAnchor(), routedExternalScroll() — ~12 members vs today's 21, with the biggest wins from merging the restore triple and the notify quartet.

## Q5 — springAnimationLatch

(`frontend/src/lib/utils/springAnimationLatch.ts`, 54 lines.) It latches **animation mode**: returns `'spring'` for `SPRING_MODE_HOLD_MS = 500ms` after the pane's `lastLiveContentAt` stamp, else `'instant'` (L48-54). The stamp is written by the data layer (`stores/thread.svelte.ts`) on text/reasoning reveals and text-like rows — i.e., it latches "**live content recently advanced**", deliberately keyed on content mutation rather than turn lifecycle (daf6ffe4), which fixes turn-ends-while-still-streaming and the end-of-turn word-by-word drain, while keeping idle shiki/KaTeX/mermaid growth (which never stamps) on sync-pin.
Why outside the controller: (a) **layer separation** — the controller is geometry-only by design; knowing what counts as "live content" requires data-layer semantics the controller must not own (options.animationMode is read per-fire, L515); (b) **purity/testability** — `now` is a parameter (L44-47); (c) **shared surface** — the latch and its HOLD constant are imported by MessageTimeline (MessageTimeline.svelte:12,438) while ChannelView omits animationMode entirely and stays sync-pin. Its one load-bearing coupling back into the controller is `HOLD(500) > RETAIN(350)` (latch L26-37; controller exports RETAIN at L156 solely so the colocated test pins the relationship) — that constraint exists only to keep the descriptor gate closed for the sentinel's lifetime and dissolves with the gate (Q1).

---
Cross-boundary notes for synthesis: `notifyHostLayoutSettled`/`preserveTimelineWindowAnchor` live in MessageTimeline's PaneScrollController wrapper, not here (their controller-side primitive is `runExternalScroll({preserveIntent:true})`, L425-432); the `warm` flag's consumer-side hide gate is MessageTimeline.svelte:649; the virtua patch drop-rule and the `markProgrammaticScroll` contract live in frontend/CLAUDE.md's virtua@0.49.1.patch entry.

result: Full mechanism inventory of useStickToBottom.svelte.ts delivered — 45 mechanisms with provenance and verdicts; headline findings: the ~290-line scrollTop descriptor gate (all 9 tiers + both carve-outs) and the sentinel's gate-holding rAF loop are deletable only under routed $fixScrollJump, animateScrollTo/stopScroll have zero production callers today, the oscillation snaps and idle deadband need one post-f42dc6e6 capture to classify, and the minimal resolver API is ~12 members vs today's 21.


---

## A2 — MessageTimeline flows (~30 mechanisms)

All evidence gathered. Sanity check: read both target files end-to-end (MessageTimeline.svelte 2189 lines, frontend-scroll.md 559 lines), verified provenance for every mechanism via `git log -S`, confirmed the pane-side controller seam (`thread.svelte.ts:986,3183`) and that `captureTimelineAnchor` uses virtua geometry, not DOM. Inventory below.

# MessageTimeline.svelte scroll-machinery inventory

All line numbers are current working tree (`/home/rmurphy/repos/agent-overflow/frontend/src/lib/components/chat/MessageTimeline.svelte`). "The fix" = f42dc6e6 (markProgrammaticScroll patch, fractional heights).

## A. Controller construction & wiring

**A1. Stick controller creation + onBeforeScrollTopWrite marking** — L441-452.
- WHAT: creates `useStickToBottom` controller with `animationMode` thunk, `quietContextSignal` thunk, and `onBeforeScrollTopWrite: () => listRef?.markProgrammaticScroll()` — the settle-flicker fix wiring.
- WHY: marking hook is f42dc6e6 (capture bug-report-20260701T201655Z; settle-flicker-analysis.md 2026-07-01 entry). Doc contract: frontend-scroll.md §Intent And Programmatic Writes L225-240.
- COUPLING: `listRef` ($state, rebinds on every {#key} remount — the thunk re-reads it each write, so remounts self-heal); patched `virtua@0.49.1` handle. Tests: `messageTimelineVirtuaMarking.test.ts`, `virtua-patch-buffer-retention.browser.test.ts`.
- VERDICT: **KEEP** — this IS the root fix; under merged virtualization it becomes internal and disappears as a seam.

**A2. paneScrollController publish** — L631-634 (Object.assign of `stick` + `notifyHostLayoutSettled` + `preserveTimelineWindowAnchor`), attach/detach effect L659-662.
- WHAT: publishes controller onto pane so external surfaces take pause leases and so the PANE calls back INTO the timeline (`thread.svelte.ts:986` calls `scrollController?.preserveTimelineWindowAnchor` for the settle prune; `:417,:3869` take settle leases).
- WHY: registration slot per doc §Owners; the prune callback per §Live Window Bounds.
- COUPLING: pane ↔ timeline bidirectional. The Object.assign widens the controller type with timeline-owned methods — two owners under one handle.
- VERDICT: **SIMPLIFY** — the widened handle blurs the doc's "do not add another owner" rule; a re-architecture should give the pane→timeline prune transaction its own seam or move it into the merged resolver.

**A3. stick.attach + auto-load gesture re-arm** — L667-692.
- WHAT: attaches controller to scrollEl/contentEl (installs RO + intent listeners); adds wheel/touchmove/keydown listeners that re-arm both auto-load gates on real gestures.
- WHY: gesture-armed gate per doc §Thread Switch L39-54 (incident bug-report-20260616T143320Z is the gate's progress-guard, in `timelineScroll.ts`).
- COUPLING: attach() also arms warm-up on first attach (`useStickToBottom.svelte.ts:2691`, comment L721); early-returns on unchanged elements across switches — which is exactly why B1 must re-arm warmup itself.
- VERDICT: **KEEP**.

## B. Thread-switch restore split

**B1. $effect.pre: switch reset + escape + warmup + restore consent** — L1409-1497.
- WHAT: on `threadId` OR `switchGeneration` edge: clears `restoredThreadId`, bumps `restoreToken`, resets both auto-load gates, clears the local prune-shift flag, then branches: (a) thread→thread: `stick.setEscapedFromLock(true)` (freeze), `armWarmupWithReset()` (isWarm=false BEFORE first paint), `stick.armRestoreSnap()` (one-shot consent, must run AFTER the defensive escape because escape clears the arm — order inside the block is load-bearing, L1464-1476); (b) null→thread (placeholder materialized): `skipWarmup()` + `markAtBottom()` (4d7e6d11 — first-send latency); (c) thread→null (draft): `markAtBottom()` (6bf945ce — chip over empty draft).
- WHY: warmup-in-pre is the "flaky-fix bug" (L1448-1463): attach() can't re-arm because els don't change across switches. Consent gate is the seq-509 stale-restore investigation (53126e01 "Stabilize virtualized plan rows" introduced `armRestoreSnap`/`lastEffectPreAt`). switchGeneration discriminator is the revert-to-checkpoint stuck-at-top bug (comment L236-246). Deliberately does NOT snapshot the outgoing thread here — pane.items already mutated (L1401-1408); scroll-event saves are the snapshot.
- COUPLING: controller `setEscapedFromLock/armWarmup/armRestoreSnap/skipWarmup/markAtBottom`; runs in pre phase before the {#key} DOM flush — ordering guaranteed only by Svelte's pre/post phases.
- VERDICT: **SIMPLIFY** — the escape-then-consent dance exists because restore is split across two effect phases with other writers possibly interleaving; a single switch transaction owned by the resolver collapses defensive-escape + consent into one atomic state change. The generation/threadId edge detection itself must survive in some form.

**B2. Restore $effect** — L1499-1557, `restoreToBottom` L1590-1658, `restoreAnchor` L1660-1739.
- WHAT: after DOM flush, when items (or empty+settled) and listRef are ready, sets `restoredThreadId` and branches on snapshot kind synchronously: bottom → `forceStick({reason:'restore'})` (consent-gated) + `saveScrollSnapshot()` + one thread-guarded rAF `notifyContentMaybeGrew()` settle pass; empty → `markAtBottom()` + write bottom snapshot; anchor → async `restoreAnchor` (pause lease, tick, `pane.loadUntilItem`, tick, `runExternalScroll(scrollToIndex(idx,{align:'start',offset:-offsetTop}))`, token-checked at every await, falls back to `restoreToBottom`).
- WHY: synchronous bottom branch beats virtua's deferred scroller attach (`tick().then(observe)`) reading the outgoing thread's carry-over scrollTop — the long-thread flash (comment L1514-1523; e00723f8, 72a05f87). Single-writer rule L1574-1578: never pair scrollToIndex+markAtBottom (oscillates). The trailing rAF covers padding-only changes that don't fire contentRO (W3C content-box) — c85dc226. Doc §Warm-Up L188-194.
- COUPLING: `forceStick/markAtBottom/runExternalScroll/pauseAutoScroll/notifyContentMaybeGrew`; `restoreToken` staleness; reads `groupedNodes`/`revealedNodes` (reveal-gate-corrected last index, L1591-1595); heavy gated trace records.
- VERDICT: bottom/anchor branching **KEEP**; the rAF settle pass and the dual-writer avoidance commentary are exactly the class of thing a per-frame resolver absorbs — **SIMPLIFY** under target architecture.

**B3. Restore bookkeeping state** — `restoredThreadId` L221, `lastEffectPreAt` L229 (diagnostic-only, trace-gated), `scrollSnapshotThreadId` L234, `scrollSnapshotSwitchGeneration` L246, `restoreToken` L250.
- VERDICT: `lastEffectPreAt` + its msSinceEffectPre trace **DELETE-CANDIDATE** post-re-architecture (it instruments the seq-509 bug class the consent gate already defends); the rest KEEP until the switch transaction is unified.

## C. Warm-up / reveal gating

**C1. hideContentForWarmup** — L646-649, applied at L1997 (`visibility:hidden` on contentEl).
- WHAT: hides content while `!stick.isWarm && pane.items.length > 5`.
- WHY: estimate→measure cascade shifts viewport hundreds of px on fresh mounts (c85dc226). Doc §Warm-Up And Restore.
- COUPLING: `stick.isWarm`; the >5 small-thread bypass is NOT in the doc (minor drift, see Q5).
- VERDICT: **KEEP** — post-fix it guards only first visits (replay eliminates revisit cascades, doc L179-186), still needed.

**C2. Markdown-settled quiet-context signal** — `anyMarkdownSettledSinceArm` L426, `markMarkdownSettled` + context L454-459, `armWarmupWithReset` L461-464.
- WHAT: rising-edge-once boolean fed to controller `quietContextSignal` to shorten warm-gate quiet window (QUIET_MS→SETTLED_QUIET_MS) once any ChatMarkdown fired `onsettled`.
- WHY: acc42fb8 "Adaptive warm-gate quiet window". Doc L150-173 (geometry-stability gate lives controller-side).
- COUPLING: context consumed by ChatMarkdown descendants; `stick.notifyQuietContextSignalChanged()`.
- VERDICT: **KEEP** — orthogonal to the settle-flicker fix; latency optimization for first-visit reveal.

## D. virtua size-cache replay (thread-switch flicker)

**D1. virtuaReplayCache resolve ($effect.pre)** — L212-219, L1364-1372; **capture** `maybePersistVirtuaSizeCache` L1299-1355 (size-gated on `getScrollSize()`), key `currentVirtuaSizeKey` L1299-1305 (width + `timelineStructureSignature(revealedNodes)` + `pane.expansionSignature()`); **isWarm rising-edge capture** L1387-1393; `lastPersistedScrollSize` reset on threadId edge L1368.
- WHAT: replays previous visit's measured sizes into the next {#key} Virtualizer mount (`cache={virtuaReplayCache}` L2004) so revisits mount at final height; captures ride `saveScrollSnapshot` co-location (L1237-1243) + the warm rising edge (guarantees settle capture for never-scrolled threads).
- WHY: 8bf8b97f "eliminate cold thread-switch scroll flicker at the source". Doc §Row And Payload State L417-471 (including the superseded timelineRevision-keyed version that never matched).
- COUPLING: must resolve in $effect.pre because virtua reads `cache` once at construction; TypeError-teardown swallow around virtua reads only (L1338-1351, deliberately NOT around key construction); `rowGeometryWidth` persists across switches (component not keyed on threadId) so it carries the correct width to the lookup.
- VERDICT: **KEEP** — independent of the settle-flicker mechanism; it kills the revisit cascade the warm gate can only mask.

## E. Snapshot save (position)

**E1. saveScrollSnapshot / saveScrollSnapshotForThread** — L1233-1279; triggers: every virtua scroll frame (L1135), scroll-end (L1148), restore completion (L1629, 1735), prune restores (L562, 576), load handlers (L1760, 1788, 1811, 1828), onDestroy (L1872).
- WHAT: `{kind:'bottom'}` when `stick.isAtBottom`, else `captureTimelineAnchor` (virtua `findItemIndex`/`getItemOffset` — no DOM) into `threadScrollSnapshots`. Guarded by `restoredThreadId === threadId` (pre-restore scroll events ignored); TypeError teardown swallow.
- WHY: back-button/switch position return, doc §Owners L17-18. dc0d644c/e00723f8 era.
- COUPLING: co-located D1 capture trigger; `stick.isAtBottom` read.
- VERDICT: **SIMPLIFY** — per-frame saves at 60Hz under a spring are cheap-but-busy (Map set + two virtua lookups per frame); a resolver could snapshot on scroll-end + switch-out only. Correctness note (L1401-1408): switch-out capture can't happen in $effect.pre, so per-scroll freshness is currently load-bearing.

## F. Window prune anchor transaction (streaming/settle prune)

**F1. preserveTimelineWindowAnchor** — L601-629 + helpers L502-599: `captureTimelineWindowAnchorIntent` (L508: intent = `isSticky || (!escaped && isAtBottom)`; else anchor via `captureTimelineAnchor`), `canApplyPruneWithoutDroppingAnchor` (L526: veto → pane defers prune), `markTimelineWindowPruneShiftForOneFlush` (L540: local `shift=true`, reset via `tick().then` token), before/after `timelineNodeKeys()` compare with `isPureKeyedHeadDrop` (L616-622), `restoreTimelineWindowAnchorAfterPrune` (L579: tick, generation+token check, bottom → `scrollToIndex(end)`+`markAtBottom` under `runExternalScroll{preserveIntent}`, else `scrollToIndex(start,-offsetTop)`), pause lease around the whole transaction.
- WHY: mid-stream head-drop under bottom pin = blank flash (incident 2026-06-10; 21a71a5e); the rendered-suffix shift check is c4b6b922 (prune through a Read group/filter must not splice virtua's cache against the wrong row set) — doc §Load Paging L92-103, §Live Window Bounds.
- COUPLING: called BY the pane through the published controller (A2); dual shift ownership: `virtualizerShiftAtHead = pane.pendingTimelineShiftAtHead || timelineWindowPruneShiftAtHead` (L324-328); `clearTimelineWindowPruneShift` on switch (L1447).
- VERDICT: **KEEP the semantics, SIMPLIFY the choreography** — virtua's shift contract is inherent, but the pause-lease + token + tick + two restore paths is a hand-rolled transaction the resolver should own; the two shift sources should merge into one.

**F2. Deferred prune retry effect** — L636-640: retries `pane.retryDeferredRecentWindowPrune()` when sticky returns.
- WHY: 21a71a5e; doc L121-124 ("retries when bottom intent returns instead of re-asking on every append").
- VERDICT: **KEEP** (small, contract-matching).

## G. Load paging

**G1. Auto-load triggers** — `maybeAutoLoadOlder` L1157-1177, `maybeAutoLoadNewer` L1184-1214 (called per scroll frame from `handleVirtuaScroll`, one direction per frame L1136-1141), gates at L251-252, zone constants L135-140.
- WHY: ce10501e; direction-agnostic gate + full-cursor progress guard per doc L39-54.
- COUPLING: `restoredThreadId` fed into gate (no auto-load pre-restore); newer-edge reads `scrollEl.scrollHeight/clientHeight` directly (Q4); virtua `findItemIndex` inside zone thunks.
- VERDICT: **KEEP**.

**G2. handleLoadOlder** L1752-1770 / **handleLoadNewerAuto** L1804-1820 / **handleLoadNewer (manual)** L1774-1797 / **jumpToLatest** L1822-1829.
- WHAT: older: pause lease + `setEscapedFromLock(true)`, await `pane.loadOlder()` (shift compensation is virtua-native — deliberately NO re-anchor, L1744-1751), snapshot, disarm gate (switchGeneration-guarded). Newer-auto: same minus escape. Newer-manual: scrollToIndex(end) under `runExternalScroll` + restoreToken guard. jumpToLatest: `loadRecentTail` + `forceStick({reason:'user'})`; also the ScrollToBottomButton action (L2188).
- WHY: 1816893b rework, 27f7a7da (virtua shift), doc §Load Paging.
- COUPLING: `pane.pendingTimelineShiftAtHead` set store-side, bound at L2005.
- VERDICT: **KEEP** — post-fix these are clean; the pause-lease-per-operation pattern is another resolver-collapsible seam but not broken.

## H. Live-follow structural nudge (spring for tool rows)

**H1. activeTurnStructuralSignature** — L355-399 (tail 5 node keys + tail 5 item identity keys + thread/turn ids; tracks only `timelineRevision`+`revealBoundary`, reads arrays under `untrack`).
**H2. The nudge effect** — L777-840 + `notifyAfterActiveTurnStructuralChange` L744-758 + baselining state L721-732.
- WHAT: on genuine tail-row appearance during an active turn: `stick.markStructuralContentPending()` (one-shot spring eligibility for the coming RO growth) then tick+rAF → `stick.notifyLiveContentMaybeGrew()` (token + thread + signature + still-active-turn checked). Re-baselines WITHOUT marking across switch/reload/initial-load via three load-bearing disjuncts (`generationChanged || loading || loadingChanged`, L801-819 — cache-hit, in-window, and cache-miss closing edge respectively).
- WHY: thinking rows tail-pin internally so outer row height doesn't grow; the next top-level row's first bottom target was missed by contentRO timing (ad4b2e1a "Fix sticky follow after thinking rows"). The re-baseline guards bug-report-20260622T041049Z (visible multi-hundred-px spring on switch into streaming thread; 8bf8b97f). Doc §Live Content Animation L317-334.
- COUPLING: heavy — the most ordering-sensitive machine in the file: signature derivation untrack discipline, effect-vs-load flush ordering, tick+rAF scheduling, token invalidation from B1 (no — its own token; B1 doesn't bump it, the effect self-baselines via generation).
- VERDICT: **DELETE-CANDIDATE under the resolver, KEEP until then** — this whole machine compensates for "contentRO timing can miss the first bottom target"; a per-frame resolver that sees appended-content vs measured-height each frame makes both the mark and the deferred nudge unnecessary. It is also worth re-testing whether the pre-fix buffer churn contributed to the missed-first-target symptom (the mechanism predates the fix).

## I. Animation mode

**I1. animationModeForScroll / latchedSpringMode** — L437-439, wired at L442.
- WHAT: `spring` for SPRING_MODE_HOLD_MS (500ms) after `pane.lastLiveContentAt`, else `instant`.
- WHY: daf6ffe4 "Key chat spring scroll on live content, not the turn signal"; doc §Live Content Animation L305-316; hold > controller sentinel retain (350ms) invariant.
- COUPLING: pane stamps; controller consumes per write.
- VERDICT: **KEEP** — content-keyed latch is a product decision, not churn-era scaffolding.

## J. Row-geometry reservation & width

**J1. rowGeometryReservation** — L188-195 (action factory bound to pane height cache), applied per row at L2036-2039 via `use:rowGeometryReservation`, key from `timelineRowGeometryKey(node, item, rowGeometryWidth, expansion, shellSignature)` L2016-2022, `rowGeometryShellSignature` L268-278.
- WHAT: temporary `min-height` while a remounted row re-measures; per-row cache on pane keyed by row key + content signature + width.
- WHY: c5c79d5a "preserve row geometry across remounts" — built while the misclassified-write buffer churn was remounting ~39 rows per settle beat. It was the settle-flicker **amplifier** (Math.round residues; f42dc6e6 made it fractional).
- COUPLING: `rowGeometryWidth` (J2) must be the identical content-box the remember path measures (the wrapper must stay padding/border-free, L2042-2056); `[data-row-geometry-content] display:flow-root` margin containment (4b3759a1, L2057-2071); trace hook `timeline.row.geometry`.
- VERDICT: **UNKNOWN → re-evaluate empirically** — post-fix, streaming no longer remounts the buffer, so the reservation only serves genuine scroll-away/scroll-back remounts, where virtua's own size cache already holds the slot height (only inner content collapse remains). This is the single largest built-during-churn mechanism; measure whether removing it reproduces any visible artifact on a patched build.

**J2. rowGeometryWidth observer effect** — L210, L694-719.
- WHAT: content-box width from `observeScrollSurfaceContentWidth` RO ONLY; depends on `scrollEl` alone; 0 until first delivery.
- WHY: idle width-oscillation feedback loop, incident 2026-06-26, effect re-ran ~33k times/30s (a5a5d032, 4a4f07ed). Doc L477-485.
- COUPLING: feeds J1 keys and D1's size-cache key width.
- VERDICT: **KEEP** (as long as any width-keyed cache exists); the "never seed synchronously" rule must be preserved verbatim.

## K. Row-UI state pruning (scroll-coupled memory bound)

**K1. currentVisibleTimelineRange** L1041-1061 (virtua cached geometry only — explicit no-DOM comment), `scheduleRowUiStatePrune` L1066-1073 (tick+token), `pruneOffscreenRowUiState` L1075-1105 (signature short-circuit), trigger effects L1113-1124 (structural + listRef/scrollEl) and scroll-end (L1149). `IS_TEST` skip L1067.
- WHY: b61b5a01 "Bound timeline memory for long threads"; buffer constants L102-103.
- COUPLING: reads `revealBoundary`/`timelineRevision`; prune cadence deliberately never per-frame (L1107-1112).
- VERDICT: **KEEP** — memory bound, not a scroll-write mechanism; only interacts with scroll via scroll-end trigger. Fold/evict re-hydration rides pane registries + timelineRevision, not this component.

## L. Virtua callbacks & mount plumbing

**L1. handleVirtuaScroll / handleVirtuaScrollEnd** — L1134-1150: snapshot save per frame + auto-load (one direction/frame); scroll-end adds prune. Explicitly NOT intent (controller's native listener owns intent, L1129-1132).
**L2. {#key pane.threadId} Virtualizer remount** — L1999-2011: decision documented at L1986-1994 (internal size store must reset per thread; D1 replay compensates). Props: `cache`, `shift={virtualizerShiftAtHead}`, `getKey=timelineNodeKey`, `itemSize=56`, `bufferSize=1800` (L89-97), `ssrCount={IS_TEST ? 100_000 : undefined}` (L171-176, 80d756bc; happy-dom zero-geometry workaround).
**L3. Empty-timeline branches** — L1943-1954 template; `markAtBottom` empty restores (B1 branches b/c, B2 empty branch L1608-1618).
- VERDICT: **KEEP**; the {#key} remount is load-bearing today but is the primary casualty/beneficiary of an eventual merged virtualization (remount-per-switch + cache replay could become "swap dataset in place").

## M. notifyHostLayoutSettled retry ladder

**M1.** L466-500: pane reorder / host layout change → up to 2 rAF retries waiting for `listRef`, then under `runExternalScroll{preserveIntent}`: sticky → `scrollToIndex(end)`+`markAtBottom`; empty+sticky → `markAtBottom`; escaped → re-issue `scrollTo(getScrollOffset())` (nudges virtua re-measure without moving), or `notifyContentMaybeGrew`.
- WHY: 650cf74f/447f6380 pane-reorder reconciliation (Virtualizer remounts when a pane moves in layout; listRef momentarily unbound).
- COUPLING: called by pane through published controller (A2); `hostLayoutRetryToken` invalidation incl. onDestroy.
- VERDICT: **SIMPLIFY** — the rAF-retry-until-listRef-exists ladder is a workaround for not having a mount-settled signal; a resolver with a per-frame pass makes "do X once geometry exists" a queued intent instead of a retry loop.

## N. Scroll-to-item routing

**N1.** `scrollToItem` L1835-1857 + nonce effect L1859-1866 + `targetFlash` L253.
- WHAT: `pane.loadUntilItem(id)` → token checks → `findTimelineNodeIndex` (reveal-aware resolve, falls back to containing visible node for subagent hits) → `runExternalScroll(scrollToIndex(center))` → flash.
- WHY: f51c47aa (multi-click jump bug → token discipline); doc §Search.
- VERDICT: **KEEP**.

## O. Diagnostics/trace installs (7 effects)

L846-852 render trace; L854-868 memory diagnostics; L876-967 `captureTimelineGeometry` + probe install (26db4cba, dev-only Ctrl+Shift+B; direct DOM reads by design); L974-986 listRef bind trace (virtua deferred-attach seam); L993-996 row-resize trace; L998-1009 margin-divergence oracle (`timeline.margin.diverge`, must stay silent post-4b3759a1); L1011-1021 reasoning tail-jump oracle. All gated on `isUiRenderTraceEnabled()` except memory/probe installs (dev-only inside helpers).
- VERDICT: **KEEP through the re-architecture** (they are the regression oracles you'll want while rebuilding), then re-home; the two "oracle" observers (L998-1021) are candidates to convert into browser-test-only assertions once the new architecture lands.

## P. Layout seams (template)

- Composer clearance: `padding-bottom: calc(var(--composer-height) + 16px)` on scrollEl L1935 (padding-only changes invisible to contentRO — why ChatView's composer RO must notify explicitly; giant comment L1880-1927).
- `overflow-anchor:none` L1933, `scrollbar-gutter: stable both-edges` L1934 (329d89ba; WebKitGTK verified), `overscroll-behavior-y: contain` L1932, paint-only top fade mask L121-126/L1936-1937 (6e347877 — deliberately geometry-invisible).
- ScrollToBottomButton visibility `!stick.isAtBottom || pane.hasMoreNewer` L2188 (deliberately not `!isSticky` — pause leases would flash the chip).
- VERDICT: **KEEP** — all doc-matching and geometry-inert by construction.

---

# Answers

**1. Load-bearing $effect orderings and how they're guaranteed.**
- **Pre-phase vs post-phase (Svelte-guaranteed):** B1 (L1409) and D1-resolve (L1364) MUST run before the {#key} DOM flush — warmup must be armed before first paint (else the flaky "lands wrong, jumps to correct", L1448-1463) and virtua reads `cache` exactly once at construction (L1364-1372). The restore $effect (L1499) MUST run post-flush so `listRef` is bound. Guarantee: effect phase only — nothing else enforces it.
- **Order WITHIN B1:** `setEscapedFromLock(true)` before `armRestoreSnap()` (escape clears the arm; L1464-1476). Guarantee: statement order in one block.
- **Between the two $effect.pre blocks (L1364 vs L1409):** currently independent (no shared state), guaranteed by declaration order if that ever changes — fragile and undocumented.
- **Restore-$effect synchronous bottom branch:** no `await` before `forceStick` — races virtua's `tick().then(observe)` deferred scroller attach (L1514-1523). Guarantee: code shape only; any added microtask reintroduces the long-thread flash.
- **Prune shift flag same-flush semantics:** `markTimelineWindowPruneShiftForOneFlush` sets synchronously inside `operation.run()` and resets via `tick().then` token (L540-547) — virtua must read shift in the same flush as the data mutation. Guarantee: token + tick discipline.
- **H2 re-baseline before mark:** the generation/loading disjuncts must be evaluated in the same flush the signature changes; guaranteed by tracked deps (`switchGeneration`, `loading`) flipping in the same reactive batch as the item mutations.
- **attach ordering on first mount:** B1's `skipWarmup()` (branch b fires on first mount too) is immediately overridden by attach()'s warmup arm in the post phase — accidental but currently correct; a reorder would leave first mounts unhidden.

**2. State MessageTimeline owns that arguably belongs elsewhere (and vice versa).**
- Belongs in controller/store: the restore state machine (`restoredThreadId`, `restoreToken`, `scrollSnapshotThreadId/SwitchGeneration`) — it's scroll-intent lifecycle, not rendering; the defensive-escape + consent choreography already half-lives in the controller (`armRestoreSnap`). Snapshot-save timing (E1) could live with `threadScrollSnapshots`. The prune anchor transaction (F1) is window policy executed in the component because it needs listRef — under merged virtualization it's resolver-owned.
- Split ownership smells: virtua `shift` has two owners (pane `pendingTimelineShiftAtHead` + local `timelineWindowPruneShiftAtHead`, merged L326). The published controller handle carries timeline methods (A2) — pane→timeline calls masquerade as controller calls.
- Correctly placed: `rowGeometryWidth` (component-local, feeds only component caches), reveal/derivation pipeline, auto-load gates (component-local UX policy), the diagnostics.

**3. Flows that collapse under a single per-frame resolver owning all writes.**
Directly collapsible: B2's trailing rAF settle pass; M1's rAF retry ladder; H1+H2 entirely (mark + tick/rAF nudge become "resolver sees new tail content this frame"); F1's pause-lease/token/tick transaction (becomes a resolver op: "apply mutation, restore anchor, same frame"); the dual-writer avoidance rules (L1574-1578) and `runExternalScroll` wrapping (every scrollToIndex call site) because there is only one writer; per-frame snapshot saves (E1) become resolver-scheduled; the defensive-escape+consent split in B1 becomes an atomic switch transaction. Controller-side (sibling scope but visible from here): the virtua write gate, anchor-redirect, magnitude carve-out, and idle deadband all exist because virtua is a second writer — merged virtualization removes the writer, and A1's marking hook disappears with it.
Not collapsible: warm-up hide (C1), size-cache replay (D1), auto-load gating (G1), window-prune veto policy, animation-mode latch (I1), memory pruning (K1).

**4. Direct DOM geometry reads (vs controller/virtua) and why.**
- `maybeAutoLoadNewer` L1199-1203: `scrollEl.scrollHeight/clientHeight` for bottom-edge distance — virtua's handle exposes offset/viewport but the code predates trusting `getViewportSize` here; guarded by the `hasMoreNewer` pre-check so it's off the common path. Only production-path forced-layout read in the file.
- `captureTimelineGeometry` L897-960: scrollTop/scrollHeight/client*, `offsetHeight`, inline min-height — dev-only probe, deliberately direct to bypass `__stickState` last-writer-wins.
- Trace records (B1/B2/L974-986): `scrollEl.scrollTop` etc. — all `isUiRenderTraceEnabled()`-gated.
- Everything else is deliberate indirection: `currentVisibleTimelineRange` uses virtua cached geometry with an explicit no-clientHeight comment (L1044-1050); anchor capture uses virtua lookups; width comes only from the async RO (J2).

**5. Doc drift (frontend-scroll.md vs code).**
The doc is remarkably current (updated with the f42dc6e6 entry, L225-240). Residual gaps:
- **Warm-up hide threshold:** doc L146-147 says chat hides contentEl while `!stick.isWarm`; code adds `pane.items.length > 5` (L648-649) — small threads never hide. Undocumented.
- **armWarmup vs armWarmupWithReset:** doc L188-189 names `stick.armWarmup()`; code calls the wrapper that also resets the quiet-context signal (L461-464). The reset semantics are only in code comments.
- **notifyHostLayoutSettled and the widened controller handle:** doc §Owners forbids new owners and describes ThreadPane as a registration slot, but doesn't mention that the slot carries `notifyHostLayoutSettled`/`preserveTimelineWindowAnchor` timeline callbacks (A2) — the mechanism is doc'd in §Live Window Bounds by behavior only.
- **Diagnostics rule vs component reality:** doc L548-552 forbids fixing regressions with rAF/second observers/length-watching effects, yet the component's sanctioned machinery includes three rAF mechanisms (B2 settle pass, M1 ladder, H2 nudge) and several structure-watching effects — grandfathered tension the re-architecture should resolve rather than a doc error.
- **jumpToLatest / loadRecentTail** (G2) and the ScrollToBottomButton visibility rule are undocumented in the doc's §Intent write list (it lists `forceStick({reason:'user'})` generically).
- No claims found that the code outright contradicts.

Cross-cutting planning note: the mechanisms built **while the churn existed** (candidates for empirical re-test on the patched build) are J1 (row-height reservation, the flicker amplifier), H1/H2 (structural nudge), and the M1 retry ladder; the mechanisms that are **independent of the churn** and should survive any rewrite are D1 (size replay), C1/C2 (warm-up), F1's veto policy, G1/G2, and the layout seams in P.

result: Complete inventory of MessageTimeline.svelte's ~30 scroll mechanisms delivered with lines, provenance commits, coupling, and post-fix verdicts — headline findings: row-geometry reservation (J1) and the structural live-follow nudge (H1/H2) are the biggest churn-era re-evaluation candidates, virtua `shift` has two owners, the restore split's ordering is guaranteed only by Svelte effect phases, and frontend-scroll.md has only minor drift (warm-up hide threshold, armWarmupWithReset, widened controller handle).


---

## A3 — Geometry caches (5 layers)

Investigation complete — all five layers read end-to-end, consumption sites traced, provenance harvested from git and the analysis doc. Structured inventory follows.

---

# GEOMETRY CACHING/PERSISTENCE INVENTORY (post-f42dc6e6)

## 0. Commit timeline (provenance spine)

| Date | Commit | What |
|---|---|---|
| 2026-04-26/05-14 | e87081c0 → dc4d931c → bc4a4df9 | `threadScrollSnapshots.ts` born + cleanup (position-only, stable since) |
| 2026-06-19 | c5c79d5a | Per-row min-height reservation + row-height cache born ("preserve row geometry across remounts"; "keeps transient renderer or virtualizer remount height collapses from perturbing bottom-follow"). At this time the doc explicitly **prohibited** whole-store virtua cache replay (see c5c79d5a's diff to frontend-scroll.md: "Do not replay virtua row-size caches across thread switches") |
| 2026-06-23 | 8bf8b97f | `threadVirtuaSizeCache.ts` born — keyed CacheSnapshot replay overturns that prohibition; fixes the thread-switch estimate→measure flicker at the source |
| 2026-06-23 | 4a4f07ed | Floor bug: cache keyed by laggy prop width stranded timeline above composer after width reflow → remember under RO-measured width |
| 2026-06-29 | a5a5d032 | Floor bug: dual width source (border-box gBCR + content-box RO) → infinite idle width-oscillation, ~33k effect re-runs, ~660MB string churn (incident 2026-06-26) → `observeScrollSurfaceContentWidth`, one box one async source |
| 2026-07-01 | db77ba0d, 4b3759a1 | Adjacent strands (idle DPR deadband; margin containment via `[data-row-geometry-content] { display: flow-root }`) |
| 2026-07-01 | f42dc6e6 | **The fix.** virtua patch `markProgrammaticScroll()` ends per-beat buffer-drop remount churn; fractional heights end-to-end; settled-height gate added; signature builders split into `timelineRowGeometrySignature.ts` (file is *uncommitted/untracked* on this branch) |

Load-bearing verdict from `docs/architecture/settle-flicker-analysis.md:29-31`: *"The pre-floor build had identical churn but no floors — invisible. The floors are the **amplifier**; the misclassified-write buffer churn is the **disease**."*

## 1. Layered map

### Layer A — per-row min-height reservation ("floor") — `timelineRowGeometry.ts`
- **Stores:** nothing itself; a state machine per mounted row (`RowReservationState`, :64-81) that writes `row.style.minHeight` from Layer B and feeds Layer B from a per-row content-box ResizeObserver.
- **Keyed by:** the action param `TimelineRowGeometryKey` = (row key, signature, width, ownerItemIds), built per render at `MessageTimeline.svelte:2016-2022`, attached via `use:rowGeometryReservation` (:2039).
- **Written when:** action mount/update (`applyParams`, :195-302). Cold-mount floor written only if `!hasSettledHeight` (:249) and cache hit (:262-290). RO measure path (:304-341): below-floor ⇒ HOLD; at/above ⇒ remember + release. 750ms stale timer (:12, :291-301) force-releases + remembers the short height.
- **Read when:** every reactive param change — during streaming many times/sec for the tail row (value-equal fast path :206; settled gate :249 makes the hot path a `skip-settled` no-op).
- **Invalidated:** per-mount latch `hasSettledHeight` reset only on content-element swap (:154-181, today unreachable — "kept for a future conditional content wrapper", :170-176) or fresh action instance (real remount).
- **Failure it prevents (provenance):** (a) original: remount height collapse perturbing bottom-follow during the then-undiagnosed per-beat buffer churn (c5c79d5a); (b) virtua's flat 56px estimate on a fresh mount before first measure (`ESTIMATED_ROW_SIZE = 56`, MessageTimeline:89; comment :244); (c) the settled gate prevents a failure the floor itself causes — re-flooring a settled fractional row with a stale integer height, the 2-6px flutter (comment :240-248, pinned by `rowSettleFloor.browser.test.ts:78-121`).

### Layer B — row-height cache — `threadRowUiState.svelte.ts:233, 721-765`
- **Stores:** `Map<cacheKey, {key, signature, width, ownerItemIds, height}>`; height exact fractional (:898-901, f42dc6e6).
- **Keyed by:** `JSON.stringify([key, signature, width])` (:888-890). ≤3 widths per (key, signature) (:88, :755-765). **Supersession:** any write deletes all same-key different-signature entries (:742-746) — effectively *one* signature per row key, i.e. "last known height at ≤3 widths".
- **Written when:** every committed RO measure of a mounted row (`rememberMeasuredHeight`, geometry :353-376) — remembered under the RO-measured width, not the prop width (4a4f07ed).
- **Read when:** cold-mount floor lookup only (:262).
- **Invalidated by:** (1) `rowUiState.clear()` — **every thread switch** (`thread.svelte.ts` `installCacheOrFreshState`, ~:1918) and thread clear (~:2635); (2) `disposeItems` via `ownerItemIds` — live-window prune drops (:610), `removeItemById` (:3256), `removeItemsFromTurn` (:3290); (3) `pruneRowUiState` retention window (MessageTimeline :1075-1118; retention = visible range ±96 nodes + last 64 + active-item nodes, `timelineRowUiRetention.ts:85-107`); (4) synchronous per-item invalidation on payload-expansion toggle (`onExpandedChange`, :307 → :568-570) and diff-card toggle (:690); (5) supersession + width cap above.
- **Failure prevented:** none directly — it exists solely to feed Layer A.

### Layer C — virtua CacheSnapshot replay — `threadVirtuaSizeCache.ts`
- **Stores:** virtua's opaque measured-size snapshot (`getCache()`, runtime shape `[sizes[], estimate]`) per thread; LRU, 50 entries (:98).
- **Keyed by:** validity stamp {width, structureSig (`timelineStructureSignature` over `revealedNodes`), expansionSig (`rowUiState.expansionSignature()`, threadRowUiState :804-830)}; refuse-on-any-mismatch (:109-111). Display settings deliberately unkeyed — benign, warm-up-gate-masked residual (:51-67).
- **Written when:** `maybePersistVirtuaSizeCache` (MessageTimeline :1329-1355) — co-located with every scroll-position save + `stick.isWarm` rising edge (:1388-1393); gated on `getScrollSize()` change.
- **Read when:** `$effect.pre` on the threadId edge, before the `{#key pane.threadId}` remount consumes `cache` once at construction (:1364-1372, :1999-2004).
- **Invalidated:** key mismatch; thread delete (`threads.svelte.ts:52-53`); same-thread revert reswitch and item removal/truncate (`thread.svelte.ts:1776, 3260, 3294`); LRU.
- **Failure prevented (provenance):** thread-switch estimate→measure cascade — "lands right, flickers up a few hundred px ~1 frame, re-settles" (8bf8b97f). Safety premise: virtua no-ops re-measures matching restored sizes (verified against 0.49.1 core, header :10-18).

### Layer D — scroll position snapshots — `threadScrollSnapshots.ts`
- **Stores:** per thread, `{kind:'bottom'} | {kind:'anchor', itemId, offsetTop}`; LRU 100; equality-guarded writes (streaming writes one per virtua onscroll).
- **Written:** `saveScrollSnapshot` on virtua onscroll/onscrollend (MessageTimeline :1135, :1148), restore completion, load-older settle, window-prune restores (:562, :576).
- **Read (two consumers):** (1) MessageTimeline restore `$effect` (:1524-1556) → `restoreToBottom()` / `restoreAnchor()`; (2) **thread store slice selection** — `installCacheOrFreshState` uses the anchor's `itemId` as `sliceAnchorId` to decide which item window to fetch (thread.svelte.ts :1881-1884). It shapes data loading, not just scrollTop.
- **Failure prevented:** losing the user's reading position across thread switches; wrong initial window fetch.

### Layer E — virtua's internal size store (context)
Per-index measured sizes inside a mounted `<Virtualizer>`; survives row unmount/remount within an instance (this is what `getCache()` snapshots); reset by the `{#key pane.threadId}` remount; equal re-measures are filtered. Consequence: on a scroll-away/return remount, totalSize stays correct *unless the remounted DOM actually reports a different height* — the only gap Layer A can fill there.

### Plumbing — `observeScrollSurfaceContentWidth` (geometry :411-425) + `rowGeometryWidth` (MessageTimeline :210, :713-718)
Single async content-box width source (a5a5d032 invariant). **Dual-use:** feeds Layer A/B keys *and* Layer C's `currentVirtuaSizeKey().width` (:1301). Not deletable with the reservation.

## 2. Overlap analysis

- **Thread switch:** zero overlap in practice — Layer B is wiped by `rowUiState.clear()` before the incoming thread renders, so **floors contribute nothing on the thread-switch path**; CacheSnapshot replay owns it entirely. The two systems were sequential attempts (c5c79d5a Jun 19 under the replay prohibition; 8bf8b97f Jun 23 lifted it); the doc's old rationale for per-row reservation as the *only allowed* geometry persistence is gone.
- **Streaming beats:** pre-fix, floors re-armed on every buffer remount wave — that was the amplifier of the twitch, not a protection. Post-fix that entire duty cycle is dead: the hot path is `skip-settled` no-ops on the tail row.
- **Scroll-away/return:** virtua's internal store already keeps totalSize stable; floors add value only for content that transiently re-renders short on remount (async classes: attachment `<img>` layout, mermaid render; shiki/katex are module-singleton-cached and cheap per chat AGENTS.md). The 750ms timer comment (:10-12) names exactly "image/markdown remount settle".
- **Warm-up visibility gate** (`hideContentForWarmup`, MessageTimeline :643-650) overlaps both: it visually masks any cascade > 5 items on fresh mounts — the same degraded path Layer C falls back to on key mismatch.
- **Width reflow:** floors don't protect this path — historically they *broke* it twice (4a4f07ed strand, a5a5d032 oscillation). The ≤3-width bucket exists to serve panel-toggle round trips, but after a reflow the whole timeline re-measures live anyway.

## 3. Post-fix verdicts

| Mechanism | Verdict | Reasoning |
|---|---|---|
| Cold-mount floor + Layer B cache | **DELETE-CANDIDATE** | Remaining paths: scroll-away/return and prune/fold remounts of async-short-rendering rows only. No incident post-dates the fix; doc says pre-floor churn was invisible. The two shipped floor *bugs* (4a4f07ed, a5a5d032) exceed its documented saves. Needs one confirming trace/spike (see §5) before deletion. |
| HOLD path + 750ms stale timer | delete with floor | Pure floor lifecycle management. |
| Settled-height gate (`hasSettledHeight`) | delete with floor | Guards against the floor's own re-floor twitch; no floor, no twitch. `rowSettleFloor.browser.test.ts` goes with it. |
| `rebind`/content-swap re-arm | dead code today | Comment :170-176 admits the only trigger "today's unconditional content div never produces". |
| Signature builders (`timelineRowGeometrySignature.ts`) | delete with floor | Sole consumer is the reservation. Uncommitted file — cheap to fold. |
| Retention plumbing (`rowGeometryKeys` in `timelineRowUiRetention.ts:15,83,105,139`, prune loop threadRowUiState :627-630) | delete with floor | |
| Invalidation hooks (`invalidateTimelineRowGeometryForItem` :568-570, call sites :307, :690) | delete with floor | |
| `observeScrollSurfaceContentWidth` + `rowGeometryWidth` | **KEEP** | Feeds CacheSnapshot key width; a5a5d032 one-box/one-async-source invariant must survive. |
| `[data-row-geometry-content]` attr + `flow-root` CSS | **KEEP — trap** | Margin containment (4b3759a1) is keyed to this attribute and is independent of the reservation. Deleting the reservation must not drop the attr/rule; rename or re-anchor deliberately. |
| CacheSnapshot replay (Layer C) | **KEEP** | Evidence-backed (8bf8b97f), self-refusing key, bounded, post-fix-orthogonal. |
| Scroll snapshots (Layer D) | **KEEP** | Two consumers incl. slice-anchor selection; stable since April. |
| `timeline.row.geometry` trace taps | delete with floor (keep contentRO/margin oracles — different files) | `mountSeq` remount discrimination was the diagnostic that cracked f42dc6e6; its job is done, and the buffer-retention browser tripwire now guards the regression. |

## 4. Signature system churn profile

Invalidates a (key, signature, width) entry — per `timelineRowGeometrySignature.ts`:
- **Per streaming beat (hottest):** `item.updatedAt`, `item.summary.length` (:93-94) on the tail leaf; `latestChildSummary.length` (:80) on active group cards; `payloadMeta.length` fill (:97).
- **Per lifecycle transition:** `status` (:90), `descendantCount`/`loadedDescendantCount` (:78-79), wait_group child composition incl. up to 25 nested child signatures (:66-67).
- **Per user action:** group `expanded/collapsed` (:71-76); width (pane/panel resize).
- **Per index shift (most corrosive):** the shell signature (`rowGeometryShellSignature`, MessageTimeline :268-278) embeds `index === 0` position and per-index divider/pill/boundary membership. Load-older prepend and head prune shift every index ⇒ every surviving entry's signature goes stale ⇒ the cache misses **exactly on the prune/load-older remount paths where a floor would be most useful**. Combined with the supersession rule (one signature per key), the cache self-destructs on any structural shift.
- Stable components: threadId, id, kind, turnIndex, itemIndex, payloadId/kind, isRail.

If the floor is kept anyway: the signature could shrink to "settled content shape" (id, kind, status, summary.length, expansion, width) and shell decorations should be keyed by node key rather than index — under the post-fix regime the signature is only ever consulted at cold mount of a *settled* row, where updatedAt-level precision buys nothing and index-derived parts only defeat hits.

## 5. If the floors were deleted today, what visibly breaks?

- **Thread switch: nothing.** Cache is already empty there (cleared before render); Layer C + warm-up gate own the path.
- **Streaming pinned-at-bottom: nothing.** The remount churn is gone (f42dc6e6); the machinery is already inert (`skip-settled` no-ops).
- **Scroll-away-and-return / prune-restore / fold re-expand:** the only candidate. A remounted row containing an attachment image or mermaid diagram may report a transient short height; virtua would shrink then re-grow totalSize (its equal-size filter no longer no-ops). Above-viewport this is jump-compensated by virtua; at-bottom it would perturb the pin by the image's height for the settle interval. **UNKNOWN severity** — no post-fix capture demonstrates it, the pre-floor build was "invisible", and note the fold re-expand and payload-expand paths *already get no floor* (toggle invalidates the entry; group toggles change the signature). Concrete check before deletion: a trace/spike counting `hold` events (the only action that proves a floor did real work) during scroll-back over an image/mermaid-bearing thread.
- **Minimal covering system:** Layer C (thread switch) + Layer D (position) + warm-up gate (fresh mounts) + virtua's internal store (intra-instance remounts), with the residual async-content collapse fixed **at the content layer** if the spike confirms it — intrinsic dimension reservation on attachment images and a min-height placeholder on mermaid hosts (row-local, no cross-cutting state machine), which also covers the first-ever mount that no height cache can.

result: Geometry-layer inventory complete — 5 layers mapped with provenance; per-row floor system (and its cache/signature/retention plumbing) is a DELETE-CANDIDATE post-f42dc6e6 pending one async-content remount spike, CacheSnapshot replay and scroll snapshots are the keepers, with two deletion traps flagged (shared width observer, flow-root attr coupling).


---

## A4 — virtua dependency surface + core verification

All investigation complete and validated: the two virtua unit suites pass (6/6) against the installed patched package, confirming the semantics documented below. Read-only throughout — extraction artifacts live only in the session scratchpad. One method note: virtua ships `lib/core/index.js.map` with full `sourcesContent`, so I extracted the verbatim un-minified TypeScript (`store.ts`, `scroller.ts`, `cache.ts`, `resizer.ts`) to `/tmp/claude-1000/-home-rmurphy-repos-agent-overflow/c71db52f-53e2-4a38-9fdd-23242d84b845/scratchpad/virtua-src-*.ts`; core citations below use those (line numbers are the original v0.49.1 sources), with minified cross-references where patch (a) would land.

# 1. Complete dependency surface on virtua

**Mounts.** Exactly one production mount: `frontend/src/lib/components/chat/MessageTimeline.svelte:2000-2150`. One test-only mount: `frontend/src/test/integration/virtua-patch-fixtures/VirtuaBufferRetentionHost.svelte:55`. `ChannelView.svelte` (discussion) does not use virtua. `components/shared/VirtualList.svelte` is homegrown fixed-height math; `utils/diffSidebarVirtualizer.svelte.ts` is homegrown IntersectionObserver. Virtua is chat-timeline-only.

**Imports.**
- `virtua/svelte` `{ Virtualizer, type VirtualizerHandle }` — MessageTimeline.svelte:3; type-only also in `messageTimelineTrace.ts:6`, fixture host:12.
- `virtua/svelte` `type VirtualizerProps` — `threadVirtuaSizeCache.ts:69`, deriving `VirtuaCacheSnapshot = NonNullable<VirtualizerProps['cache']>` (deliberately avoids importing `unstable_core` into app code).
- `virtua/unstable_core` `{ createVirtualStore, ACTION_ITEMS_LENGTH_CHANGE }` — **test only**, `virtuaShiftCache.test.ts:24-27` (drives the real core store; intentionally version-coupled tripwire).
- Full handle surface is mirrored in `src/test/mocks/StubVirtualizer.svelte` (vi.mock target for `messageTimelineVirtuaMarking.test.ts:16`).

**Props passed** (MessageTimeline.svelte:2000-2011): `data={revealedNodes}` (grouped/revealed TimelineNode array, not raw items); `cache={virtuaReplayCache}` (consumed once at store construction; computed on the threadId edge in `$effect.pre` at 1364-1372); `shift={virtualizerShiftAtHead}` (326-328); `getKey={timelineNodeKey}`; `itemSize={56}`; `bufferSize={1800}` px; `ssrCount={IS_TEST ? 100_000 : undefined}`; `onscroll`; `onscrollend`. **Never used:** `keepMounted`, `startMargin`, `horizontal`, `as`, `item`.

**Handle methods used** (every call site):
- `markProgrammaticScroll()` (our patch) — wired at MessageTimeline.svelte:451 into the controller's `onBeforeScrollTopWrite`; fired at `useStickToBottom.svelte.ts:1055` before every controller scrollTop write.
- `scrollToIndex(idx, {align, offset})` — 494 (`end`), 558 (`end`), 571 (`start`, `-offsetTop`), 1733 (`start`, `-offsetTop`), 1786 (`end`), 1854 (`center`). Always inside `stick.runExternalScroll`, never `smooth`.
- `scrollTo(offset)` — 498 only (re-assert current offset in `notifyHostLayoutSettled`).
- `getScrollOffset()` — 498, 521, 1263; trace at `messageTimelineTrace.ts:107`.
- `getScrollSize()` — 916, 1343 (O(1) change-gate for size-cache persist); trace:108.
- `getCache()` — 917, 1345; `paneGeometryProbe` path reads the internal `[sizes[], estimate]` runtime shape defensively (917-928).
- `getViewportSize()` — 1050; trace:109.
- `findItemIndex(offset)` — 1053-1054 (visible-range for row-UI prune), 1172, 1208 (auto-load zone checks); `timelineScroll.ts:116` (anchor capture).
- `getItemOffset(index)` — `timelineScroll.ts:123` (anchor `offsetTop = getItemOffset(idx) - offset`).
- Unused: `getItemSize`, `scrollBy`.

**Implicit/behavioral dependencies** (the real coupling):
1. **shift cache semantics** — head-grow unshifts the size array, head-shrink front-splices; pinned against the real core by `virtuaShiftCache.test.ts` (all four cases + the "two single-ended length changes across separate flushes" rule). One-shot timing: shift must be true in the *same flush* as the length change (adapter `$effect.pre` Virtualizer.svelte:119-123) and cleared right after (`tick().then` MessageTimeline:543-547; `finally` blocks thread.svelte.ts:3079, 3115).
2. **CacheSnapshot replay** — consumed once at construction; `initCache` tolerates length mismatch (cache.ts:183-188, upstream #441); the resize handler's equal-size no-op filter (store.ts ACTION_ITEM_RESIZE) is *the* property making replay safe — documented reliance in `threadVirtuaSizeCache.ts:10-18`.
3. **ssrCount** — `$getRange` returns `_prevRange = [0, ssrCount-1]` until viewport is measured and a first scroll lands (store.ts:136, 192-198). The whole happy-dom test suite rides on this.
4. **`contain: size style; height: totalSize px`** on the container (adapter:182-192) — MessageTimeline's contentRO contract "contentEl.scrollHeight reflects totalSize exactly" (comment 1983-1994).
5. **`contain: layout style; position:absolute; top:offset`** item wrappers (ListItem.svelte:43-57) — trap collapsed-out row margins; the `[data-row-geometry-content]{display:flow-root}` app.css fix, the margin-divergence trace oracle (MessageTimeline:998-1009), and `rowMarginContainment.browser.test.ts` all exist to reconcile virtua's measured size with our content-box RO.
6. **`pointer-events: none` while `isScrolling`** (adapter:190). Because marked writes never latch a direction (mode=MANUAL skips the latch), streaming pins do *not* trigger it; only genuine user scrolls do, until scrollend + 150ms.
7. **`overflow-anchor: none`** on virtua's container (adapter:185) and, by contract, on our scrollEl (MessageTimeline:1933, chat/AGENTS.md rule).
8. **Hide-until-measured** — `$isUnmeasuredItem` → `visibility:hidden` (ListItem:50); the estimate→measure cascade this implies is why the warm-up hide (`hideContentForWarmup`, 642-649), the size-cache replay, and the per-row min-height reservations exist.
9. **Model-driven scroll events** — `onscroll(offset)` fires from the store on every ACTION_SCROLL with virtua's model offset; `onscrollend` after the 150ms debounce (scroller.ts:76-88). Both drive snapshot persist + auto-load + row-UI prune (MessageTimeline:1134-1150).
10. **Deferred scroller attach** — adapter `onMount` does `tick().then(assignRef)` (Virtualizer.svelte:98-112); the thread-switch restore choreography explicitly defends against virtua reading the outgoing thread's stale scrollTop in that window (MessageTimeline:1516-1523, 969-986).
11. **Teardown TypeErrors** — inner ref nulls mid-teardown; four defensive catch sites (MessageTimeline:925-928, 1057-1060, 1270-1278, 1346-1352).
12. **Second-writer arbitration** — the controller owns a `scrollTop` property-descriptor gate (useStickToBottom.svelte.ts:610-671 rationale, 2487-2671 setter) with a 7-branch decision tree over virtua's `$fixScrollJump` writes: controller-owned pass / runExternalScroll-window pass / not-warm-or-escaped-or-paused pass / **redirect-to-bottom** when DOM already pinned and write moves away / content-reflow pass / instant-mode pass / springToken==0 pass / >viewport-jump pass / else **suppress**. This encodes five documented regression cycles and is exactly the machinery patch (a) replaces.

# 2. Core internals (virtua 0.49.1, verified in source)

**$fixScrollJump.**
- Trigger: adapter `$effect` on every stateVersion change (Virtualizer.svelte:131-136) — after Svelte flushes the template (rows already at new offsets), before paint. One call per state bump.
- `store._flushJump()` (store.ts:243-247; **minified property name: bare `$`**): moves accumulated `jump` → `_flushedJump`, zeroes `jump`, returns `[jump, _scrollMode === SCROLL_BY_SHIFT]`.
- Observer `_fixScrollJump` (scroller.ts:156-167; **minified `.m()`**): if jump ≠ 0 → `updateScrollOffset(jump, shift, stillMomentumScrolling)`; then if `shift && viewportSize > totalSize`, manually dispatches ACTION_SCROLL because a clamped write fires no event.
- The element-scroller write (scroller.ts:334-358; minified inside `W.$observe`: `l[c] = R(e.$getScrollOffset() + t, i), o && u()`): an **absolute** write of `store.$getScrollOffset() + jump` — model value, not a DOM read (#475) — preceded by the iOS momentum overflow-hidden hack, followed by `cancelScroll()` when shift (#357). **This single line is the only internal compensation write in our configuration.**
- Jump accrual: `applyJump` (store.ts:168-182) adds to `jump`, or defers to `pendingJump` when (iOS WebKit && scrolling) or (frozenRange && manual smooth). `pendingJump` flushes into `jump` at scrollend (store.ts:460-463). Delta sources: (case 3) item resizes with per-item `shouldKeep` (store.ts:341-378): SHIFT mode → keep all ("keep distance from end"); manual+frozenRange → `index < frozenRange[0]`; natural not-scrolling-down → item entirely above viewport top (#385/#865); scrolling down or manual → `itemOffset < start && itemOffset+size < start+viewport` (#868). (case 5, shift=true) `jump = defaultItemSize × added` or `−Σ removed sizes` via `updateCacheLength` (cache.ts:205-234), also sets `_scrollMode = SCROLL_BY_SHIFT`.
- **Protocol invariant** for any applier: while `_flushedJump ≠ 0`, `$getRange` returns the frozen `_prevRange` (store.ts:201-205), and the next ACTION_SCROLL clears it and skips direction-latching via `isJustJumped = flushedJump && distance < |flushedJump|+1` (store.ts:267-284). Virtua re-syncs its model *from the resulting scroll event*. Writing a different value than requested is safe (model follows the event); writing **nothing** leaves the range frozen and the model diverged until some other scroll event — the precise mechanism behind the controller's documented "suppression desyncs virtua" regressions (useStickToBottom:2543-2551).

**Measurement pipeline.** ListItem registers with `resizer.$observeItem` (WeakMap element→index, resizer.ts:79-86); one shared RO batches entries per callback, skips `offsetParent`-less targets, emits one ACTION_ITEM_RESIZE batch (resizer.ts:54-73). Store filters equal-size entries (no-op, no state bump — load-bearing for cache replay), computes jump (above), `setItemSize` marks the prefix-sum dirty (`_computedOffsetIndex = min(...)`, cache.ts:43-53), bumps stateVersion with `shouldSync=true` → adapter `$derived` range/totalSize → template repositions rows → `$effect` → `$fixScrollJump`. **Median re-estimation** (`estimateDefaultItemSize`, cache.ts:137-170; run from store.ts:392-406) executes only when `itemSize` is *not* supplied (adapter passes `!itemSize` as `shouldAutoEstimateItemSize`, Virtualizer.svelte:44-50). We pass 56 — **the estimation path is dead code for us**, as is its buffer-suppression side effect in `$getRange` (store.ts:210).

**Scroll state machine** (verifying the patch's assumptions): ACTION_SCROLL(1): same-offset native ignored; direction latched `delta<0?UP:DOWN` only when `!isJustJumped && mode==NATIVE`; sync update when distance > viewport. ACTION_SCROLL_END(2): resets direction=IDLE, **mode=NATIVE**, frozenRange=null, flushes pendingJump; fired by a 150ms debounce extended while wheeling/touching (scroller.ts:76-88), with wheel-event inference for dropped scroll events (scroller.ts:107-130). ACTION_MANUAL_SCROLL(7): `mode=MANUAL`, no state bump — our marker; **cleared at scrollend, hence mark-before-every-write**. ACTION_BEFORE_MANUAL_SMOOTH_SCROLL(8): frozenRange, smooth-only, unused by us. `scrollToIndex`/`scrollTo` are async convergence loops (mark-manual → write → await UPDATE_SIZE_EVENT → re-resolve target → repeat, 150ms cancel timer; scroller.ts:188-271) — confirming why a sync marker had to be patched in.

**getRange/buffer/totalSize.** `visibleOffset = scrollOffset − startSpacer + pendingJump + jump`; buffer applied asymmetrically by latched direction — SCROLL_DOWN drops the entire backward buffer, SCROLL_UP the forward one (store.ts:213-218; the root cause the current patch defuses). Binary search over lazily-computed prefix sums with dirty-index invalidation (cache.ts:58-107); `computeRange` seeds from prevStartIndex (cache.ts:112-132). `totalSize = getItemOffset(cache, length)`. Public `getItemOffset` subtracts `pendingJump` (store.ts:154-160). `getScrollSize = max(totalSize, viewportSize)` (store.ts:82-84).

# 3. Patch (a): "bring your own scroll applier" — concrete assessment

- **An adapter-only patch is impossible.** The write lives in a closure created inside core `createScroller.$observe` (minified `W`); the Svelte adapter has no seam. Today's descriptor gate is an *external* interception of that write; a real applier needs a core hunk.
- **Hunk size/shape:** minimal version = add a mutable applier slot to the scroller returned by `W` (e.g. `$setScrollApplier(fn)`), and change the one line to `applier ? applier(target, jump, shift) : (l[c] = target)` where `target = R(e.$getScrollOffset() + t, i)`. ~4-6 lines in `lib/core/index.js`, mirrored in `index.cjs` (Vite uses the ESM file; the stale `.map` is cosmetic), plus ~10 lines in `Virtualizer.svelte` + `Virtualizer.type.d.ts` to expose it (prop or handle method). Optionally also route the scheduled-scroll write (`o[c] = e` in minified `C`) so the controller becomes the *only* writer including scrollToIndex — nice-to-have; those paths are already marked and wrapped in `runExternalScroll`.
- **Risk:** low-moderate. Mangled-name coupling is real but bounded: exported names (`createVirtualStore`, actions) and `$`-prefixed properties survive minification; `_`-prefixed ones are mangled (`_flushJump`→`$`, `_fixScrollJump`→`.m`, `_dispose`→`.h`). Every version bump re-rolls the patch against re-mangled code — strictly more brittle than the current unminified-adapter-only patch. The existing browser tripwire pattern (`virtua-patch-buffer-retention.browser.test.ts`) generalizes directly: a paired "applier receives the compensation delta / default write no longer fires" test is the drop-rule guard.
- **Applier contract** (from §2's protocol invariant): (1) receives an **absolute** target plus `(jump, shift)`; may write a different value safely — model re-syncs from the scroll event; (2) must ensure a scroll event eventually fires — if it decides not to write, it should poke ACTION_SCROLL with the current DOM offset (mirroring core's own clamped-shift fallback, scroller.ts:162-166), which would make "suppression" *safe by construction*, unlike today's gate which relies on the in-flight spring emitting the next write; (3) must act **synchronously in the same call** (it runs inside the adapter's post-flush pre-paint `$effect`; deferring paints one uncompensated frame); (4) `shift` must be honored verbatim (head-anchor preservation), whereas above-viewport resize jumps may be redirected to the bottom target while pinned — exactly the two intents the current descriptor gate has to *infer* from controller state and jump magnitude.
- **What it deletes:** the descriptor install/uninstall bookkeeping and 7-branch setter (useStickToBottom.svelte.ts:610-671, ~2445-2690), the happy-dom own-descriptor interplay, and the magnitude-heuristic carve-outs (>clientHeight jump, redirect epsilon). Post-patch, browser auto-anchor is already disabled by `overflow-anchor:none`, so the gate could go entirely — but that removal inherits five regression histories (bug-reports 20260524T200233Z, 20260622T041049Z, revert-to-top, cold-switch flicker, seq-509 family) and needs its own regression pass.
- **Upstreamability:** no existing issue/PR requests this (searched: "external scroll controller", "programmatic scroll", "stick to bottom" → only #584 closed built-in-stick-to-bottom request, #162 reverse-mode load-more, #216). It would be a novel proposal; it also subsumes `markProgrammaticScroll` (already flagged in frontend/AGENTS.md as an upstream candidate). Everything touched is `@internal` and deliberately mangled — expect a design conversation with the maintainer, not a quick merge. Plan on carrying the pnpm patch either way.

# 4. Replacement (b): purpose-built bottom-anchored virtualizer — sober scope

**What it must implement to match actual usage** (not virtua's generality): vertical, keyed rows, external scroll element; sizes array + lazy prefix sums + binary search (cache.ts:43-132 equivalent); px-buffer range window; batched RO measurement with equal-size no-op; head-shift (unshift/front-splice, ~25 lines, spec already written as `virtuaShiftCache.test.ts`); above-viewport resize compensation; snapshot take/replay (`[sizes, default]` — the validity-keying layer in `threadVirtuaSizeCache.ts` is ours already); hide-until-measured; render-all test mode (simpler than ssrCount); handle surface of 9 methods + 2 events (§1). Explicitly out: horizontal/grid/window modes, smooth scroll, keepMounted, startMargin, RTL/negative offsets, iOS momentum hacks, wheel-inference.

**Genuinely hard vs straightforward:**
- *Hard-ish:* (1) the resolve-step anchor policy — the one-resolve design (range + scrollTop from a single anchor: "bottom-pinned" or "anchor row + offset") eliminates virtua's entire jump/flushedJump/isJustJumped/frozen-range feedback protocol (store.ts:243-247, 267-284, 201-205) *and* most of the shouldKeep table (store.ts:341-378, which embodies #385/#865/#868/#380/#758) — but the escaped-reading case still needs correct keep/don't-keep for resizes above the anchor row; (2) subpixel/DPR — scrollTop writes round per DPR (store.ts comment ~line 275); equality must be epsilon-based (we already fixed an idle vibration from exactly this, commit db77ba0d).
- *Straightforward:* prefix-sum cache, binary search, range windows, RO batching, head-shift, snapshot, hidden-until-measured, test mode. Scroll-intent classification (direction/wheel/touch/scrollend) largely **already lives in useStickToBottom** — the bespoke engine wouldn't duplicate virtua's copy.
- Order-of-magnitude: a few hundred lines of engine + the row-mount component, against a consumer seam that is already narrow and heavily specified by our own tests (virtuaShiftCache semantics, buffer-retention tripwire, marking seam, scroll.test.ts, rowSettleFloor, geometry suites) — most of which test *our* contracts and convert into the new engine's spec.

**Virtua behaviors we actively fight that simply vanish:**
1. Direction-latched backward-buffer drop on unmarked writes → the pnpm patch + `onBeforeScrollTopWrite` wiring + two dedicated test suites exist solely for this.
2. `$fixScrollJump` as a second scrollTop writer → the descriptor gate's 7 branches and five regression cycles.
3. 150ms scrollend debounce resetting manual mode → mark-before-every-write; our snapshot/prune cadence tied to virtua's debounce rather than our own.
4. Estimate→measure mount cascade → warm-up visibility hide, size-cache replay, per-row min-height reservations, restore-consent choreography. A bottom-up resolve makes the pinned-case cascade invisible by construction (tail measures first; above-viewport error can't move the viewport); per-thread size caching stays useful for scrollbar stability but the hide gate can go.
5. Content-box measurement + `contain:layout` wrappers trapping collapsed margins → measure border-box and the entire flow-root fix class disappears (app.css rule, divergence oracle, browser test).
6. `pointer-events:none` during scrolling (hover/selection loss during user scroll) — omitted.
7. Deferred `tick().then` scroller attach racing thread-switch restore — attach synchronously; the race and its trace hooks go.
8. Teardown TypeErrors from a nulled inner ref — four defensive catch sites go.
9. Opaque minified core — every behavioral question stops needing version-coupled tripwire tests against internals.

# 5. Verifications requested

- **`shift`**: used *only* for head mutations — load-older prepend (`thread.svelte.ts:2866`), paged head-drop prune in loadNewer (`applyPagedPrune`, thread.svelte.ts:949-963), and MessageTimeline's one-flush hint when a streaming/settle prune is a pure keyed head drop (540-552 via `isPureKeyedHeadDrop`, 620-622). Never mid/tail; tail mutations explicitly set it false (thread.svelte.ts:3079, 3115).
- **`keepMounted`**: unused anywhere.
- **VList vs Virtualizer**: only `Virtualizer` from `virtua/svelte`; VList/WindowVirtualizer never imported.
- **horizontal/grid**: none; all `horizontal` grep hits are unrelated UI code.
- **Version/patch plumbing**: `virtua@0.49.1` pinned (`frontend/package.json:41`), patch registered in `frontend/pnpm-workspace.yaml:4`; `unstable_core` is an official package export (virtua package.json exports map) but is used only in the one test.

result: virtua dependency surface fully mapped (1 mount, 6 props, 9 handle methods, 12 implicit behaviors, shift=head-only, keepMounted/VList/horizontal unused); core verified from shipped source maps — patch (a) is a ~5-line core hunk on the single compensation write in `createScroller.$observe` with a strict applier contract (absolute target, sync same-call, must yield a scroll event, honor shift), no prior upstream request; replacement (b) needs ~9 subsystems of which only the anchor-policy resolve and DPR epsilon are hard, and 9 actively-fought virtua behaviors disappear.


---

## A5 — Consumer seams (all raw-scroll hits classified)

Investigation complete — all grep hits classified, every consumer read in full. Sanity check: the `createUseStickToBottomController` grep, the 28-line `scrollController` grep, and the 57-line raw-scroll grep are each fully accounted for below; no files were modified (read-only run, scratchpad only held grep dumps).

# Scroll-System Consumer Map (outside useStickToBottom.svelte.ts + MessageTimeline.svelte)

## 0. Controller public surface (baseline for the matrix)

`UseStickToBottomController` (useStickToBottom.svelte.ts:317-494): props `isSticky`, `isAtBottom`, `escapedFromLock`, `isWarm`; methods `pauseAutoScroll`, `notifyContentMaybeGrew`, `notifyLiveContentMaybeGrew`, `markStructuralContentPending`, `preserveScrollAnchor`, `attach`, `detach`, `forceStick`, `armRestoreSnap`, `markAtBottom`, `animateScrollTo`, `runExternalScroll`, `stopScroll`, `setEscapedFromLock`, `armWarmup`, `skipWarmup`, `notifyQuietContextSignalChanged`. Options (:496-565): `animationMode`, `quietContextSignal`, `onBeforeScrollTopWrite`.

`PaneScrollController` (threadPaneShared.ts:139-152) is the pane-facing narrowing: `pauseAutoScroll`, `notifyContentMaybeGrew`, `notifyLiveContentMaybeGrew`, optional `notifyHostLayoutSettled`, `preserveScrollAnchor`, `preserveTimelineWindowAnchor`, `readonly isAtBottom?`. MessageTimeline publishes `Object.assign(stick, { notifyHostLayoutSettled, preserveTimelineWindowAnchor })` (MessageTimeline.svelte:631-634, attached at :660); ChannelView attaches the bare `stick` (ChannelView.svelte:181), which lacks the two optional methods — both fallback paths exist (PaneHost.svelte:147-151; thread.svelte.ts:986-989).

## 1. `createUseStickToBottomController` callers — exactly two

- MessageTimeline.svelte:441 (options wired: animationMode :438 via `latchedSpringMode(..., pane.lastLiveContentAt, ...)`, quietContextSignal :443, onBeforeScrollTopWrite :451 → virtua `markProgrammaticScroll`).
- ChannelView.svelte:49 — zero options (comment :43-48: deliberate default sync-pin).
- No others (grep verified, non-test).

## 2. ChannelView.svelte (Discussion surface) — full usage

Flow → API:
- **Channel switch/setup** (`$effect` :56-104): defensive `stick.setEscapedFromLock(true)` :72 then `stick.armRestoreSnap()` :82 — explicitly mirrors MessageTimeline's `$effect.pre` escape guard (comment :69-71).
- **Initial poll landed** (:145-161): `await tick()` then `stick.forceStick({ reason: 'restore' })` :159. Subsequent poll batches ride the contentRO sync-pin (no API call).
- **Scroll-to-bottom chip** (:328): reads `stick.isAtBottom`, calls `stick.forceStick()` (user reason).
- **Composer section RO** (:204-210): sibling-of-contentEl composer → `stick.notifyContentMaybeGrew()` :207 (concluded-toggle swaps textarea for a paragraph; textarea itself is fixed `rows={1}`).
- **DOM binding**: `stick.attach(scrollEl, contentEl)` :188-191; `stick.detach()` in onDestroy :112.
- **Pane publication**: `pane.attachScrollController(stick)` / detach :180-183 — so all pane-side lease/notify consumers below also operate on the Discussion controller.
- `overflow-anchor: none` on scrollEl :292 (mirrors chat contract).

NOT used: animationMode/spring, warm gate (`isWarm`/`armWarmup`/`skipWarmup`/`notifyQuietContextSignalChanged`), `quietContextSignal`, `onBeforeScrollTopWrite`, `notifyLiveContentMaybeGrew`, `markStructuralContentPending`, `preserveScrollAnchor`, `pauseAutoScroll` (directly), `markAtBottom`, `runExternalScroll`, `animateScrollTo`, `stopScroll`, `isSticky`, `escapedFromLock`.

Duplication a shared abstraction should own: the escape-then-arm-then-restore-forceStick thread-entry dance (:72-93 vs MessageTimeline's `$effect.pre` equivalent), controller pane publication effect (:180-183 vs MessageTimeline :659-662), attach effect, composer-geometry RO notification (:204-210 vs ChatView :116-167), and the `ScrollToBottomButton` chip wiring (:328).

## 3. PaneScrollController consumers (all 28 grep hits accounted for)

**Pause leases** (all assume: depth-counted, idempotent release, release-at-depth-0 re-pins if sticky — verified `willRepin` at useStickToBottom.svelte.ts:2440-2444):

| Consumer | Site | Flow | Lease window |
|---|---|---|---|
| SidebarResizer | sidebar/SidebarResizer.svelte:50 | LHS sidebar width drag (reflows every paragraph) | pointerdown→pointerup via `resizeGesture` (resizeGesture.svelte.ts:57,107,119) |
| RhsSidebarResizer | chat/RhsSidebarResizer.svelte:62 | RHS panel width drag; composed by RhsSidebarShell (RhsSidebarShell.svelte:84-93 passes `{pane}`) | same gesture lifecycle |
| PaneDivider | panes/PaneDivider.svelte:21-30,53 | split-pane divider drag — acquires leases on BOTH adjacent panes | same |
| Terminal drawer resize | terminal/ThreadTerminalPlacement.svelte:23-25 → ThreadTerminalDrawer.svelte:40 → primitives/Drawer.svelte:31,72 | bottom drawer height drag | pointerdown→pointerup |
| Terminal drawer async mount | ThreadTerminalPlacement.svelte:29-36 `leaseDuringSettle(pane.scrollController)` | cold lazy-drawer commit after the open lease already released | 2-rAF; relies on release→re-pin to land the timeline above the drawer |
| RHS panel open/close | thread.svelte.ts:417 `leaseDuringSettle(scrollController, 250)` inside `activatePanel` | plan/diff/design panel toggles (thread.svelte.ts:3903-3933) | 250ms setTimeout |
| Terminal drawer toggle | thread.svelte.ts:3869 in `setShowTerminal` | terminal show/hide | 2-rAF default |

Helper: `leaseDuringSettle` (utils/scrollLeaseDuringTransition.ts:26-46) — time-boxes a lease, returns idempotent early-release.

**Notifications:**
- ChatView.svelte:162 `notifyLiveContentMaybeGrew()` — composer RO (see §4). Contract assumed: synchronous, escape-aware, reads post-layout scrollHeight in the same RO phase.
- PaneHost.svelte:142-153 — after pane reorder (`paneOrderKey` :140), 2-rAF later calls `notifyHostLayoutSettled()` per pane, falling back to `notifyContentMaybeGrew()` for controllers lacking it (i.e. Discussion). Not content growth — virtualizer/host-offset reconciliation (comment :133-139). Contract: eventual (rAF-deferred), intent-preserving.

**Anchor transactions:**
- `preservePaneScrollAnchor` (chat/preserveScrollAnchor.ts:3-14) → `scrollController.preserveScrollAnchor(anchor, action)`. 13 row components use it for disclosure toggles/show-full: AgentRow:124, AdvisorRow:125, AskUserQuestionCard:145, GenericToolCallRow:174, SubagentGroup:212, DiffFileBlock:196, CompactionDivider:83, ToolResultCard:229+275, LazyContentBlock:94+109, ReasoningTailRow:157, ExpandablePayloadBody:88, WaitGroup:67, CommandOutput:201+246. Contract: sticky users stay pinned, escaped users keep the clicked element viewport-stable; may be async (action can return a Promise); falls back to bare `action()` when no controller.
- `preserveTimelineWindowAnchor` — thread.svelte.ts:986-999 in `applyPrunedWindow` (streaming/settle live-window prune). Contract is **synchronous veto**: the store passes `{keepsItem, run}`, and relies on `operationApplied` being observable immediately after the call to decide `'deferred'` vs forced apply (:996-998). The deferred-prune retry loop closes in MessageTimeline (:636-640, gated on `stick.isSticky`). By contrast, `loadOlder`/`loadNewer` paging deliberately does NOT use it — it uses virtua's `shift` compensation (`pendingTimelineShiftAtHead`, thread.svelte.ts:2863-2866, 3098-3104).

**Registration plumbing:** thread.svelte.ts:567 (slot), :3178-3194 (getter/attach/detach with stale-teardown guard).

**Not-a-controller but scroll-adjacent routing:**
- `pane.requestScrollToItem` (thread.svelte.ts:3159-3169) publishes a nonce'd `ScrollToItemRequest` (threadPaneShared.ts:163-167); MessageTimeline consumes it. Publishers: MessageSearch.svelte:156 (search-hit navigation, after `openThreadFromNavigation`) and DiffPanelDrawer.svelte:185 (checkpoint→user-message jump, flash). Clean seam; unaffected by a resolver redesign.
- events.ts touches scroll only indirectly: `pane.markLiveContentAdvanced()` at events.ts:711 stamps `lastLiveContentAt` (thread.svelte.ts:258-260) — the sole input to MessageTimeline's `animationMode` latch (springAnimationLatch.ts:41-53). Tool rows deliberately don't stamp (events.ts:706-710).

## 4. ChatView.svelte composer/overlay seam (:109-167)

The composer is an overlay, NOT inside the controller's contentEl or scrollEl — growth changes only the trailing padding, never scrollEl `clientHeight` (comment :109-115). The RO (:116-167):
1. Writes `--composer-height` **directly on the DOM node** (:153-155), bypassing Svelte's microtask flush, so the controller's subsequent scrollHeight read forces layout with the new padding applied (rationale :132-145).
2. Then `pane.scrollController?.notifyLiveContentMaybeGrew()` (:162). The **live-capable vs sync-pin distinction** (comment :156-161): the notification is escape/pause/at-bottom-gated, and routing through the live-capable hook lets an active stream spring through a working/todo-rail height change while idle composer geometry still sync-pins (animationMode returns 'instant' when the latch is cold).

## 5. Raw scrollTop/scrollIntoView/scroll-listener sweep — all 57 hits classified

**Contract violations: none found.** The documented contract ("controller owns every chat-scroller scrollTop write outside virtua internals") holds. Gray areas / flags:

- **PaneHost.svelte:173** `scrollIntoView({behavior:'smooth', block:'nearest'})` on a pane `<section>` — targets the *horizontal* pane-host scroller (its own listener :111 tracks `scrollLeft` only). Gray area: `block:'nearest'` could in principle move a vertical ancestor, but pane sections are full-height; benign today, worth pinning in the redesign contract.
- **DiffSidebarBody.svelte:122** (`scrollRoot.scrollTop = initialScrollTop`), :128 (`scrollIntoView` to a file block), :261 (`onscroll` → persist) — RHS diff panel's **own** scroller; state hoisted per-thread via `pane.recordDiffSidebarUI` → rhsPanelSlot.svelte.ts:105,153 (thread.svelte.ts:3982-3993). Allowed, but it is a **second, independent scroll-snapshot/restore system** parallel to `threadScrollSnapshots.ts` — duplication candidate for the shared abstraction.
- **CommandPalette.svelte:160**, **DirectoryBrowser.svelte:73** — `scrollIntoView({block:'nearest'})` on their own modal list scrollers. Fine.
- **VirtualList.svelte:38-78** — self-contained fixed-row virtualizer (used only by sidebar ProjectList.svelte); own onscroll. Fine.
- **Popover.svelte:347** — capture-phase window scroll listener, read-only reposition. Fine.
- **ProposedPlanReviewSurface.svelte:112-114** — read-only `surfaceRoot.scrollTop/scrollLeft` for selection-comment math on the plan panel's own scroller. Fine.
- **uiRenderTrace.ts:464**, **paneGeometryProbe.ts:85-90** — diagnostics; probe reads `isSticky/isAtBottom/escapedFromLock/isWarm` (:83-88) read-only.
- **Former violators now clean (comment-only hits):** TailClampedText.svelte:9-18 (imperative `scrollTop = scrollHeight` replaced by pure-CSS flex-end anchor), StreamdownMathHost/StreamdownMermaidHost (CSS grid fallback + min-height cache pins, no scroll writes — MermaidHost:26-35), payloadExpansion.svelte.ts:266-274 (avoids the clamp by never blanking chunks), threadItems.ts:57, thread.svelte.ts:289/879/2864/3099 (explanatory comments).
- **Stale comment**: MessageSearch.svelte:153 says the timeline handles the hit via "loadUntilItem + scrollIntoView" — actual mechanism is virtua `scrollToIndex` under `runExternalScroll` (chat/CLAUDE.md forbids scrollIntoView on virtualized rows). Wording fix, not a violation.
- Unrelated `detach` hits (TakeControlTerminal.svelte:244, focusTrap.ts, frontendErrorCapture.ts:306) are different functions.

## Consumer → API matrix

| API member | MessageTimeline | ChannelView | ChatView | thread store | PaneHost | Resizers ×3 | Terminal placement/drawer | 13 row comps | geometry probe |
|---|---|---|---|---|---|---|---|---|---|
| attach / detach | yes | :188/:112 | – | – | – | – | – | – | – |
| forceStick user | yes | :328 | – | – | – | – | – | – | – |
| forceStick restore + armRestoreSnap | yes | :159/:82 | – | – | – | – | – | – | – |
| setEscapedFromLock | yes | :72,:75 | – | – | – | – | – | – | – |
| isAtBottom | yes | :64,:328 | – | – | – | – | – | – | read |
| isSticky / escapedFromLock / isWarm | yes | – | – | – | – | – | – | – | read |
| pauseAutoScroll | yes (:614) | – | – | :417,:3869 | – | all 3 | :24,:35 | – | – |
| notifyContentMaybeGrew | yes | :207 | – | – | fallback :150 | – | – | – | – |
| notifyLiveContentMaybeGrew | yes | – | :162 | – | – | – | – | – | – |
| notifyHostLayoutSettled (pane ext.) | provider :632 | – | – | – | :148 | – | – | – | – |
| preserveScrollAnchor | provider | – | – | – | – | – | – | all via helper | – |
| preserveTimelineWindowAnchor (pane ext.) | provider :601 | – | – | :986 | – | – | – | – | – |
| markAtBottom, runExternalScroll, skipWarmup, armWarmup, notifyQuietContextSignalChanged, markStructuralContentPending | yes (only) | – | – | – | – | – | – | – | – |
| animateScrollTo, stopScroll | **NOBODY** | – | – | – | – | – | – | – | – |
| options: animationMode, quietContextSignal, onBeforeScrollTopWrite | yes (only) | – | – | – | – | – | – | – | – |

**Dead surface:** `animateScrollTo` + `stopScroll` have zero production callers (test-only, useStickToBottom.svelte.test.ts:1456-1567); the docstring at :418 and the test comment at :1549 both carry the stale claim "used by handleLoadOlder / scrollToItem" — those paths now use `runExternalScroll` (MessageTimeline :1732,:1785,:1853). `PaneScrollController.isAtBottom?` (threadPaneShared.ts:151) is declared but never read through the pane. All three are deletion candidates.

## Redesign implications (resolver-based controller)

- **Pause leases → resolver input.** All 7 acquirers share one need: "suppress writes while held, re-evaluate target on release." One contract to preserve: release-at-depth-0 must trigger an immediate re-evaluation (sync-pin if sticky) — ThreadTerminalPlacement.svelte:29-36 depends on it.
- **notifyContentMaybeGrew / notifyLiveContentMaybeGrew / notifyHostLayoutSettled → one observation event with a source hint** (`composer-geometry` | `host-layout` | `content`). The spring-vs-instant decision already lives in animationMode + the latch; consumers don't actually choose animation, they just picked whichever method existed. ChatView's only hard requirement is that the resolver's read happens after its synchronous CSS-var write (same-task ordering).
- **preserveScrollAnchor** stays a transaction API (13 call sites, trivially wrapped) — could compile to "anchor constraint for the resolver during an action window."
- **preserveTimelineWindowAnchor** must keep its **synchronous veto** return; the store's defer logic (thread.svelte.ts:996-998) breaks under an async/eventual answer.
- **ChannelView** needs only: attach/detach, escape set, restore arm+snap, user snap, isAtBottom, and the single observation event — a strict subset; it would shrink under the redesign, not need rework.
- **Consumers needing rework:** none externally — every external consumer's need maps 1:1 onto {lease, observe, anchor-transaction, snap/escape intent, isAtBottom}. The whole 21-member surface minus the timeline-private members (warm gate, runExternalScroll, markAtBottom, markStructuralContentPending, options) is only exercised by MessageTimeline, so the split is clean: a ~7-member pane/consumer API and a timeline-private internal seam.

result: Consumer map complete — controller has exactly 2 instantiations (MessageTimeline, ChannelView); pane surface reduces to lease/observe/anchor/snap/isAtBottom across 7 lease sites, 2 notify sites, 14 anchor sites; animateScrollTo+stopScroll and PaneScrollController.isAtBottom are production-dead; no raw-scroll contract violations found.


---

## A6 — Test contracts (399 tests classified, 27 contracts)

Both subagent reports are validated (spot checks passed on seq-509, the verbatim user-report, constant-inequality guard, A→B→A block, overflow-anchor guard, and the 20260622 regression). I also covered `timelineScroll.test.ts` which both briefings had missed. Sanity check done: all cited files/lines verified against the working tree at f42dc6e6, both suites run green (unit scroll files 369 tests/~30s; browser 8 tests/2.4s). Full findings follow.

---

# Scroll-system test surface — investigation findings

## A. Inventory, runtime cost, infrastructure

| File | Tests | Runtime | Notes |
|---|---|---|---|
| `frontend/src/lib/utils/useStickToBottom.svelte.test.ts` (6385 ln) | 255 | **27.2s** test-time | Real timers throughout; ~25–40s is pure sleeping (`waitMs(150)` warm-up per spring test, one 2.65s failsafe test at 2492–2514). Dominant cost of the whole frontend unit gate. |
| `frontend/src/lib/components/chat/scroll.test.ts` (2589 ln) | 64 | 0.95s | Mounts real MessageTimeline/ChatView, **real virtua** under happy-dom (`ssrCount=100_000` when `MODE==='test'`, MessageTimeline.svelte:176,2009). |
| `timelineRowGeometry.test.ts` (585 ln) + `threadRowUiState.svelte.test.ts` (1010 ln) + `messageTimelineVirtuaMarking.test.ts` (80 ln) | 50 | ~1s | Fake timers (geometry), pure store factory, stub virtualizer respectively. |
| `timelineScroll.test.ts` (426 ln; **not in the original briefing — found via scroll.test.ts cross-reference**) | 22 | fast | Pure functions: anchor capture, trigger zones, auto-load gate. |
| Browser suite: `rowMarginContainment` / `rowSettleFloor` / `tailClampedText` `.browser.test.ts` + `src/test/integration/virtua-patch-buffer-retention.browser.test.ts` | 8 | **2.44s wall** | Real Chromium (playwright provider, headless). |

**Infrastructure to build on**
- `src/test/helpers/chat.ts` — `buildPane`/`makeItem`/`makeThread` + `installPaneMocks` (all backend RPCs mocked via `src/test/mocks/bindings-app.ts`). Streams are driven via `pane.upsertItem(...)` directly (scroll.test.ts does this; no test uses `setupEventListeners`).
- `src/test/helpers/scrollDom.ts` — geometry `defineProperty` stubs, `setElementRect`, controllable RO stub (happy-dom only).
- `src/test/helpers/browserFrames.ts` — `raf()`, `waitFor(predicate, label, frameBudget=120)`, `wait(ms)` for the Chromium project.
- `src/test/mocks/StubVirtualizer.svelte` + `virtuaMarkRecorder.ts` — flat-list Virtualizer stand-in whose only live method is the patched `markProgrammaticScroll()` (records to a shared counter).
- `src/test/integration/virtua-patch-fixtures/VirtuaBufferRetentionHost.svelte` — real `<Virtualizer>` (200×40px rows, external scrollRef, `use:lifecycle` mount/destroy counters). The template for outcome-level unmount-burst detection.
- **Key config fact:** the browser project reuses the same resolve aliases as unit (`vitest.config.ts:88` `resolve: happyDomResolve`) → bindings/runtime mocks + buildPane work in Chromium. But it has **no `setupFiles`** — no jest-dom, no `cleanup()`, none of `setup.ts`'s store resets.

**Browser-suite flake risk:** rowMarginContainment — none (sync layout reads). rowSettleFloor — low (predicate `waitFor`, no fixed sleeps). tailClampedText — low (raf-stepped, ≤1px threshold assertions, width-relative). virtua-patch-buffer-retention — moderate-but-bounded: two fixed 400ms real waits (virtua synthesizes scrollend 150ms after last scroll); assertions are wide thresholds (≥10 / ≤1 destroys).

## B. Contract map (classification: OUTCOME = survives rewrite; IMPL = dies with code; MIXED = behavioral contract, implementation-coupled assertion)

### useStickToBottom.svelte.test.ts
Imports that couple tests to the implementation (lines 1–9): `resetUseStickToBottomModuleStateForTest`, `RETAIN_ANIMATION_DURATION_MS`, `latchedSpringMode`/`SPRING_MODE_HOLD_MS` (springAnimationLatch), uiRenderTrace API. Scaffolding (stubGeometry/MockResizeObserver/mockNow-frames/event synthesizers, 11–146) is implementation-agnostic and ports to any `attach(scrollEl, contentEl)` controller.

| Block (lines) | Contract | Class | Provenance |
|---|---|---|---|
| initial state (194) | at-bottom start = follow on; escape suppresses first-fire snap AND growth pins | OUTCOME | open-thread animation regression (222) |
| wheel handler (256) | any upward wheel (incl. nested scrollers, zero-movement) escapes synchronously; scrollbar drag down to bottom re-sticks; resize-correlated jump ≠ user scroll | OUTCOME | hard-won |
| keyboard/touch (504/516) | ArrowUp/PageUp/Home & finger-down escape; down keys don't | OUTCOME | |
| content RO (536) | sticky growth sync-pins same frame; escaped/leased ⇒ nothing moves; shrink re-pin honors logical at-bottom over geometric band; overscroll clamp only when sticky; **idle sub-pixel deadband** (760–824: ≤2 writes across 12 oscillations, line-sized growth still pins) | OUTCOME; deadband MIXED (write counts); `clears resizeDifference` (744) vacuous IMPL | **bug-report-20260701T012813Z** WSLg fractional-DPR viewport shimmer; layout-cascade jump (614) |
| programmatic write (827) | controller writes invisible to intent model; tag consumed exactly once (915); hook before every write (875) | MIXED→IMPL (trace records 853–873, getComputedStyle spy, scrollBehavior) | hook is the virtua-patch seam |
| scroll handler (952) | escape clears only at ≤4px auto-follow epsilon, never the 70px visual band; wheel-down during RO cascade re-sticks; 1-notch wheel-up not undone by own scroll event | OUTCOME | "auto-follow stops until refresh" (1011); "scroll up one notch yanks back" (1113) |
| forceStick + restore consent gate (1181/1222) | chip-click always wins & consumes pending restore; stale restore never yanks escaped user; arm is one-shot, gesture-invalidated, survives detach/re-attach | MIXED (arm lifecycle = mount-ordering coupling; core is OUTCOME) | seq-509 trace bug (1224) |
| markAtBottom (1402) | flip intent at bottom **without writing scrollTop** | OUTCOME | restore path load-bearing |
| animateScrollTo/stopScroll/runExternalScroll/preserveScrollAnchor/pauseAutoScroll/notifyContentMaybeGrew (1456–1787) | cancel-on-user-intent; external-scroll tagging; disclosure anchor for escaped users, pause released after DOM flush; depth-counted lease | OUTCOME (via controller seam) | |
| lifecycle (1789) | listener attach/detach | MIXED (RO counts, `window.__stickState` = IMPL) | |
| **architectural invariants (1847)** | geometry mutation alone NEVER flips intent; only input-backed bottom-reaching scroll resurrects sticky | OUTCOME — most rewrite-relevant block in the file | design decision |
| scroll-intent regressions (1928) | dist-from-bottom captured at event time (moving-bottom re-stick); 4px epsilon boundary (3px re-sticks, 5px doesn't); scrollend fully inert; pinch-zoom ≠ intent; scrollbar-tap escape blocks all pin paths with zero writes | OUTCOME | gentle-mango Bug A; Opus-stream regression (1944) |
| virtua $fixScrollJump anchor redirect (2259) | settled-at-bottom stale below-bottom write must not paint one frame short | MIXED (presumes write-intercept gate) | cold-switch flicker |
| warm-up gate (2328) + quietContextSignal (2571) | uncached loads reveal only after measurement settles (RO evidence + quiet, never bare timer); renderer settled-signal shortens reveal only when geometry holds still | MIXED→IMPL (isWarm is a consumed seam; quiet/failsafe timing races are mechanism) | "half-screen-high" uncached-thread regression (2456); idle-thread flicker (2633) |
| spring chase (2817) | streaming follow animates frame-rate-independently, bounded catch-up on stall; **width-reflow height changes sync-pin, never chase** (2948–3024); policy matrix for live nudges (escaped/paused/instant/reduced-motion) | MIXED — policy matrix portable; physics nested blocks (stuck-spring 3200, momentum carry 3331) pure IMPL; distance invariants (3435: visual travel == real content delta) MIXED w/ portable invariant | width-reflow chase regression |
| pauseDepth during spring (3833); gate coupling (3915) | lease kills animation, external writes pass, release re-pins | MIXED; gate-coupling block IMPL | |
| **user-reported regression: streaming stops following after send + manual scroll (4049)** | full send→stream→wheel-up→re-stick→stream flow keeps following; scrollend races never change outcomes | **OUTCOME — most portable battery** | verbatim user report 4052–4055 |
| **user-reported regression: re-stick invariants (4388)** | "stick by default, unstick on any tiny scroll-away, re-stick always at bottom"; **clamp-then-wheel lockout**: wheel-down while escaped at absolute bottom re-sticks with zero scroll events | **OUTCOME — port nearly verbatim** | user intent quote 4394–4398; **bug-report-20260520T010930Z** seq 4953–5317 ("180 wheel events could not reach re-stick; user stranded", 4766) |
| edge cases (4965) | small mid-stream shrinks (~22px streamdown fence rebalance) don't jitter; >50px shrink snaps; net-zero oscillation recovers synchronously (no one-frame strand); inter-chunk >350ms dead-zone doesn't degrade to snaps; selection pauses follow | MIXED (contracts hard-won; forms saturated with sentinel/retain mechanism) | streaming jitter / parseIncompleteMarkdown (5078); Bug B (5216); **bug-report-20260615T182227Z** (5320) |
| **external scrollTop write gate (5638)** | scrollTop ownership matrix: during active animated follow controller is sole writer; escaped/paused/pre-warm/post-restore/dormant/instant/viewport-scale corrections pass through immediately; brief wire-round gaps don't flip arbitration | MIXED→IMPL (presumes descriptor-intercept gate). Matrix distills **five shipped regressions**: bug-reports 20260524T183128Z, 20260524T200233Z, 20260622T041049Z (+2276px suppressed → ~1s visible "spring scroll", 5887), revert-puts-you-at-top, wire-round gap. `SPRING_MODE_HOLD_MS > RETAIN_ANIMATION_DURATION_MS` (6205) & descriptor-restore (6362) pure IMPL | |

### scroll.test.ts (mounts real MessageTimeline/ChatView; internal-coupling hazards: monkey-patched `pane.attachScrollController`, widened casts to internal controller state, module spy on `getReplayableVirtuaCache`, `*ForTest` hooks, testid/data-attr DOM contract)

| Block (lines) | Contract | Class | Provenance |
|---|---|---|---|
| snapshot save/restore (134–689) | anchor vs bottom snapshots; **A→B→A replays cached row geometry** (220–286); **bottom restore = exactly one scrollTop writer** + one rAF settle pass; fallbacks end sticky | MIXED (contracts OUTCOME; spy/structureSig-grammar assertions IMPL) | thread-switch flicker; revision-counter A→B→A bug; Streamdown-tick oscillation (449); "half a scroll tick from bottom" (493) |
| load older (691) | one batch per click, never auto-cascade | MIXED | auto-load-gate cascade regression |
| scroll to item (749) | loads item first; missing → toast; flash highlight | OUTCOME | |
| composer height (798–1046) | composer growth re-pins **synchronously in RO callback** (no 1-frame gap, 816–889); `overflow-anchor:none` (1006); `scrollbar-gutter: stable both-edges` (1025, WebKitGTK ~5px column jump) | MIXED; the two CSS opt-outs OUTCOME/durable | "appears then settles", 200–400px chip flicker; note 891–902: **width-oscillation fix deliberately NOT tested here** ("coverage theater" in happy-dom) — discriminating guard lives in timelineRowGeometry.test.ts |
| banner overlay (1048) | banners never reserve height → no timeline reflow | OUTCOME | |
| auto-follow + button (1068–1390) | 1px wheel-up escapes; layout-only movement doesn't; chip lives outside scroll container; **Bug A re-stick under 60Hz streaming cadence** (1319) | MIXED (intent OUTCOME, asserted via internal controller state) | chip rode off-screen; gentle-mango Bug A |
| mid-list inserts (1392) | late tool_completions land in `(turnIndex,itemIndex)` order; tail stays resolvable | OUTCOME (data contract) | |
| auto-load gates (1495–1640) | no request loop on null cursors/in-flight/exhausted, both edges | MIXED (drives via happy-dom+ssrCount scroll seam) | positive triggers live in timelineScroll.test.ts |
| useStickToBottom wiring (1649–2432) | controller published for host surfaces; **window pruning invisible** (veto if anchor dropped; re-pin via markAtBottom not forceStick, 1682–1859); **no intent inference from scrollTop direction (R4)** (1948); **live-follow nudge selectivity** (2024–2328: structural tail appends only — never deltas, metadata churn, or switch-into-streaming-thread); warm doesn't leak across switches, cascade behind `visibility:hidden` (2330: "lands wrong, jumps to correct", 461px/216-item sample) | MIXED throughout (spy-count observables) | **bug-report-20260622T041049Z** multi-hundred-px scroll on switch (2062) |
| draft placeholder (2434) | real→draft returns to sticky-bottom; chip never stranded over empty pane | MIXED | |
| row geometry containment application point (2559) | every `[data-row-index]` wraps one `[data-row-geometry-content]` | IMPL guarding the CSS-side OUTCOME rule | 4b3759a1, settle-flicker-analysis.md |

### timelineRowGeometry.test.ts (pure action, fake timers, cache keyed by production `timelineRowGeometryCacheKey`)
- **Exact fractional cached heights** (68–84, MIXED ⚑): rounding = ±0.5px release residue per row = "settle-flicker amplifier".
- Height keyed to **measured** width, not laggy param width (86–110, MIXED ⚑): else timeline "strands above the composer".
- Hold floor through transient short remounts (112); stale reservation self-expires at 750ms (135, IMPL timer, durable rule "never a permanent wrong floor"); **never re-floor an already-settled visible row** (237–264, MIXED ⚑ — the 2–6px settle "twitch"); rebind re-arms (303).
- Trace-hook block (333–460, IMPL): pins `timeline.row.geometry` event grammar for Ctrl+Shift+B captures — diagnostics capability worth re-establishing, form dies.
- **observeScrollSurfaceContentWidth never makes a sync layout read** (463–489, MIXED ⚑): THE unit-level discriminating guard for the idle width-oscillation loop (incident 2026-06-26, commit a5a5d032); rule: row-geometry width has exactly one source (RO content-box).

### threadRowUiState.svelte.test.ts (geometry scope only)
- Row-height cache: fractional round-trip; signature/width miss → undefined; new signature invalidates all widths (462–500, MIXED ⚑).
- **User-driven height change (diff toggle 502, expansion collapse 527) synchronously invalidates that row's preserved geometry** (OUTCOME ⚑).
- Geometry cache pruned with row window / disposal (563, 782 — memory-bound rule, MIXED).
- **expansionSignature** (941–1009, MIXED ⚑): keys virtua CacheSnapshot replay; force-collapsed override (`=0`) must stamp and differ from `=1` else "the cache replays the wrong height"; deterministic across insertion order. Durable rule: cached size snapshots never replay when expansion state differs from capture.

### timelineScroll.test.ts (pure functions, fake TimelineGeometry stubs)
Anchor capture at offset + clamp to loaded window; head-drop detection from virtua keys; trigger-zone cheap-gates-first ordering (IMPL perf rule); **auto-load gate: fires once per floor cursor, blocks until progress, disarm/`armOnGesture` (user-gesture-armed), cooldown fallback, reset on switch** — the positive-side counterpart of scroll.test.ts's no-cascade contract. Mostly MIXED/IMPL; the gesture-armed no-cascade rule is the durable piece.

### messageTimelineVirtuaMarking.test.ts (IMPL guarding an OUTCOME rule ⚑)
Pins the seam: MessageTimeline wires controller `onBeforeScrollTopWrite` → patched `markProgrammaticScroll()` (asserts pin lands at 400 AND mark counter incremented; deliberately 10px shy because equal-value pins skip the write). Three-way split is documented in its header: controller side in useStickToBottom tests, virtua side in the browser tripwire, this closes the middle. Dies with any rename of the option/method/stub surface. Durable rule: an intent-inferring virtualizer must be told which scrollTop writes are programmatic.

### Browser suite (all OUTCOME, deliberately mechanism-neutral)
- `rowMarginContainment.browser.test.ts` (44–79): trailing bottom margin stays inside the row content box (`flow-root` fails-without/passes-with, imports production app.css); markdown h1 flush-top (BFC × margin-reset coupling). ⚑ settle-flicker fix.
- `rowSettleFloor.browser.test.ts` (77–122): once measured (fractional 235.4px), a later `update()` with churned signature + taller stale cache entry (350) must NOT grow the rendered box — the ~115px twitch. Header explicitly states it locks the outcome so it "survives a future refactor that changes HOW the floor is written". ⚑
- `tailClampedText.browser.test.ts` (118–177): collapsed reasoning tail = 3-line bottom-anchored window; newest glyph stays visible through a **width-only re-wrap with no text delta** (the a5a5d032 spring-width-oscillation strand); short content collapses without flex-end gap. Assertions via character Range rects, pin-mechanism-agnostic. ⚑
- `virtua-patch-buffer-retention.browser.test.ts` (85–108): paired tripwire — unmarked 8px programmatic write drops ≥10 buffered rows (patch drop-rule signal); marked write destroys ≤1 (the settle-flicker fix). ⚑

## C. Must-survive-any-rewrite contracts (deduplicated, stated as behaviors)

**Intent**
1. At-bottom threads auto-follow; growth pins same-frame, no lag.
2. Any upward user input (1px/sub-pixel/zero-movement wheel, keys, touch, scrollbar grab, middle-button, selection-while-scrolling) breaks follow synchronously — same-frame growth must not pin.
3. While escaped, nothing layout-driven moves the viewport (no snap, pin, shrink re-pin, clamp, composer/live nudge).
4. Re-stick only via: input-backed scroll reaching bottom within ~4px (5px stays escaped; the 70px band is chip-visibility only), explicit forceStick, or wheel-down while already clamped at bottom (zero scroll events — the 20260520 lockout).
5. Down-intent is fresh: expires (~300ms), cancelled by any up input including in the deferred window; judged by distance seen at event time (Bug A — moving bottom).
6. Intent mutates only on explicit signals — never inferred from geometry or untagged scrollTop direction (R4; applies to virtua $fixScrollJump and per-row resizes).
7. Scrollend inert; pinch-zoom ≠ intent; nested-scroller wheel-up escapes outer follow.
8. After re-stick/chip-click, every subsequent chunk follows — no leaked one-shot state ("stops following until refresh").

**Programmatic writes / virtualizer arbitration**
9. Controller writes invisible to intent model; suppress exactly one scroll event; announced to the virtualizer before landing (else buffer-drop remount churn = streaming settle flicker).
10. During active animated follow the controller is the sole scrollTop writer; escaped/paused/mount-cascade/post-restore/dormant/instant/viewport-scale corrections pass through untouched; brief wire-round gaps don't flip arbitration.
11. Settled-at-bottom: stale below-bottom anchor writes never paint even one frame short.

**Thread switch / restore**
12. Switch escapes first; restore-to-bottom needs one-shot consent invalidated by any gesture; stale restores lose; landing via virtua then marking at bottom issues no redundant write; exactly one writer at restore + one settle pass.
13. Per-thread position survives switch (anchor → same item, loading if needed; bottom → sticky, chip hidden); A→B→A reuses cached row geometry with a content-derived validity key (never a monotonic counter), keyed by width AND expansion state (`=0` ≠ `=1`).
14. Cold loads: cascade hidden until measurement settles (evidence-based, never bare timer while data loads); warm state doesn't leak; renderer settled-signal shortens reveal only when geometry holds still.
15. Switching INTO a streaming thread must not arm an append-follow animation over the restore backlog (20260622: multi-hundred-px scroll).

**Streaming motion quality (if animation kept)**
16. Frame-rate-independent, bounded catch-up, no per-chunk restart, never moves away from bottom; total travel == real content delta (estimates/nudges/net-zero oscillations add zero); net-zero oscillation with browser clamp recovers synchronously.
17. Small mid-stream shrinks (~22px streamdown rebalance) don't jitter; viewport-scale corrections land instantly; >350ms inter-chunk gaps don't degrade; width-reflow height changes pin instantly; reduced-motion disables animation.

**Rows / geometry**
18. Row margins contained in the measured row box; markdown rows flush-top.
19. Reservations: exact fractional heights; keyed to measured width; cold-mount bridge only — never re-floor a settled visible row; hold through transient shorts; self-expire; sync-invalidated on user collapse/expand; pruned with window.
20. Row-geometry width from exactly one source (RO content-box) — no sync layout reads (idle width-oscillation loop, 2026-06-26).
21. Collapsed reasoning tail: newest line visible through width-only re-wraps.
22. Idle fractional-DPR: sub-pixel oscillation never sustains a write loop; line-height growth still pins (20260701 vibration).

**Host/UX invariants**
23. Composer growth re-pins in the same frame (no 200–400px flash); live-capable path during active output.
24. Chip outside the scroll container; shown iff escaped ∧ not-at-bottom; never stranded on draft panes.
25. `overflow-anchor: none` + symmetric scrollbar-gutter on the timeline scroller; banners overlay without reserving height.
26. Load-older: one batch per gesture, no auto-cascade, no request loops (both edges); window pruning invisible (anchor veto / restore / markAtBottom re-pin); disclosure toggles anchor escaped users, pause releases after DOM flush; pause lease depth-counted, never strands.
27. Items stay `(turnIndex,itemIndex)`-ordered under late arrivals so anchors/last-index resolve.

## D. Coverage holes (no test today)

1. **Full streaming-beat simulation with real geometry** — nothing mounts MessageTimeline in Chromium. Unit suite stubs geometry; browser suite tests isolated fragments (plain-div virtua host, single row, single component). The interaction *stick-controller × real virtua × real row measurement × real RO timing* is exactly where the settle-flicker family lived, and it is untested end-to-end.
2. **Spring visual continuity in a real engine** — all spring assertions run on mocked `performance.now` + happy-dom rAF.
3. **Load-older/paging interleaved with active streaming** (anchor preservation while the bottom grows) — gates are tested separately, the interleave never.
4. **Multi-pane**: every test registers a single `'main'` pane; module-level controller state exists (`resetUseStickToBottomModuleStateForTest`, `window.__stickState`) and cross-pane interference is unexplored.
5. **Escape gestures in real Chromium** — trusted wheel/scrollbar/momentum input never exercised (all synthetic events with hand-written geometry consequences).
6. **Thread switch mid-stream, away-and-back while the turn continues** (snapshot correctness during streaming) — only the switch-INTO-streaming case is covered.
7. **virtua CacheSnapshot replay in real geometry** — A→B→A is verified as cache bookkeeping, not as "the pixels didn't jump".
8. **Fractional DPR / zoom in a real engine** (the 20260701 fix is deadband-unit-tested only; playwright could set deviceScaleFactor).
9. **ChannelView (discussion) scroll surface** — no scroll tests at all (frontend-scroll.md claims the same contracts apply).
10. **Window/viewport resize while pinned**; real reduced-motion media query.
11. scroll.test.ts:891–902 documents a deliberate hole: the width-oscillation feedback loop is only guarded at the unit level (no-sync-layout-read); a happy-dom component test was rejected as "coverage theater". A Chromium harness could close this honestly.

## E. Streaming outcome harness — concrete proposal

**Shape:** `frontend/src/lib/components/chat/streamingOutcome.browser.test.ts` (browser project matches `src/**/*.browser.test.ts`). Mount the **real MessageTimeline** with a **real pane** in Chromium; drive a synthetic stream; assert outcomes only.

**What already works:**
- `buildPane`/`makeItem` + bindings mocks resolve in the browser project (shared `happyDomResolve`, vitest.config.ts:88). The marking test (happy-dom) already proves the buildPane→render-MessageTimeline pattern.
- Streaming drive seam: `pane.upsertItem(...)` with growing summaries + `projectTurnStarted/projectTurnCompleted` (threadStatuses) for active-turn/spring mode — exactly what scroll.test.ts:2024–2328 uses.
- `import '../../../app.css'` compiles (tailwind plugin present in browser project) → real row cascade incl. `flow-root` rule.
- `browserFrames.waitFor` for deterministic settles; `VirtuaBufferRetentionHost`'s counter pattern for burst detection.

**Assertions (all outcome-level):**
- *Bottom-follow monotonicity / zero clamp-backs:* rAF sampling loop over `scrollEl.scrollTop`; while pinned and content growing, scrollTop never decreases (>1px tolerance) and `scrollHeight - clientHeight - scrollTop` returns to ≤ epsilon at each quiet point.
- *No unmount bursts while pinned:* MutationObserver on the virtua container counting removed `[data-row-index]` nodes during the stream (threshold like the tripwire's ≤1/beat), or a `use:lifecycle`-style probe if rows get a test hook.
- *Chip never appears* (`[data-testid="scroll-to-bottom"]` absent) while never escaped.
- Optional extension: trusted user input via `@vitest/browser` `userEvent` (playwright CDP → real wheel with native scrolling) to run the wheel-up escape / wheel-down re-stick battery against real scroll physics — closes hole D5.

**What's missing / small changes needed:**
1. **`ssrCount=100_000` fires in the browser project too** (`IS_TEST = MODE==='test'`, MessageTimeline.svelte:176) — initial flat render then a windowing unmount wave pollutes counters. Mitigation: reset counters after initial settle (buffer-retention test pattern), or add a seam (env flag / browser detection) to disable ssrCount under the browser project. The latter is a 1-line production change worth making.
2. **No browser-project `setupFiles`** — add one (subset of setup.ts: store resets + cleanup) or reset manually per test (`clearThreadItemCacheForTest`, `clearThreadScrollSnapshotsForTest`, `clearThreadVirtuaSizeCacheForTest`, `resetBindingMocks`, threadStatuses reset).
3. Font-dependent row heights → keep all assertions threshold/relative (existing browser tests already model this).
4. Budget: real timers (~150ms warmup + N beats at 30–60ms) → ~3–5s per scenario; fine next to the current 2.4s suite.

## F. Gate wiring facts + smallest gating change

**Today:**
- `pnpm test` = `vitest run --project unit` (frontend/package.json:16); `pnpm test:browser` = `--project browser` (line 18) — separate, manual.
- `make test` = go-test + `pnpm test` (Makefile:183–185). `make check` = go-build + svelte-check (187–189).
- `make verify` → `scripts/release-check.sh`: frontend build, go-build, go-test, check, `pnpm test` (unit only), wails build (+ build-wsl on Linux). **Browser suite in no gate.**
- CI (`.github/workflows/release-build.yml`) runs **zero tests** — it's build/package/artifact only. The only "required gate" in this repo is the local `make verify` convention.
- Provisioning: `playwright@1.61.1` + `@vitest/browser-playwright@4.1.4` are already pinned devDeps; only the Chromium binary is missing from `make install` (needs `pnpm exec playwright install chromium`, ~170MB into `~/.cache/ms-playwright`, idempotent). On my machine it's installed; suite passes in 2.44s.

**Smallest change to gate the browser suite (two lines):**
1. Makefile `install` target: append `cd frontend && pnpm exec playwright install chromium`.
2. `scripts/release-check.sh`: add `run sh -c "cd '$ROOT_DIR/frontend' && pnpm run test:browser"` after the unit-test line (optionally also in `make test`). At 2.4s the cost is negligible; the chromium download happens once per machine. If CI testing is ever added, ubuntu runners need `playwright install --with-deps chromium`.

Key files: `/home/rmurphy/repos/agent-overflow/frontend/vitest.config.ts`, `/home/rmurphy/repos/agent-overflow/frontend/package.json`, `/home/rmurphy/repos/agent-overflow/Makefile`, `/home/rmurphy/repos/agent-overflow/scripts/release-check.sh`, `/home/rmurphy/repos/agent-overflow/frontend/src/test/helpers/{chat.ts,scrollDom.ts,browserFrames.ts}`, `/home/rmurphy/repos/agent-overflow/frontend/src/test/mocks/{StubVirtualizer.svelte,virtuaMarkRecorder.ts}`, `/home/rmurphy/repos/agent-overflow/frontend/src/test/integration/virtua-patch-fixtures/VirtuaBufferRetentionHost.svelte`.

result: Scroll-test surface mapped — 399 tests across 10 files classified OUTCOME/IMPL/MIXED with provenance (6 bug-report captures, 2 verbatim user reports), 27 must-survive contracts, 11 coverage holes, a Chromium streaming-outcome harness sketch (buildPane already browser-capable; ssrCount + setupFiles are the only gaps), and a 2-line change to gate the currently-ungated 2.4s browser suite.
