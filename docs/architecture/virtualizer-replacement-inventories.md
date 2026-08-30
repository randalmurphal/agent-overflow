# Bespoke Timeline Virtualizer: Evidence Inventories

Companion to [virtualizer-replacement-plan.md](virtualizer-replacement-plan.md).
Two investigations run 2026-07-02 on branch `virtualizer/bespoke-engine`
(main at `90ba7f77`, the merged scroll re-architecture):

- **Part A** covers upstream `virtua` anatomy, from the source checkout
  at `/home/rmurphy/repos/virtua`, tag `0.49.1` (commit `4d7002d`, MIT).
- **Part B** is the exhaustive app-side touchpoint inventory: every
  prop, handle call, patch seam, test, and DOM contract that the
  replacement must satisfy, delete, or leave alone.

## Validation notes (read before trusting either part)

Both reports were spot-checked against the actual sources. Corrections
and dispositions on their judgment calls:

1. **Part A §6.1's suffix-sum suggestion is REJECTED.** The report
   suggests a bottom-anchored engine should flip the offset store to
   suffix sums so "tail mutations don't invalidate." The verified math
   says otherwise: `setItemSize` invalidates via
   `_computedOffsetIndex = min(index, watermark)` (cache.ts:51), i.e.
   only offsets *after* the changed index. Tail mutations (the 60Hz
   streaming path) therefore invalidate almost nothing today, and
   appends are `push` + lazy extension. It is *head prepends* (rare,
   user-triggered load-older) that pay an O(window) memo rebuild, and
   the window is bounded (hundreds to low thousands of rows). Suffix
   sums would invert the cost onto the hot path. **Decision: keep
   top-anchored prefix sums; bottom-anchoring lives in the policy layer
   (mount seeding, pin semantics, compensation-as-observation), not in
   the coordinate system.** The two prepend fixes worth taking are the
   ones Part A also identifies: replace the O(n·k) `unshift` loop
   (cache.ts:24–30) with a single splice, and don't reset the watermark
   to −1 when a cheaper `min()` bound is available.
2. Part A's LOC counts, ACTION constants, `_frozenRange` mechanics, and
   the `ACTION_MANUAL_SCROLL = 7` non-export were re-verified against
   the checkout and are accurate.
3. Part B's "no other production mount" claim was re-verified:
   `shared/VirtualList.svelte` (homegrown fixed-height list) and
   `utils/diffSidebarVirtualizer.svelte.ts` (IntersectionObserver) exist
   and are unrelated to virtua; ChannelView has zero virtua references.
4. Part B's cutover-surface summary is adopted verbatim as the plan's
   handle-parity contract (plan §4).

---

# Part A: upstream virtua 0.49.1 anatomy

Verified against `/home/rmurphy/repos/virtua` at tag `0.49.1` (commit
`4d7002d`). LOC totals: **src/core = 3,198 total / 1,821 excluding
specs** (cache.spec.ts 1348, store.spec.ts 29); **src/svelte = 859
total / 784 excluding VList.ssr.spec.ts**.

## 1. File-by-file map

| File | LOC | Responsibility |
|---|---|---|
| `src/core/cache.ts` | 234 | The size/offset model: parallel `_sizes[]` / `_offsets[]` arrays with lazily-extended memoized prefix sums, binary search (`findIndex`), range computation with locality hints (`computeRange`), median-based default-size estimation, length-change (append/prepend/remove) bookkeeping, snapshot take/restore. Pure functions over a plain object; imports only `types.js` + `utils.js`. |
| `src/core/store.ts` | 477 | The state machine. `createVirtualStore` (store.ts:118) closes over cache + scroll state (`scrollOffset`, `jump`, `pendingJump`, `_flushedJump`, `_scrollDirection`, `_scrollMode`, `_frozenRange`, `_prevRange`, `isSSR`, `_isViewportMeasured`) and exposes read accessors, `_flushJump`, a bitflag pub/sub, and a single `$update(ACTION_*, payload)` reducer. All jump/compensation *decisions* live here; the DOM write does not. |
| `src/core/scroller.ts` | 645 | DOM scroll side: `createScrollObserver` (scroll/wheel/touch listeners, 150 ms debounced scrollend, and `_fixScrollJump`, the one internal scrollTop write), `createScrollScheduler` (async measure-converge loop behind scrollTo/scrollBy/scrollToIndex), `createScroller` (element scroller, the only one the app uses), plus `createWindowScroller` (419–586) and `createGridScroller` (600–645), both irrelevant. |
| `src/core/resizer.ts` | 293 | ResizeObserver plumbing: `createResizer` (46–89, the one used), plus window (100–155) and grid (160–293) variants, which are irrelevant. |
| `src/core/environment.ts` | 45 | `isIOSWebKit()` UA/maxTouchPoints detection (29–38), `isSmoothScrollSupported`, multi-document `getCurrentWindow/Document` helpers (iframe-safe RO construction). |
| `src/core/utils.ts` | 57 | `clamp`, `sort`, `microtask`, `createPromise` (deconstructed promise), `once`, `NULL = null` (minification aliases). |
| `src/core/types.ts` | 44 | `ItemResize`, `ItemsRange`, `InternalCacheSnapshot` (`[sizes[], defaultSize]`), branded public `CacheSnapshot`, `ScrollToIndexOpts`. |
| `src/core/index.ts` | 26 | Barrel; does **not** export `ACTION_MANUAL_SCROLL`, which is why the app's patch hardcodes the literal `7`. |
| `src/svelte/Virtualizer.svelte` | 192 | The adapter: instantiates store/resizer/scroller, mirrors store into one `stateVersion` rune, derives range/indexes, renders keyed `ListItem`s, runs `$fixScrollJump` in a post-render `$effect`, exports the ~10 handle methods. |
| `src/svelte/ListItem.svelte` | 62 | One absolutely-positioned row: `top: {offset}px`, registers itself with the resizer in an `$effect`, `visibility:hidden` while unmeasured. |
| `src/svelte/Virtualizer.type.ts` | 136 | Props + `VirtualizerHandle` docs (the 10 handle methods). |
| `src/svelte/VList.svelte` / `VList.type.ts` | 88/32 | Convenience wrapper adding the `overflow-y:auto; contain:strict` viewport div; pure delegation. App doesn't use it. |
| `src/svelte/WindowVirtualizer.svelte` / `.type.ts` | 148/94 | Window-scroll variant. Irrelevant. |
| `src/svelte/utils.ts` / `types.ts` / `index.ts` | 13/7/12 | `styleToString`, `defaultGetKey` (`"_" + i`), barrel. |

## 2. The size/offset cache

**Data structure** (cache.ts:14–22). A plain object of two parallel
number arrays plus a dirty watermark:

- `_sizes: number[]` holds the measured size per index, with an
  `UNCACHED = -1` sentinel (cache.ts:9); reads fall back to
  `_defaultItemSize` (cache.ts:35–38).
- `_offsets: number[]` holds memoized prefix sums, valid only up to
  `_computedOffsetIndex`.
- `_computedOffsetIndex` is the watermark of how far the prefix sums
  are valid; `-1` = nothing computed.

**Operations & complexity:**

- `getItemSize` is O(1) (cache.ts:35).
- `setItemSize` is O(1); invalidates by
  `_computedOffsetIndex = min(index, _computedOffsetIndex)`
  (cache.ts:43–53); returns whether it was the initial measurement.
- `getItemOffset` is a memoized prefix-sum extension: walks from the
  watermark to the requested index, filling `_offsets` as it goes, then
  advances the watermark (cache.ts:58–82). Amortized O(1) for repeated
  nearby queries, O(n) worst-case after invalidation. Guards
  `_offsets[0] = 0` to avoid a NaN→infinite-rerender loop
  (cache.ts:67–72, PR #160).
- `findIndex` is a binary search over computed offsets with optional
  `[low, high]` bounds (cache.ts:89–107). Each probe calls
  `getItemOffset`, so a cold cache pays one O(n) fill, then O(log n).
- `computeRange` is locality-optimized: compares `prevStartIndex`'s
  offset to the target and searches only forward or only backward,
  reusing the found `end`/`start` as the bound for the second search
  (cache.ts:112–132). Also clamps `prevStartIndex` for shrunk lists
  (cache.ts:118–119).
- `updateCacheLength`: append pushes `UNCACHED`. **Prepend (isShift)
  uses `unshift` in a loop** (cache.ts:24–30, called at cache.ts:221),
  which is O(n·k) for k prepended rows, and it **discards the entire
  offset memoization** (`_computedOffsetIndex = -1`, cache.ts:212–214).
  Returns the signed size delta (estimated for `UNCACHED`) which
  becomes the scroll jump (cache.ts:222, 226–233).
- `estimateDefaultItemSize` is the median of all measured sizes
  (cache.ts:137–170). **Inactive in the app**:
  `shouldAutoEstimateItemSize` is `!itemSize` (Virtualizer.svelte:49)
  and the app passes `itemSize=56`.
- `takeCacheSnapshot` returns `[sizes.slice(), defaultSize]`
  (cache.ts:198–200); restore via `initCache` third arg with
  length-mismatch tolerance (cache.ts:182–188, issue #441).

**Extractability: excellent.** Zero DOM dependency, imports only
`types.js`/`utils.js`, and `cache.spec.ts` (1,348 LOC) tests it in
isolation, so the spec suite ports with it. On lift, fix the
`unshift`-loop prepend and the full offset invalidation on shift (see
validation note 1: stay top-anchored; make prepends one splice).

## 3. The store state machine

**Actions** (store.ts:41–55) and what each mutation does (`$update`
reducer, store.ts:255–475):

| Action | Effect |
|---|---|
| `ACTION_SCROLL = 1` (store.ts:261–319) | New scroll offset from the DOM. Early-breaks on same offset **in native mode only** (262–265). This is the early-break the app's patch defeats with a manual-mark. Computes `isJustJumped = flushedJump && distance < abs(flushedJump)+1` (267–277; +1 tolerates subpixel loss from integer scrollTop writes) to avoid latching a bogus direction from its own compensation write. Latches `_scrollDirection` from delta **only when `_scrollMode === SCROLL_BY_NATIVE`** (279–285), the "unmarked programmatic write = user scroll" classification. Clears `isSSR` (299–301). Skips virtual-state update outside elastic-scroll bounds (306–317). `shouldSync = distance > viewportSize` (316). |
| `ACTION_SCROLL_END = 2` (320–329) | Resets `_scrollDirection = SCROLL_IDLE`, `_scrollMode = SCROLL_BY_NATIVE`, `_frozenRange = null`, and flags the pendingJump flush. This is why `markProgrammaticScroll()` must be re-fired before *every* write. The mark dies at scrollend. |
| `ACTION_ITEM_RESIZE = 3` (331–417) | Batch of `[index, size]`. Filters no-ops; computes a compensation jump via the `shouldKeep` matrix (343–377): keep-all under `SCROLL_BY_SHIFT`; keep-if-before-frozen-range under manual smooth scroll (#380/#758); otherwise keep if the item is fully above the visible start (not scrolling down, native, per #385/#865) or above-start-and-not-spanning (868). Then writes sizes into the cache (381–390), optionally runs median estimation (393–406), `shouldSync = true` (416). |
| `ACTION_VIEWPORT_RESIZE = 4` (419–428) | Sets `viewportSize`; first ever measure sets `_isViewportMeasured` and forces sync. |
| `ACTION_ITEMS_LENGTH_CHANGE = 5` (429–441) | `payload = [length, isShift]`. With shift: `applyJump(updateCacheLength(..., true))` and `_scrollMode = SCROLL_BY_SHIFT` (430–433). Without: just resize the cache. |
| `ACTION_START_OFFSET_CHANGE = 6` (442–445) | Sets `startSpacerSize` (the `startMargin` prop). |
| `ACTION_MANUAL_SCROLL = 7` (446–449) | Sets `_scrollMode = SCROLL_BY_MANUAL_SCROLL`, and that is the entire body. This is what the app's `markProgrammaticScroll()` patch fires. Does **not** bump `stateVersion`. |
| `ACTION_BEFORE_MANUAL_SMOOTH_SCROLL = 8` (450–454) | Sets `_frozenRange` to the destination range so those rows pre-mount and pre-measure before a smooth scroll. App never uses smooth. |

**Jump pipeline** (no `_flushDelayedJumps` symbol in 0.49.1; the pieces
are):

- `applyJump(j)` (store.ts:168–182): routes compensation into
  `pendingJump` (deferred) when iOS-WebKit-while-scrolling or
  frozen-range-during-manual-smooth-scroll; otherwise into `jump`.
- `_flushJump()` (store.ts:243–247): moves `jump` → `_flushedJump`,
  returns `[jump, isShiftMode]`. Called only by the scroller's
  `_fixScrollJump` (scroller.ts:157).
- `pendingJump` flush: on scrollend (store.ts:322–324) merged into
  `jump` at 460–463. `pendingJump` is also *subtracted* from
  `getItemOffset` and *added* into `getVisibleOffset` while pending
  (store.ts:149, 155) so geometry stays coherent before the flush.
- **Frozen render**: while `_flushedJump` is set, `$getRange` returns
  `_prevRange` verbatim (store.ts:201–204, issue #597). Render stays
  frozen until the compensation write's scroll event arrives and clears
  it (267–268).

**Direction tracking drives** (`SCROLL_IDLE/DOWN/UP`, store.ts:24–30):

1. **Directional buffer drop** in `$getRange` (store.ts:210–219): when
   direction ≠ DOWN, extend `startOffset` backward by `bufferSize`;
   when ≠ UP, extend `endOffset` forward. A latched DOWN drops the
   entire *backward* overscan, which is the source of the app's settle
   flicker with unmarked writes.
2. iOS jump deferral (store.ts:170–177).
3. `$isScrolling()` → `pointer-events: none` on the container
   (store.ts:239; Virtualizer.svelte:169).
4. Scrollend gate for flushing pendingJump (store.ts:322–325).

**150 ms timings** live in scroller.ts: the debounced synthetic
scrollend (`debounce(..., 150)`, scroller.ts:76–88, which *re-arms
itself* while `wheeling || touching`), the wheel-continuation window
`150 > timeDelta && 50 < timeDelta` (scroller.ts:118–127), and the
measurement-wait cancel timer in the scroll scheduler
(`timeout(cancelScroll, 150)`, scroller.ts:214).

**Shift prepend, end-to-end:** adapter detects `data.length` change in
`$effect.pre` and dispatches `ACTION_ITEMS_LENGTH_CHANGE [len, shift]`
(Virtualizer.svelte:119–123) → store enters `SCROLL_BY_SHIFT` and
accumulates a jump equal to the estimated prepended height
(store.ts:430–433; cache.ts:218–222) → subsequent real measurements of
the new rows also produce keep-distance-from-end jumps because
`SCROLL_BY_SHIFT` forces `shouldKeep = true` (store.ts:344–350) →
adapter's post-render `$effect` calls `$fixScrollJump`
(Virtualizer.svelte:131–136) → scroller writes **absolute**
`scrollTop = store.$getScrollOffset() + jump` (scroller.ts:348–353,
discussion #475: absolute, not `scrollBy`, to avoid exceeding
scrollable bounds) and cancels any scheduled scroll (354–357, issue
#357). If content is shorter than the viewport, the jump can't cause a
scroll event, so `_fixScrollJump` dispatches `ACTION_SCROLL` manually
to unfreeze (scroller.ts:162–166). `SCROLL_BY_SHIFT` resets to native
at scrollend (store.ts:327).

**Pub/sub**: subscribers register with a bitmask target
(`UPDATE_VIRTUAL_STATE/SIZE/SCROLL/SCROLL_END`, store.ts:71–77); every
mutating action bumps a wrapping int32 `stateVersion` and notifies
matching subscribers with a `sync` hint (store.ts:457–474). The Svelte
adapter ignores the sync hint entirely (Virtualizer.svelte:53–61).
The flushSync commentary is React-only.

## 4. createResizer (resizer.ts:46–89)

- **One ResizeObserver for everything.** Viewport root and all rows
  share a single RO; constructed lazily on first `_observe` via
  `getCurrentWindow(getCurrentDocument(e)).ResizeObserver`
  (resizer.ts:10–30), lazy for SSR and window-scoped for iframes
  (Chromium bug 1491739).
- Element→index mapping is a `WeakMap` (resizer.ts:52), written in
  `$observeItem` (79–85), which returns an unobserve closure.
- **Entry batching**: the RO callback partitions entries in one pass.
  The viewport entry dispatches `ACTION_VIEWPORT_RESIZE`, row entries
  accumulate into one `ItemResize[]` dispatched as a single
  `ACTION_ITEM_RESIZE` (resizer.ts:54–73). One store mutation per RO
  flush.
- **`display:none` guard**: entries whose target has no `offsetParent`
  are skipped (resizer.ts:58), because zero-sized rects from hidden
  subtrees would otherwise poison the cache.
- **No initial-measure skip**: the initial RO callback *is* the initial
  measurement; `setItemSize`'s return value distinguishes initial vs
  re-measure (cache.ts:48). Uses `contentRect[sizeKey]`, which is
  content-box, so row borders/margins are invisible to the cache.
- **Worth stealing: essentially all of it.** ~45 LOC, correct, minimal.
- Fragility note: `_unobserve` is `ro!.unobserve(e)` (resizer.ts:23–24),
  a non-null assertion that throws if item cleanup runs when the RO was
  never constructed.

## 5. createScroller (scroller.ts:289–414)

- **Scroll event handling** (`createScrollObserver`, scroller.ts:56–169):
  `scroll` listener stamps `lastScrollTime`, dispatches `ACTION_SCROLL`
  with the normalized offset, re-arms the debounced scrollend (90–103).
  `wheel` (passive) purely *infers* that scrolling is still in progress
  when frame drops swallow scroll events. It sets `wheeling` if a wheel
  arrives 50–150 ms after the last scroll event with nonzero delta on
  the scroll axis (107–130); the scrollend debounce refuses to fire
  while `wheeling || touching` (77–83). `touchstart`/`touchend`
  (passive) track finger state; on iOS WebKit, `touchend` sets
  `justTouchEnded` so the next scroll event marks
  `stillMomentumScrolling` (132–141, 93–95).
- **The `$fixScrollJump` write site** is the exact code the app's
  `setScrollApplier` patch replaces: `_fixScrollJump`
  (scroller.ts:156–167) flushes the jump and calls
  `updateScrollOffset`, whose element-scroller implementation is
  scroller.ts:334–358: iOS momentum hack (set `overflow` to `hidden`
  for one macrotask to kill momentum before writing, 339–346), then
  **`viewport[scrollOffsetKey] = normalizeScrollOffset(store.$getScrollOffset() + jump, isNegative)`**
  (350–353), then `cancelScroll()` if shift (354–357). Plus the
  shorter-than-viewport manual `ACTION_SCROLL` fallback (162–166).
- **scrollTo/scrollBy/scrollToIndex** all funnel through
  `createScrollScheduler` (scroller.ts:178–277), an async convergence
  loop: await `initialized` promise (element may attach late, PRs
  #733/#750); non-smooth path loops { mark `ACTION_MANUAL_SCROLL`,
  write scrollTop, await either a size event (target got measured →
  offsets moved → retry) or the 150 ms cancel timer (settled) }
  (256–271). The 150 ms timer is only armed when `viewportSize` is
  nonzero (212–215, issue #450). Smooth path first sets the frozen
  range, waits until every item in range is measured, then marks manual
  and calls native `scrollTo({behavior:"smooth"})` (224–254, issue
  #590). This async-retry shape is exactly why the app couldn't reuse
  these for per-frame pin writes and needed the patch.
- **`scrollToIndex`** (377–409): clamps index, resolves `nearest` by
  comparing item bounds to the viewport (380–395), computes the target
  as `offset + startSpacerSize + itemOffset + align adjustment`,
  recomputed lazily via `getTargetOffset()` on each retry so it tracks
  re-measurements.
- **RTL/negative offsets**: `normalizeScrollOffset` + `isNegative` from
  computed `direction: rtl` (scroller.ts:42–54, 325–327), dead weight
  for the app.

## 6. Top-anchored assumptions inventory (the parts a bottom-anchored engine does NOT port)

1. **Prefix sums from index 0 downward** (cache.ts:67–82) are
   coordinate-agnostic in principle; see validation note 1 (KEPT, with
   prepend-cost surgery only).
2. **The entire jump-compensation pipeline**:
   `jump`/`pendingJump`/`_flushedJump`/`isJustJumped`/frozen render
   (store.ts:130–132, 168–182, 201–204, 243–247, 267–277, 460–463)
   plus the write site (scroller.ts:156–167, 348–357) exists solely
   because above-viewport size changes move top-anchored content under
   a fixed scrollTop. This is what the `setScrollApplier` patch
   reroutes today and what the bespoke engine replaces with "emit a
   compensation observation; controller decides."
3. **`shouldKeep` matrix** (store.ts:341–378): every branch is "is
   this item above the visible start?" geometry.
4. **`shift` prepend machinery**: `SCROLL_BY_SHIFT` mode,
   unshift-based cache fill, full offset invalidation,
   keep-distance-from-end via jumps (cache.ts:24–30, 212–233;
   store.ts:344–350, 430–433; scroller.ts:162–166, 354–357).
5. **`startMargin`/`startSpacerSize`** (store.ts:148, 233, 442–444;
   scroller.ts:398–400). App doesn't use it.
6. **`$getRange` window math** with the *directional* buffer trim
   (store.ts:206–224) that caused the buffer-drop flicker.
7. **`scrollToIndex` align math** from top offsets
   (scroller.ts:397–408).
8. **Row positioning `top: offsetpx`** (ListItem.svelte:43–57) and
   **container `height: totalSize`** growing downward
   (Virtualizer.svelte:161–171). Note the RTL-horizontal path already
   computes end-anchored offsets, where `getItemOffset(index, fromEnd)`
   = `totalSize − offset − size` (store.ts:154–160). That is a tiny
   existing proof that end-anchored positioning slots into the same
   cache.
9. **`overflow-anchor: none`** (Virtualizer.svelte:164). Virtua must
   opt out of native scroll anchoring because its own compensation
   would fight it. The bespoke engine makes this decision deliberately
   (and keeps the opt-out: OUR controller is the anchor authority).
10. **SSR freeze semantics** (§7): seeded range `[0, ssrCount-1]` is
    top-anchored ("first N items").

## 7. ssrCount: exactly what it changes

1. `isSSR = !!ssrCount` (store.ts:125) and the initial range seed
   `_prevRange = [0, max(ssrCount-1, 0)]` (store.ts:136).
2. `$getRange` returns that seeded `_prevRange` verbatim while
   `!_isViewportMeasured || isSSR` (store.ts:192–198), so exactly the
   first `ssrCount` items mount regardless of viewport size.
3. `isSSR` is cleared in exactly one place: the first `ACTION_SCROLL`
   (store.ts:299–301). In happy-dom (no real layout, no scroll events)
   the range stays frozen at `[0, ssrCount-1]` indefinitely, and that
   determinism is why it works as a test harness knob.

Svelte wart: the Svelte `ListItem` has no `_isSSR` handling, so
SSR-rendered rows carry `visibility:hidden` (contrast
react/ListItem.tsx:47–51). Fine for DOM-count assertions. The bespoke
engine's test seam should be a first-class render-all mode instead.

## 8. Svelte adapter

**Subscription / rerender granularity** (Virtualizer.svelte:44–96): the
adapter collapses the whole store into **one** `$state` number
(`stateVersion`) and derives `range`, `isScrolling`, `totalSize`,
`indexes` off it. Per-item `offset`/`hide` are recomputed inline in the
`#each` for every rendered index on every store mutation (186–187), a
whole-window recompute, with Svelte's keyed-each + prop-equality
diffing limiting DOM touches to rows whose values changed.

**Positioning**: absolute, no translate, no spacers. Container:
`position:relative; height:{totalSize}px; contain:size style;
overflow-anchor:none; flex:none; pointer-events:none-while-scrolling`
(161–171, PRs #775/#800). Rows: `position:absolute; top:{offset}px;
width:100%; contain:layout style; visibility:hidden-until-measured`
(ListItem.svelte:43–57).

**Lifecycle / teardown** (Virtualizer.svelte:98–136): `onMount` defers
element attachment behind `tick().then(...)` (104–111, issues
#603/#690). `onDestroy` runs `store.$dispose()` → `resizer.$dispose()`
→ `scroller.$dispose()`. Post-render `$effect` calls
`scroller.$fixScrollJump()` once per version change (131–136).

**Fragile spots (teardown TypeErrors), the do-not-reproduce list:**

1. **Unguarded `tick().then` mount race** (Virtualizer.svelte:105–111):
   no destroyed flag; unmount before the tick resolves →
   `container.parentElement` is null → TypeError chain into
   resizer/environment; even without a throw, four listeners attach
   *after* `$dispose` ran (leak) and the `initialized` promise
   re-resolves against a dead element.
2. **`ro!.unobserve`** (resizer.ts:23–24) non-null assertion; RO
   construction throws in environments without ResizeObserver.
3. **Scheduler leak on dispose**: `scroller.$dispose` never calls
   `cancelScroll` (scroller.ts:363–368); an in-flight measurement-wait
   loop with `viewportSize === 0` has no cancel timer and its
   subscription was wiped. The promise never settles, pinning the
   closure forever.
4. **`elementRef` non-null assumption** in ListItem's `$effect`
   (ListItem.svelte:29, 37), an unchecked invariant.

## 9. Steal-vs-skip verdict

### Lift near-verbatim (MIT, attribute to inokawa/virtua)

- **`cache.ts` (234 LOC) + `cache.spec.ts` (1,348 LOC)** is the crown
  jewel; coordinate-agnostic; port the spec suite with it. Surgery:
  one-splice prepend, cheaper shift invalidation (validation note 1).
- **`createResizer` shape** (resizer.ts:10–89): lazy single RO, WeakMap
  indexing, one-batch dispatch, `offsetParent` guard. Change: emit
  measurement observations; mind content-box vs border-box.
- **`environment.ts:29–38`**: `isIOSWebKit()` verbatim.
- **Style contracts**: `contain: size style` + `overflow-anchor:none` +
  `flex:none` on container; `contain: layout style` +
  `visibility:hidden`-until-measured on rows; `pointer-events:none`
  while scrolling (each line is a shipped bug fix, #775/#800).
- **Scroll-observer edge-case kit** (scroller.ts:56–146): 150 ms
  self-re-arming scrollend debounce gated on `wheeling||touching`, the
  50–150 ms wheel-continuation inference, passive listener set. Lift
  these as *observations feeding the controller*, not behavior.
- **`isJustJumped` ±1px subpixel tolerance** (store.ts:270–277). Any
  engine that ever compensates scrollTop needs this self-echo
  classifier.
- **Store skeleton pattern** (monotonic int32 `stateVersion` + bitmask
  subscriber targets) and the single-rune adapter binding.

### Read for edge-case knowledge only

- **`shouldKeep` matrix** (store.ts:341–378) with its issue
  bibliography (#380, #385, #758, #865, #868): the case taxonomy of
  when resize compensation is owed.
- **Scroll scheduler convergence loop** (scroller.ts:178–277): how to
  scroll to an unmeasured target; corner cases #450, #590, #733, #750.
- **iOS momentum overflow-hidden hack** + iOS jump deferral. Desktop
  webview doesn't need them today; keep the reference.
- **Elastic-scroll bounds guard** (store.ts:306–317) and
  shorter-than-viewport jump fallback (scroller.ts:162–166).
- **`estimateDefaultItemSize`**: median approach, if flat-estimate
  ever goes adaptive.
- **Svelte adapter teardown bugs** (§8): the do-not-reproduce
  checklist.

### Skip: the top-anchored machinery being replaced

The entire jump pipeline; `SCROLL_BY_SHIFT` + shift prepend; direction
latch + directional buffer drop + native/manual `ScrollMode`
classification (replace with explicit intent from the controller, which
already knows user-vs-programmatic); frozen range / smooth scroll;
`startMargin`; RTL/horizontal; `keepMounted`; SSR freeze; window/grid
scrollers and resizers; VList/WindowVirtualizer.

**Bottom line**: ~280 LOC of core (cache + resizer pattern + env
detection + style contract) lifts nearly clean with its 1,300-line test
suite; the scroll-observer timing heuristics and the `isJustJumped`
predicate port as observation logic; everything else (roughly the
whole of store.ts's mutation logic and scroller.ts's write paths, i.e.
the machinery both patch hunks exist to subvert) is the top-anchored
compensation engine the bespoke design deletes rather than
re-implements.

---

# Part B: app-side touchpoint inventory

All paths relative to the repo root. Classification: **MIGRATES** (needs
an equivalent on the new engine's handle/props), **DELETES** (exists
only because virtua is a second writer / opaque store), **STAYS**
(engine-agnostic).

## 1. Imports & package plumbing

**Runtime imports (production):**
- `frontend/src/lib/components/chat/MessageTimeline.svelte:3` has
  `import { Virtualizer, type VirtualizerHandle } from 'virtua/svelte'`,
  the ONLY production value import. **MIGRATES.**

**Type-only imports (production):**
- `frontend/src/lib/utils/threadVirtuaSizeCache.ts:69` has
  `import type { VirtualizerProps }` (derives `VirtuaCacheSnapshot`,
  line 75). **DELETES** (whole module replaced by live per-item
  priors).
- `frontend/src/lib/components/chat/messageTimelineTrace.ts:6` has
  `import type { VirtualizerHandle }` (trace signature, line 37).
  **MIGRATES** (retype to new handle).

**Test imports:**
- `frontend/src/lib/components/chat/virtuaShiftCache.test.ts:24-27`
  imports `createVirtualStore, ACTION_ITEMS_LENGTH_CHANGE` from
  `'virtua/unstable_core'`. **DELETES.**
- `frontend/src/test/integration/virtua-patch-fixtures/VirtuaApplierHost.svelte:12,23`
  and `VirtuaBufferRetentionHost.svelte:12,22` use the real
  `Virtualizer` + handle type. **DELETES.**
- `frontend/src/lib/components/chat/messageTimelineVirtuaMarking.test.ts:16-18`
  does `vi.mock('virtua/svelte', …)` → StubVirtualizer.
  **DELETES/rewrites** against new handle.

**Manifests:**
- `frontend/package.json:41` pins `"virtua": "0.49.1"`. **DELETES.**
- `frontend/pnpm-workspace.yaml:4` lists `patchedDependencies`.
  **DELETES.**
- `frontend/pnpm-lock.yaml` holds lockfile entries + patch hash.
  **DELETES.**
- `frontend/patches/virtua@0.49.1.patch` (two logical hunks):
  1. **manual-scroll marking** exports `markProgrammaticScroll()` =
     `store.$update(7)` so the controller's sync per-frame pin writes
     aren't classified as user scroll-downs (which latch a direction
     and drop the entire backward buffer).
  2. **scroll-applier seam**: `setScrollApplier(fn)` routes
     `$fixScrollJump`'s direct scrollTop write to an external applier
     (absolute target, raw `(jump, shift)`, synchronous; return true =
     you wrote, false = core pokes its own store at the current DOM
     offset so a decline never desyncs the model).
  Both hunks **DELETE**, because their *semantics* (single writer,
  programmatic-vs-user classification, head-shift compensation) are
  exactly what the bespoke engine must provide natively.
- **No hits** in `Makefile`, root `package.json`, scripts, or
  `frontend/vite.config.ts` / `frontend/vitest.config.ts`.

## 2. Component usage: the single production mount

`frontend/src/lib/components/chat/MessageTimeline.svelte:1966-2094`,
inside `{#key pane.threadId}` (1965/2121) inside `contentEl` (1961).
**Verified: no other production mount.** ChannelView.svelte has zero
virtua references; `shared/VirtualList.svelte` is a homegrown
fixed-height virtualizer (sidebar-scale lists) and
`utils/diffSidebarVirtualizer.svelte.ts` is an IntersectionObserver
file-level virtualizer, both unrelated (**STAY**). Only the two
browser-test fixtures mount additional Virtualizers.

Props (line refs in MessageTimeline.svelte):

| Prop | Line | Value / purpose | Classification |
|---|---|---|---|
| `bind:this={listRef}` | 1967 | imperative handle (`$state`, line 200) | MIGRATES |
| `scrollRef={scrollEl}` | 1968 | external scroll container (MessageTimeline owns scrolling; virtua only measures) | MIGRATES |
| `data={revealedNodes}` | 1969 | reveal-gated grouped `TimelineNode[]` (derivation 261-294) | MIGRATES |
| `cache={virtuaReplayCache}` | 1970 | measured-size snapshot, **consumed once at construction** (comments 203-211, 1954-1960) | DELETES (live priors) |
| `shift={virtualizerShiftAtHead}` | 1971 | head-mutation hint = `pane.pendingTimelineShiftAtHead \|\| timelineWindowPruneShiftAtHead` (297-299) | MIGRATES (head-splice semantics) |
| `getKey={(node) => timelineNodeKey(node)}` | 1972 | stable structural key (`utils/subagentGrouping.ts:465`) | MIGRATES |
| `itemSize={ESTIMATED_ROW_SIZE}` | 1973 | 56px initial estimate (const line 84) | MIGRATES (prior fallback) |
| `bufferSize={BUFFER_SIZE_PX}` | 1974 | 1800px overscan each side (const line 92, flicker rationale 85-91) | MIGRATES |
| `ssrCount={IS_TEST ? 100_000 : undefined}` | 1975 | happy-dom render-all shim (`MODE==='test' && 'happyDOM' in window`, 166-176); Chromium keeps real windowing | MIGRATES (first-class render-all test seam) |
| `onscroll={handleVirtuaScroll}` | 1976 | snapshot persist + auto-load older/newer (1120-1131) | MIGRATES |
| `onscrollend={handleVirtuaScrollEnd}` | 1977 | snapshot persist + row-UI prune (1133-1136); virtua synthesizes scrollend 150ms after last scroll | MIGRATES |
| `children` snippet `(node, index)` | 1979-2093 | row template; no `as`/`item` wrapper overrides used | MIGRATES |

Not used anywhere: `horizontal`, `overscan`, `startMargin`,
`keepMounted`, `scrollBy`, `getItemSize` (only the stub exports it).

## 3. Handle-method call sites (production), grouped by feature

**Bottom-pin / restore:**
- `MessageTimeline.svelte:464` calls `scrollToIndex(lastIndex, {align:'end'})`
  in `notifyHostLayoutSettled` (host-layout re-pin when sticky).
  **MIGRATES**
- `:468` calls `scrollTo(listRef.getScrollOffset())` for the non-sticky
  host-layout reconcile: rewrites the SAME offset to force virtua to
  re-sync rendered range to host geometry. **DELETES** (virtua-model
  resync; new engine exposes an explicit "revalidate geometry" entry
  instead).
- `:1518-1523, 1556-1624`: the bottom restore path deliberately uses
  `forceStick()` NOT `scrollToIndex(end)` (two-writers oscillation,
  comment 1540-1544). **STAYS**

**Timeline-window prune anchor transaction
(`preserveTimelineWindowAnchor`, 472-599):**
- `:487-493` calls `captureTimelineAnchor(revealedNodes, currentListRef,
  currentListRef.getScrollOffset(), {clampIndex:true})`, which uses
  `TimelineGeometry` = `{findItemIndex(offset), getItemOffset(index)}`
  (`timelineScroll.ts:15-18,110-125`; anchor =
  `{itemId, offsetTop: getItemOffset(idx) - offset}`). **MIGRATES**
- `:528` calls `scrollToIndex(lastIndex, {align:'end'})` restore-bottom
  after prune. **MIGRATES**
- `:541-544` calls `scrollToIndex(idx, {align:'start',
  offset:-anchor.offsetTop})` restore-anchor after prune. **MIGRATES**

**Load-older / load-newer anchor math:**
- Load-older (`:1718-1736`) does **no** handle call. Position is held
  entirely by the `shift` prop (comment 1710-1717). **MIGRATES** as
  shift semantics.
- `:1752` is the manual Load-newer button: `scrollToIndex(lastIndex,
  {align:'end'})`. **MIGRATES**
- Auto-load triggers: `:1158` `list.findItemIndex(offset)` (top zone),
  `:1194` `list.findItemIndex(edge.bottomProbeOffset)` (bottom zone),
  both deferred thunks behind cheap gates
  (`timelineScroll.ts:147-170,186-195`). **MIGRATES**

**Scroll-to-item / search / restore-anchor:**
- `:1699` calls `scrollToIndex(idx, {align:'start', offset:-snap.offsetTop})`
  (thread-switch anchor restore).
- `:1820` calls `scrollToIndex(idx, {align:'center'})` (`scrollToItem`,
  driven by `pane.scrollToItemRequest` nonce effect 1825-1832;
  publisher: `MessageSearch.svelte:150-158`). **MIGRATES**

**Snapshot persistence:**
- `:1249` `getScrollOffset()` + `:1253` `captureTimelineAnchor(...)`
  live in `saveScrollSnapshotForThread` (TypeError-swallowing teardown
  guard 1244-1264, a virtua-specific "inner ref nulls mid-teardown"
  wart; **DELETES** the guard, **MIGRATES** the capture).

**Row-UI prune visible range (`currentVisibleTimelineRange`, :1027-1047):**
- `:1036` `getViewportSize()`, `:1038` `getScrollOffset()`,
  `:1039-1040` `findItemIndex(offset)` / `findItemIndex(offset+viewport)`
  are cached-geometry-only reads (explicit "no clientHeight forced
  layout" rationale). Another TypeError teardown guard (1043-1046).
  **MIGRATES** (range query), guard **DELETES**.

**Size-cache persist:**
- `:1329` `getScrollSize()` (O(1) change gate), `:1331` `getCache()`.
  **DELETES.**

**Dev diagnostics:**
- `:901-902` calls `getScrollSize()` + `getCache()` in
  `captureTimelineGeometry` (pane geometry probe; reads `cache[0]` as
  sizes array, coupled to virtua's opaque `[sizes[], estimate]`
  shape). `utils/paneGeometryProbe.ts:57-89`. **MIGRATES in spirit**
  (new engine exposes per-index size introspection) / shape
  **DELETES**.
- `messageTimelineTrace.ts:105-107` calls
  `getScrollOffset/getScrollSize/getViewportSize` in render trace.
  **MIGRATES** (rename).

**Patch wiring:**
- `:421` sets `onBeforeScrollTopWrite: () => listRef?.markProgrammaticScroll()`.
  **DELETES.**
- `:951-953` runs `$effect(() => { listRef?.setScrollApplier(stick.applyVirtuaScrollCompensation); })`.
  **DELETES** (see §5).

## 4. The size-cache replay dance (all DELETES: replaced by live per-item priors)

1. **Store** lives in `frontend/src/lib/utils/threadVirtuaSizeCache.ts`:
   `VirtuaCacheSnapshot` (75), key `{width, structureSig, expansionSig}`
   (78-82), `setThreadVirtuaSizeCache` (113, LRU re-insert),
   `getReplayableVirtuaCache` (129, exact-match-or-undefined + LRU
   bump), `clearThreadVirtuaSizeCache` (140), test helpers (144, 151),
   `MAX_ENTRIES = 50` (98). Header (1-67) documents why replay is safe
   (virtua 0.49.1's resize handler no-ops equal sizes) and the
   deliberately-unkeyed display-settings residual.
2. **Key construction** happens in `MessageTimeline.svelte:1285-1291`
   `currentVirtuaSizeKey()`: `width = Math.round(scrollSurfaceContentWidth)`
   (fed only by the async RO in `scrollSurfaceWidth.ts` via effect
   695-704), `structureSig = timelineStructureSignature(revealedNodes)`
   (`utils/timelineStructureSignature.ts`, a positional encoding
   *because virtua's size cache is index-addressed*, line 29),
   `expansionSig = pane.expansionSignature()`
   (`stores/threadRowUiState.svelte.ts:52`).
3. **Capture** is `maybePersistVirtuaSizeCache()` (:1315-1341), gated by
   `restoredThreadId === threadId`,
   `getScrollSize() !== lastPersistedScrollSize` (:1298, reset on
   threadId edge :1354), TypeError teardown skip; then `getCache()` +
   `setThreadVirtuaSizeCache`. Triggers: co-located with every
   `saveScrollSnapshot()` (:1219-1230) **and** the isWarm rising-edge
   effect (:1373-1379, `lastWarmForCapture`, `untrack`ed).
4. **Replay** uses the `virtuaReplayCache` `$state` (:209-210), resolved in
   `$effect.pre` on the threadId edge only (:1350-1358,
   `virtuaReplayCacheThreadId` dedupe :210) *before* the
   `{#key pane.threadId}` remount, because `cache` is constructor-once;
   passed at :1970. Mismatch ⇒ `undefined` ⇒ flat 56px estimate.
5. **Invalidation** happens at `stores/threads.svelte.ts:53` (thread delete),
   `stores/thread.svelte.ts:1776` (same-thread reswitch /
   revert-to-checkpoint), `:3260` (`removeItemById`), `:3294`
   (`removeItemsFromTurn`). Store-side note at
   `thread.svelte.ts:1748-1754`.
6. **Consumers/tests** are `scroll.test.ts:203-325` (persist + ABA replay
   + structureSig stability), `threadVirtuaSizeCache.test.ts`,
   `thread.svelte.test.ts:41-45`,
   `threadRowUiState.svelte.test.ts:802-804`,
   `setup.browser.ts:20,70`.

Inputs the new priors system must reproduce: per-index measured heights
valid under (width, node sequence + leaf content, expansion state);
output consumed at mount to skip the estimate→measure cascade. The
warm-up hide gate (`hideContentForWarmup`, :631-632) and
`armWarmupWithReset` (:431-434) are the cascade-masking companions.
They **STAY** while any estimate-based mounting exists, otherwise
shrink.

## 5. Scroll-controller seams encoding virtua-specific behavior

**The applier pipeline (DELETES as a virtua seam; re-lands as the
engine's native compensation observation):**
- `utils/scroll/index.svelte.ts:450-479` defines
  `applyVirtuaScrollCompensation(target, jump, shift)`: builds
  `VirtuaCompensationObservation` `{target, jump, shift, scrollTop,
  bottomTarget: targetScrollTop(), clientHeight, widthReflowActive}`
  (:452-460), delegates to the pure resolver, applies via
  `writeScrollTop` (:477), returns false to decline (detached ⇒
  decline, :448-449). Exported :884.
- `utils/scroll/resolver.ts:410-517` holds `resolveVirtuaCompensation`.
  Decision tiers (provenance at 422-453): ① `shift` verbatim write
  (:497); ② `!warm || !isAtBottom || escaped || paused` → write (:500);
  ③ **anchor-redirect**, where DOM already pinned && target moves
  meaningfully above bottom → write `bottomTarget` instead, caller
  `'virtua.anchorRedirect'` (:503-509, cold-switch flicker 8bf8b97f);
  ④ width-reflow window active → write (:510); ⑤ spring active &&
  |jump| ≤ clientHeight → **decline** (:513-514, mid-stream
  settle-flicker snap; decline is safe only because the patch pokes
  virtua's store); ⑥ default write (:516). Types (:466-491).
- `utils/scroll/types.ts:18`: the `ScrollWriteCaller` union includes
  `VirtuaWriteCaller`; `:214-227` `applyVirtuaScrollCompensation` on
  the controller interface.
- Tiers ①-④'s *outcomes* (head-shift anchors hold; reading-position
  stability; never paint a frame short of the bottom while pinned;
  width-reflow lands same-paint) become internal requirements of the
  new engine, pinned by `compensationOutcome.browser.test.ts`.

**Programmatic-write marking:**
- `utils/scroll/types.ts:270-298` carries the `onBeforeScrollTopWrite`
  contract (must-not-throw, fires even on no-op writes, per-write
  because virtua clears the mark on scrollend). Invoked at the
  chokepoint `index.svelte.ts:293-296` immediately before the write
  (:297) and the tag `intent.noteProgrammaticWrite(taggedTop)` (:301).
  Wired in `MessageTimeline.svelte:411-422`. **Option + wiring
  DELETE**; the chokepoint + tagging **STAY**.

**runExternalScroll wrappers:**
- Impl `utils/scroll/intent.ts:388` (doc `types.ts:148-159`). All 6
  production call sites are in MessageTimeline (:454, :526, :540,
  :1698, :1751, :1819), each wrapping a `listRef.scrollToIndex` /
  `scrollTo`. Rule codified in `components/chat/AGENTS.md:21-24`.
  **MIGRATES if** the new engine's scroll-to-index writes through its
  own path; **DELETES if** scroll-to-index becomes a controller
  primitive (preferred, because external-writer tagging then has no
  remaining consumer in chat).

**Host-layout retry ladder (MessageTimeline :436-470, adapter
:601-617):**
- `hostLayoutRetryToken`/`retryHostLayoutSettled` (:436-444) retries
  `notifyHostLayoutSettled` up to 2 rAFs while `listRef` is unbound;
  then `runExternalScroll(preserveIntent)` doing
  sticky→`scrollToIndex(end)`+`markAtBottom`, else
  `scrollTo(getScrollOffset())` self-rewrite (:454-469).
  `paneScrollController.observe('host-layout')` routes here (:605-617;
  kind defined `utils/scroll/types.ts:35-50`, default instant re-pin
  `index.svelte.ts:711-723`). Trigger: `PaneHost.svelte:133-165`, a
  double-rAF after pane reorder (`paneOrderKey`), because insertBefore
  leaves "virtua's rendered range out of sync with the pane's
  scrollTop" (:136). **Ladder + offset-rewrite DELETE**; the *pane-move
  → revalidate* hook itself **MIGRATES** (new engine needs an explicit
  revalidate/reanchor entry).
- H1/H2/M1-named structural-nudge machinery: **not present** under
  those names; the retry ladder above plus the live-follow structural
  nudge (:706-825, `markStructuralContentPending` + delayed
  `observe('live-content')`) are what exists. The live-follow nudge is
  content-driven, **STAYS**.

**Shift choreography (MIGRATES as head-splice semantics):**
- `stores/thread.svelte.ts:490-507` holds the
  `pendingTimelineShiftAtHead` contract; set/reset around loadOlder
  head-grow (:2863-2885) and loadNewer tail-grow-then-head-prune split
  across two flushes (:3077-3098) so one `shift` boolean can represent
  each mutation.
- `MessageTimeline.svelte:295-299, 510-522` holds the
  `timelineWindowPruneShiftAtHead` one-flush mark, driven by
  `isPureKeyedHeadDrop` (`timelineScroll.ts:127-139`).

**Other resolver/observer/intent tiers that exist because of virtua
(mostly STAY but re-justify):**
- `resolver.ts:291-297, 376-382` holds the overshoot guard /
  negative-delta chase defense against "virtua applyJump
  mis-corrections". It **STAYS** structurally (browser clamping is
  engine-independent); the virtua branches can simplify.
- `intent.ts:545-556` is the 1ms deferred scroll-classification gate.
  It **STAYS** (still needed for RO-correlated layout scrolls) with
  reduced load.
- `observers.ts:87-97` has `SETTLED_QUIET_MS`/`WARMUP_SETTLE_EPSILON_PX`
  calibrated to the 56px estimate cascade; `:158` `widthReflowActive()`
  is exported *as a virtua-compensation input*, so the export
  **DELETES** with the seam while the window itself **STAYS**.
- `spring.ts:48-57, 454-457` holds token/decline-tier commentary;
  oscillationSnap (:348) is motivated by virtua row-remount clamps. The
  kinematics **STAY**.
- `utils/springAnimationLatch.ts:15,32-35` has provenance comments only.
  **STAYS.**

## 6. Test infrastructure

**Virtua-coupled (DELETE or rewrite at cutover):**
- `src/test/mocks/StubVirtualizer.svelte` fakes the whole
  `VirtualizerHandle` as zero/no-ops, renders all rows flat; records
  `markProgrammaticScroll` into `src/test/mocks/virtuaMarkRecorder.ts`.
- `components/chat/messageTimelineVirtuaMarking.test.ts` closes the
  onBeforeScrollTopWrite→handle seam via the stub.
- `components/chat/virtuaShiftCache.test.ts` drives the REAL
  `virtua/unstable_core` store to tripwire `shift` cache-unshift
  semantics (deliberately version-coupled, header 1-27). Replace with
  equivalent head-splice tests on the new engine's size store.
- `src/test/integration/virtua-patch-buffer-retention.browser.test.ts`
  + fixture is the patch hunk 1 drop-rule guard.
- `src/test/integration/virtua-patch-scroll-applier.browser.test.ts`
  + fixture is the patch hunk 2 guard.
- `MessageTimeline.svelte:166-176 + 1975` is the happy-dom `ssrCount`
  render-all shim. **MIGRATES**, because the new engine needs the same
  render-all test seam.
- `scroll.test.ts` (44 mentions) is happy-dom integration against REAL
  virtua in render-all mode; size-cache persist/replay tests (:203-325)
  delete with the cache; most other tests assert controller/snapshot
  behavior and **survive** with comment updates.
- `paneGeometryProbe.test.ts:30` (`virtuaScrollSize` field),
  `thread.svelte.test.ts:41-45`, and
  `threadRowUiState.svelte.test.ts:802-804` are size-cache-adjacent,
  adjust with §4.

**Engine-agnostic (STAY as-is, the cutover safety net):**
- `components/chat/streamingOutcome.browser.test.ts` is outcome-only
  (contracts C1/C9/C16), relative thresholds, no mechanism spies;
  explicitly written "so the suite survives every stage of the scroll
  rewrite". Unmount-batch counting works against any windowing engine.
- `components/chat/remountReturn.browser.test.ts` covers
  scroll-away/return outcomes (no scrollHeight dips, no scrollTop
  reversals, bounded unmount batches, clean landing).
- `components/chat/compensationOutcome.browser.test.ts` pins the
  three above-viewport-growth outcomes (pinned tail never moves;
  reading anchor holds; huge correction snaps in one paint). Scenario
  names virtua but assertions are pure outcome, so this is the spec for
  the new engine's internal compensation.
- `components/chat/rowMarginContainment.browser.test.ts` is the CSS
  containment guard; the mounted stand-in hardcodes
  `contain: layout style` as "virtua's item wrapper stand-in", which
  stays if the new engine's item wrapper keeps `contain: layout`, else
  update the stand-in.
- `src/test/helpers/timelineBrowserHarness.ts` is the real-Chromium
  harness; virtua-aware waits (`:265`, `:273`, `:128` 150ms scrollend
  debounce constant) need constant/name updates only.
- `components/chat/timelineScroll.test.ts`,
  `utils/scroll/index.svelte.test.ts` (74 mentions, where the
  onBeforeScrollTopWrite hook-ordering tests delete),
  `utils/scroll/resolver.test.ts` (35 mentions, where the
  `resolveVirtuaCompensation` matrix deletes/renames; the content-delta
  matrix stays).
- `frontend/vitest.config.ts` has the unit/browser project split
  (`:45-113`); no virtua reference; **STAYS**.
  `src/test/setup.browser.ts:20,65-70` clears the size cache per test
  and deletes with §4.

## 7. Row/DOM contracts the new engine must honor (verified: all STAY)

- **`[data-row-index]` wrapper** lives at
  `MessageTimeline.svelte:1995-1996`; consumed by `paneGeometryProbe`
  (`:916`), `timelineRowElementForIndex` (`timelineScroll.ts:95-100`),
  diagnostic traces, and browser tests. Codified
  `components/chat/AGENTS.md:42-45`.
- **`data-item-id` only on TimelineLeaf's root**, at
  `TimelineLeaf.svelte:56`; structural rows deliberately unanchored.
- **`data-row-geometry-content`** wrapper at `:2016`, paired with
  `frontend/src/app.css:341` `display: flow-root` (BFC contains
  trailing row margins; settle-flicker fix). The margin-divergence
  oracle `startVirtuaMarginDivergenceTrace`
  (`messageTimelineTrace.ts:189-241`, effect :992-995) watches the
  `contain: layout` item wrapper vs the row. The *invariant* (measured
  row total === row content box) transfers to whatever wrapper the new
  engine emits.
- **scrollEl styles** (:1894-1908): `overflow-anchor: none` (:1899),
  `overscroll-behavior-y: contain` (:1898),
  `scrollbar-gutter: stable both-edges` (:1900, WebKitGTK column-shift
  rationale :1878-1890), composer-clearance
  `padding-bottom = var(--composer-height) + 16px` **on scrollEl not
  contentEl** (:1901), paint-only top fade mask (:1902-1903).
- **contentEl** (:1961-1963) is the controller's content-RO target;
  `contentEl.scrollHeight` must equal the engine's totalSize exactly
  (comment :1949-1951; virtua achieves it via
  `contain: size; height: totalSize`). Warmup hides via `visibility`,
  not display (:1963).
- **`{#key pane.threadId}` remount block** (:1965/2121) resets engine
  row-size state per thread; MessageTimeline itself (and
  scrollEl/contentEl) persist across switches, and the
  restore/`armWarmup` `$effect.pre` choreography (:1395-1474) depends
  on that split.
- **Row-shell stability rules** (AGENTS.md:46-56): stable outer shell
  after first render, row state in pane registries keyed by item id, no
  `smooth:true`, no `scrollIntoView`.
- **pointer-events during scroll** does **not** exist in the app
  today (virtua applies it internally; verified no app-side toggling).

## 8. Everything else

**Comment-only mentions** (no code dependency, update wording at
cutover): ~35 files, list captured 2026-07-02; notable stale item:
`UserMessage.svelte:92-95` says "bufferSize=900" but the constant is
1800px.

**Docs referencing virtua** (rewrite at V3): frontend-scroll.md,
scroll-contracts.md, scroll-rearchitecture-plan.md,
scroll-rearchitecture-inventories.md (§A4 maps the minified core names
for patch re-rolls, obsolete at V3), settle-flicker-analysis.md
(historical, annotate only), chat-rewrite.md,
docs/specs/tool-call-ui-redesign/README.md, frontend/AGENTS.md (scroll
section + Vendor Patches §virtua), components/chat/AGENTS.md,
internal/store/AGENTS.md (incidental).

**Build/scripts:** no virtua references in Makefile or scripts; nothing
outside `frontend/`.

## Cutover surface summary

The new engine's handle must provide: `scrollToIndex(index, {align:
'start'|'center'|'end', offset})`, `getScrollOffset()`,
`getViewportSize()`, `getScrollSize()`, `findItemIndex(offset)`,
`getItemOffset(index)`, plus prop-equivalents for `data`/`getKey`/
`bufferSize`/external-scrollRef/`shift`(head-splice)/`onscroll`/
`onscrollend`, a render-all test seam (ssrCount replacement), and
per-index size introspection for the geometry probe. It must NOT need:
`cache` replay (live priors), `markProgrammaticScroll`,
`setScrollApplier`, `scrollTo(currentOffset)` self-resync, the
TypeError teardown guards, or the pnpm patch. The five outcome
invariants in `compensationOutcome`/`streamingOutcome`/`remountReturn`
browser tests plus the row/DOM contracts in §7 are the acceptance gate
that survives the swap unchanged.
