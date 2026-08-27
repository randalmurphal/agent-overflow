# Perf investigation reference

Measured on the 2026-08-22/23 investigation (Windows 11, WebView2, window 2560x1369, dev build). Numbers are floors to subtract, not targets. Append to the ledgers at the bottom when an investigation moves them.

## Floors

Renderer ("Agent Overflow (dev)") at first start, two panes visible: 112-128MB private. cc/tile_memory 69MB, v8 29MB, blink_gc 23MB, malloc 36MB, partition_alloc 11MB, gpu/transfer_buffer 16MB.

- Raster tiles are visible content only. Until 2026-08-23 `7b29f9d6` every visible timeline plane was its own composited layer: ~7MB (3 tiles of 896x704) per plane, 2.4MB per visible activity run, 13.8MB for the root layer (4 tiles of 2560x352), 0.2-0.9MB per Overlap-promoted fade or divider, 54-64MB active in total. That commit deleted the `will-change` promotion, so timeline content paints into the root layer: the promoted-element census fell from ~40MB of estimated texture over five planes to 0.6MB, renderer `cc/tile_memory` from 60-86MB to 27.5MB, and the GPU process from 297-336MB to 198-217MB (the last pair is flattered by six panes on a fresh start against 13-17 panes on an hour-old one). Panes scrolled out of the strip hold no tiles (cc's policy is ALLOW_PREPAINT_ONLY with a 512MB soft limit, and it only keeps NOW and SOON bins). memory-infra's `cc/tile_memory` also counts the pool of recently freed tiles that streaming re-raster double-buffers; the GPU process mirrors the same bytes as `gpu/shared_images`.
- blink_gc (Oilpan) committed is page fragmentation, not live data and not a leak. Measured 2026-08-24 on the post-`7b29f9d6` build: `probe memdump` reported 117.8MB committed against 24.4MB of objects, and the detailed dump's per-page rows put 920 pages at 21% fill, with 303 of them (39.4MB) completely empty and only 41 over three-quarters full. Sweeping empties objects out of pages, the pages stay committed, and fresh allocation takes new pages instead of refilling sparse ones. Two class censuses 28 minutes apart settle it: committed went 90.5 to 109.9MB while objects went 19.9 to 15.4MB over the same 1208 classes, every top class flat or shrinking (`PlaneRootTransform` 6397 to 461 instances). Objects down, committed up, so nothing is retained. The garbage pile is a sawtooth — Chromium purges on page-hidden and memory pressure, only a memory-reducing GC returns pages, and the peak is set by the churn rate — but the SURVIVOR-pinned floor under it RATCHETS (next bullet).
- The committed floor that survives memory-reducing GCs is pinned pages, and the pinning population is attributable per page. `probe blinkpages` right after a trim (2026-08-25): `committed=178.9MB allocated=20.5MB fragmentation=88 pooled=4.5MB` — pool retention is nothing, 98% of NormalPageSpace0's 37.25MB held 0.45MB of objects (Space1 27.1/1.5, Space2 32.4/4.7, Space3 39.4/7.1, CustomSpace3 19.25/0.26). A memory-reducing GC returns only fully-empty 128KB pages; a page with one long-lived survivor stays forever, cppgc compacts only backing-store spaces, so the floor ratchets with session age: post-trim committed went 153→178→240MB over 80min of fleet load THROUGH five trims, live flat at ~20. The census's per-page type rows name the pinners: ~57 isolated SVG document hosts' per-window singletons (each spread over ~28 pages — killed by `5ccf3a2e`, see Fixed causes) and the CSS value caches in CustomSpace3 (`CSSNumericLiteralValue` 4132 objs/112 pages, `CSSUnparsedDeclarationValue` — custom-property values — 2187/133).
- Ordinary major GCs run about every 10 seconds in this app (`probe frames`: 2 MajorGC in 20s, 5.5ms total, 2.8ms worst, plus 189 incremental marking steps at 0.7ms max). They sweep; they do not return pages. Any proposal that works by making major GCs more frequent is answering the wrong question.
- Detached DOM nodes hover around 3.2k in a dev build and stay flat. A step up that persists after a pane closes is the leak signal.

GPU process: about 185-230MB at first start and it does not fall below that. Roughly 100-120MB is fixed (D3D11/ANGLE driver heap, DirectComposition swap chain at 2560x1369 ≈ 14MB per buffer, skia GPU cache ≈ 16MB, shared images ≈ 13MB, transfer buffers), 40-50MB is heap slack, and cc/resource_memory (tiles, 40MB at start) scales with composited planes. It is not attributable to app code below that line.

Steady state with a restored 12-pane strip (2026-08-25, dump at 290.5MB private): tracked allocators total ~146MB — malloc 98.9 (win_heap 38.7 + partitions 50.3 + metadata/fragmentation caches 10.5), cc/resource_memory 16.8, gpu 15.7 (shared_images 13.4, shader_cache 2.2), skia 13.5, shared_memory 1.2 (plus one 16MB renderer↔GPU transfer segment). The other ~145MB is invisible to memory-infra: D3D11 usermode driver + DirectComposition. The band is a floor, not growth — a fresh restart re-entered 246-280MB within minutes, identical to the 5-hour-old process, and the app-drivable slice (tiles + shared images + skia ≈ 45MB) is already post-`7b29f9d6` minimal. Peaks to 340-440MB are the documented transient driver staging (line below). No app-side lever cuts the steady band without rastering less.

Empty profile (fresh `make soak` wipe, zero threads, 2026-08-25): renderer 35MB, GPU 77MB, group 153MB. The app existing costs ~150MB group-wide; everything above that under load is attributable.

Browser process ("Manager"): 40-55MB, mostly IndexedDB/leveldb and malloc. Network, Storage, Crashpad: under 10MB each.

Go backend: 13MB live heap on 6363. Lean; one confirmation profile per investigation is enough.

## Interpretation

- Task Manager's column is private working set; memory-infra `private_kb` matches it within a few MB.
- JS is under 2% of wall time in steady state. Per-frame cost is native: the reveal smoother, the 8Hz ambient ticker, the ~20fps sprite, the spring, and the composited layer set (25-27 layers, 14-15 promoted by Overlap).
- `PlaneRootTransform` was the top Oilpan churn class until 2026-08-23 `7b29f9d6` (21.6 of 28 MB/min, 1,300 allocations/s). Chromium `main` (`geometry_mapper_transform_cache.cc` `Update`) calls `MakeGarbageCollected<PlaneRootTransform>` on every cache regeneration, both for a flat non-2D-translation node and for every 2D-translation node under one, with no reuse. Any transform or clip node change anywhere bumps the global generation, and the next paint, hit test or IntersectionObserver query re-allocates for every node it walks. The app fed that loop through `.scroll-composited-content { will-change: transform, translate, rotate }`, which made every timeline plane a flat non-2D-translation node over a subtree of 2D-translation descendants, with the spring writing a transform every glide frame. Removing the rule removed the feed — at idle. An earlier claim that the class was "absent from churn now" was an idle-only measurement: WHILE SCROLLING it was still 72% of Oilpan churn on 2026-08-24 (26.44MB/min, `scrolldrift` probe), because every `<svg>` root whose rendered size differs from its viewBox qualifies as such a node (`NeedsReplacedContentTransform` gives it a scale) and the app mounted ~400 scaled lucide roots; scroll-driven paint-property updates bump the generation. Fixed 2026-08-24 by converting lucide to CSS-mask spans and matching MeterRing's viewBox to its rendered box (see Fixed causes).
- DevTools `HeapProfiler.collectGarbage` (what the between-turns trim fires) is `v8::debug::ForceGarbageCollection(isolate, kNoHeapPointers)` → `isolate->LowMemoryNotification()` — the memory-reducing path, Oilpan included, verified in v8 `src/inspector/v8-heap-profiler-agent-impl.cc` + `src/debug/debug-interface.cc` (2026-08-25). Committed pages surviving it are pinned (previous bullet), never "wrong GC type".
- A `memdump` trigger unit that auto-restarts fires a DETAILED dump every restart while the process sits above its threshold — each one walks every heap in every process and is user-visible as lag + CPU (2026-08-25: 65s cadence during a 380MB plateau was the user's "laggy feeling"). A threshold trigger is a one-shot forensic; after it fires, stop the unit or raise the threshold, and leave only light polling (`probe sample`) standing.
- `Runtime.queryObjects`, the only way to census detached nodes from outside, runs `CollectAllAvailableGarbage` before it answers (v8 `src/profiler/heap-profiler.cc`: "we should return accurate information about live objects, so we need to collect all garbage first"). That is the memory-reducing collection, Oilpan included, so a poll loop calling it holds the renderer at a floor it never reaches on its own and hides every peak between ticks. Measured 2026-08-23: a 2-minute `sample --detached` loop pinned the renderer at 256-281MB across 100 minutes of real use, while the same build sat 600-700MB group-wide unmeasured. Footprint curve and retention census are separate runs.
- In a heap snapshot, detachedness is a node field (`detachedness === 2`). The `Detached ` name prefix is absent in current snapshots; a census keyed on it reports everything attached.
- A retaining path `system / Context → <moduleVar$1> → reactions → derived` means a module-scope svelte signal still holds a reaction from a dead component. Read the derived's `parent` chain: an effect with `fn === null` was destroyed, so the derived outlived its owner and is held only through the signal.

## Red/green recipe for a svelte patch hunk

Prove the bug on the previous patch, the fix on the new one, without touching node_modules by hand:

1. Keep copies of the new patch and lock outside the tree (`cp frontend/patches/svelte@*.patch frontend/pnpm-lock.yaml /tmp/...`).
2. `git show HEAD:frontend/patches/svelte@<v>.patch > frontend/patches/svelte@<v>.patch`, same for `pnpm-lock.yaml`, then `cd frontend && pnpm install --offline`. Run the suite: it must fail.
3. Copy the saved files back, `pnpm install --offline`, run again: green.

Edit a hunk with `pnpm patch svelte@<v> --edit-dir <dir>` (applies the existing patch first) and `pnpm patch-commit <dir>`; only the patch hash changes in the lock. `svelte/internal/client` exports `get/set/state` in its types but `derived/effect/effect_root` only at runtime (import the namespace and cast).

## The present-policy mechanism, measured (2026-08-24)

`scripts/perfprobe/present-policy-arms.mjs` + `present-policy-page.html`. Synthetic
timeline, repeated compensated head splices, three-plus arms. Headless +
SoftwareRenderer + one raster thread, so only the arm-to-arm comparison counts.

The Print Doctrine's conclusion is right and its named mechanism was wrong. There
is no smoothness-priority flip: `tree_priority` stays `SAME_PRIORITY_FOR_BOTH_TREES`
in every arm (that mode is pinch / active compositor scroll). What an active
animation changes is the DRAW DRIVE — `SetNeedsOneBeginImplFrameOnImplThread`
0 -> 781, `LayerTreeHostImpl::PrepareToDraw` 34 -> ~650 — so the compositor draws on
the frame deadline instead of waiting for raster.

Draws landing while raster is still outstanding, 3 repeats:

| animating elements | 0 | 1 | 3 | 14 | 30 |
|---|---|---|---|---|---|
| draws | 34 | 638 | 637 | 652 | 670 |
| during outstanding raster | 3 | 20 | 15 | 17 | 17 |

**Binary, not proportional.** One animation costs what thirty do; the only
meaningful state is zero. **Document-wide, not scroller-scoped:** an animation
outside the scroller scores the same as one inside (18 vs 23).

**Resolved 2026-08-24, user's call: accept the mode.** Zero is unreachable —
`working-sprite-run`, `ambient-led` and `ambient-spin` run through every working
turn, and `TailClampedText`'s line-slide runs continuously through streaming, and
all four are wanted. Reverting `animate-pulse` alone therefore closed nothing and
charged ~28 whole-document repaints/sec for it, so pulse went back to CSS. Do not
re-propose a middle position — the measurement says there isn't one.

## The ambient indicators drive ~two thirds of the renderer main thread

**Built 2026-08-23, reversed, then re-landed 2026-08-24.** The pulse, LED chase,
stepped spin and sprite are all CSS keyframes (`app.css`), phase-locked to
wall clock by `utils/ambientPhase.ts`. The sprite was the single largest
writer (25/s) and is the bulk of the win; the pulse was ~28 whole-document
repaints/sec on top.

**The rule is opacity-only and stepped, not "no animation objects".** `1633dcea`
disarmed `animate-pulse` on the object-counting theory and was reverted once the
section above measured it. What survives, guarded by
`frontend/src/lib/components/chat/timelineKeyframeAnimations.test.ts`: an
animation reachable from a chat row may animate `opacity` and nothing else (a
transform or size is a third motion owner fighting the scroll controller's
compensation), and anything `infinite` must be stepped, checked over `app.css`
exhaustively because the 2026-07-04 present-rate hazard is document-wide. The
guard reads `@keyframes` bodies and the armed-class set out of `app.css`, so a
new animation fails the build rather than needing review memory.

The status glow is the last indicator still on the JS ticker, and for a property
reason, not a placement one: `box-shadow` is not compositable and opacity alone
cannot reproduce spread GROWTH. The ticker suspends outright when no consumer is
on screen, waking from a MutationObserver, and a glow only appears on a thread
pending user action — so an ordinary session has zero ambient wakeups.

**Promotion cost of arming the dots, measured 2026-08-24** (same headless rig,
`LayerTree.enable` over the `present-policy-page.html` arms, 60-row scroller): a
composited animation promotes its own element and nothing else. Layers went
8 / 9 / 11 / 22 / 38 at 0 / 1 / 3 / 14 / 30 dots — exactly +1 per dot, no overlap
cascade — and every added layer is the dot's own 8x8 box, so estimated texture
went 34.30MB to 34.31MB across the whole sweep. Arming a small opacity animation
is not a tile-memory question; do not re-litigate it as one.

Two findings only the build surfaced: a composited translate resamples the sprite
texture unless the frame width is snapped to whole DEVICE pixels (25px frame at 125%
scaling: 80.8% of pixels resampled), and a filtered ancestor (light mode's
drop-shadow) does NOT de-composite the strip. The section below is the measurement
that motivated the work; keep it for the mechanism, not as a description of current
code.

Measured 2026-08-23 on the live app (`probe frames 20`, trace kept at
`trace-postsvg.json`) plus four isolated Chrome 151 spikes under
`/tmp/svg-chunk-spike/` (throwaway profile, never the app's).

App, 20s while a turn streamed: 967 main-thread animation frames (48/s) but
only 248 rAF callbacks. 1055 of 1084 style invalidations are inline-style
writes from two timers, and they name themselves in the trace:

- `SPAN.working-sprite` 501x (25/s) — `WorkingSprite.svelte` writing
  `background-position-x` at the sprite's own `frameMs`.
- `.animate-pulse` dots 554x (~8 ticks/s x 3.5 dots) — `ambientTicker.ts`.

Each write costs a whole-document lifecycle: 562 Paints of the full
2560x1369 root (mean 364us) and 966 Layerize passes. Layerize is bimodal —
313 under 10us, 577 over 500us, 183 over 1ms — so a real frame is Paint
364us + Layerize ~900us + PrePaint/Commit/Style ~260us, call it 1.5ms.
Paint+Layerize+PrePaint+Commit+UpdateLayoutTree is 1035ms of the 1531ms
RunTask total: **62% of all renderer main-thread work, at 33 ticker ticks
of the 48 frames per second.**

### Why the inline write is expensive and a CSS animation is not

Blink promotes an element for a *known* animation and ticks
opacity/transform on the compositor thread. A one-off inline style write is
just a style change, so it repaints in the document's own layer — which is
why the Paint clip is the whole viewport. `ambientTicker.ts` wrote styles
specifically to avoid CSS animations, and in doing so forfeited the
promotion that made the repaints cheap.

Spike, 600 svg-icon rows inside a `contain:paint` scroller, indicators in
`overflow:hidden` flex rows, 5s wall, main-thread total (Layerize + Paint +
PrePaint + Commit + UpdateLayoutTree):

| indicator drive | style recalcs | main thread |
| --- | --- | --- |
| static floor | 0 | 0.2ms |
| JS ticker, 4 dots (today) | 40 | 58.9ms |
| JS ticker, 40 dots (today) | 40 | 63.4ms |
| CSS `@keyframes` opacity, 4 | **0** | **0.0ms** |
| CSS `@keyframes` opacity, 40 | **0** | **0.0ms** |
| CSS `@keyframes` rotate, 4 | **0** | **0.0ms** |
| CSS `@keyframes` box-shadow, 4 | 324 | 56.6ms |
| CSS `@keyframes` background-position, 4 | 325 | 141.0ms |

The ticker's cost is per tick, not per element: 4 dots and 40 dots cost the
same, because one tick forces one whole-document lifecycle either way.

The 324 recalcs / 5s = 65/s for box-shadow and background-position
reproduces the 2026-07-20 measurement in `ambientTicker.ts`'s header
(65.7/s running vs 7.0/s paused) exactly. **That measurement was right for
the properties it was taken on, and does not hold for opacity or
transform**, which are compositable and cost nothing.

### Both non-compositable visuals re-express at zero

| approach | style recalcs | main thread |
| --- | --- | --- |
| sprite: JS `background-position-x` @25Hz (today) | 125 | 270.1ms |
| sprite: CSS `background-position` keyframes | 325 | 136.8ms |
| sprite: CSS `translateX` strip in an `overflow:hidden` slot | **0** | **0.0ms** |
| glow: JS `box-shadow` @8Hz (today) | 40 | 127.0ms |
| glow: CSS `box-shadow` keyframes | 324 | 85.1ms |
| glow: static `box-shadow` on `::before`, opacity keyframed | **0** | **0.0ms** |

Note the ticker is *worse than the CSS animations it replaced* on total
main-thread time (sprite 270ms vs 137ms), because it traded cheap recalcs
for full-document paints. It won on the metric it measured and lost on the
one it did not.

Every consumer the ticker has is compositable except one: pulse and LED
chase already write `opacity`, `stepped-spin` writes `transform: rotate()`.

The glow is the exception and **should not be re-expressed**. Its
`--ambient-glow-t` drives three things at once in `app.css:876-878` — shadow
spread `0 -> 2px`, shadow alpha `0 -> 0.22`, and `::before` opacity
`0.7 -> 1.0` (which lifts the 1px border with it). Opacity alone cannot
reproduce spread growth: the ring would sit at full 2px and fade in instead
of expanding. Both landed: the glow stayed on the ticker, and the ticker now
suspends itself when no `.status-glow-*` element is in the DOM, waking from a
MutationObserver.

### The 2026-07-04 present-rate incident does not recur, if steps() stays

`app.css`'s `--animate-pulse` note records that the original *smooth*
pulse forced a GPU present every vsync — one 6px dot as a standing 165
presents/sec client on a 165Hz panel, stuttering other apps. Compositing
these animations puts them back on the compositor thread, so that incident
had to be re-measured, not assumed away. Present rate (compositor Swap/s),
same spike rig:

| case | style recalc/s | presents/s |
| --- | --- | --- |
| static floor | 0.5 | 0.2 |
| pulse: JS ticker @8Hz (today) | 8.0 | 8.2 |
| pulse: CSS opacity `steps(8)` | 0.0 | 7.3 |
| pulse: CSS opacity **smooth** | 0.0 | **63.2** |
| spin: CSS rotate `steps(8)` | 0.0 | 8.5 |
| sprite: JS background-position @25Hz (today) | 25.0 | 25.0 |
| sprite: CSS `translateX` `steps(8)` | 0.0 | 8.5 |

The smooth row reproduces the incident exactly (every vsync). **`steps()`
pins the present rate to the art's own frame rate whether JS or CSS drives
the animation**, so the compositable re-expressions present no more often
than today — the sprite still presents at its `frameMs`, the pulse still at
8/s. The win is main-thread work, not present rate. Keep `steps()`.

### It costs no memory

Detailed memory-infra dump per case, same instrument as the app probe:
`cc/tile_memory` 40.98-41.55MB, `blink_gc/main` 12.34-13.06MB,
`gpu/shared_images` 44.00-46.24MB — flat across static, JS ticker, CSS
opacity at 4 dots, CSS opacity at **40** dots, and the translateX strip.
Compositing these indicators promotes nothing measurable.

### Icon representation is a distant second

600 `<svg>` roots add 18.6ms per 40 ticks over a no-icon baseline (0.47ms
per repaint). Two things that were expected to matter, and do not:

- **A scaled viewBox costs the same CPU as an identity one.** `0 0 24 24`
  rendered at 12px measured 44.45ms / 46.26ms across two runs; `0 0 12 12`
  at 12px measured 46.28ms. For repaint TIME the transform node is not the
  cost; the SVG root existing at all is. The scaled node's cost is MEMORY
  CHURN instead: its `PlaneRootTransform` cache is re-allocated per
  paint-property generation bump (see the entry above — 72% of scroll
  churn), which an identity-scale root does not pay. viewBox rewriting is
  worth it for a frequently repainted svg, not for repaint speed.
- `mask-image` spans instead of svg roots save ~37% of the icon overhead
  (38.0ms vs 44.5ms against a 25.8ms floor), not all of it — and drop the
  root out of the SMIL time-container walk and the transform-cache walk
  entirely.

At the app's 396 icons the CPU side is ~15ms/s **only because frames run at
48/s**; the churn side is what got the conversion built (2026-08-24, see
Fixed causes).

### auto-animate cold-polls the sidebar

`@formkit/auto-animate` 0.9.0 (`use:autoAnimate` on `ProjectList.svelte:41`
and `ProjectThreadList.svelte:174`) ran a per-element 2s `poll()`: each
cycle calls `getCoords` (forced layout) then disconnects and rebuilds an
IntersectionObserver. The first trace under-read it at ~2.5ms/s; the
2026-08-24 re-measure put it at 22.5 forced layouts/s, 45 IntersectionObserver
constructions/s and ~11ms/s of style recalc on an idle app, for lists that
change a few times a minute. The library exposes no option to disable the
poll — it is framework-blind by design (MutationObserver sees changes only
after old positions are gone, so it must track continuously). Removed
2026-08-24: the sidebar's two keyed eaches use svelte `animate:flip` +
enter/exit transitions (`utils/sidebarAnimate.ts`), which measure during
reconcile and cost zero at idle.

### Blink retains one edit command per typed character

Every keystroke in a textarea allocates an `InsertIntoTextNodeCommand`
(undo machinery) that Blink retains for the ELEMENT's lifetime — no API
clears it, `value = ''` does not release it (and already discards the undo
stack, measured), and each command can pin a whole 128KB Oilpan page,
because normal spaces are never compacted. Measured 2026-08-24
(`editcmdpages` probe): 383 pages held ONLY by edit commands, 47.9MB at
2.9% fill, ~8KB of committed heap per character ever typed in the app's
lifetime. Growth is strictly typing-driven — a 180s idle watch
(`editcmdgrowth`) added +0 commands / +0 pages. The one release is
replacing the element: the composer swaps its `<textarea>` after every
send (`ComposerInputSurface.recreateInput`, same-flush `{#key}` bump +
refocus, invisible by frame capture), which is the natural boundary since
send already emptied the undo stack. `editcmdpages` is the verification
probe.

## Fixed causes (do not re-derive)

- 2026-08-23 `761452b6`: MessageNavRail ticks rested at `translateY(-50%) scaleX(0.38)`, 540 non-2D transform nodes regenerating per scroll frame (~85KB/frame, ~300MB/min while scrolling). Rest state has no transform now.
- 2026-08-23 `6cbfb341`: `pane.items` is `$state.raw` with per-row boxes; the deep proxy re-minted per-index sources every batch.
- 2026-08-23 `cf33baf2`: dev-only detached-DOM leak from probe Maps, now WeakMaps.
- 2026-08-23 `b0f34fe7`: composer textarea autosizes with `field-sizing: content`; the JS measurement forced two layouts per keystroke.
- 2026-08-23 `3e5984ce`: upstream svelte bug, a reconnecting dirty derived was registered twice on deps new to that run and kept a closed pane's DOM alive through the global `accounts` signal (patch hunk 5, reconnect-dedupe).
- 2026-08-23 `0e6eefc4`: `CommandOutput.svelte` re-splits the command only when its text changes (7MB/min of JS garbage while streaming before).
- 2026-08-23 `7b29f9d6`: `.scroll-composited-content { will-change: transform, translate, rotate }` gave every timeline plane its own composited layer and fed Chromium's `PlaneRootTransform` re-allocation loop. Motion goes through `scrollTop` now; steady-state Oilpan churn fell from 28 to 1.07 MB/min, and `frontend/src/lib/architecture.test.ts` fails any new will-change or content transform on a controller surface.
- 2026-08-24 `acc09802`: lucide icons render as CSS-mask spans (pnpm patch on `@lucide/svelte`), removing ~400 scaled svg roots — the transform-cache churn feed while scrolling and the SMIL walk membership. Verify post-restart with `scrolldrift`.
- 2026-08-24 `54d04e72`: sidebar rows animate with svelte `animate:flip` (`utils/sidebarAnimate.ts`); `@formkit/auto-animate` removed. Idle forced-layout/IO-rebuild/style-recalc cost gone; verify with `frames` on an idle app.
- 2026-08-24 `04c0af7e`: composer swaps its `<textarea>` element after every send (`recreateInput`), releasing Blink's per-character edit-command pages (~48MB after weeks of use). Verify with `editcmdpages` after typing + sending.
- 2026-08-25 `3e249b5a`: **composited pane scrollers** — THE per-frame glide cost and the streaming-load memory refill. The pane timeline scroller had no composited scrolling layer, so every scroll offset change (spring glide write and real wheel alike — the JS write was never the mechanism, wheel-gesture A/B cost the same) ran a full main-frame lifecycle: Layerize ~1.1-1.6ms × 155/s during two-pane glide, 17-25% of main thread, plus the whole GeometryMapper churn family (PlaneRootTransform, ClipCacheEntry, PendingLayer, ScopedForcedUpdate). Fix: `.pane-scroll-surface` in app.css — `will-change: scroll-position` (Blink takes the direct scroll-offset-transform path, `DirectlyUpdateScrollOffsetTransform`) + `scrollbar-width: none`, with `OverlayScrollbar` mounted as the surfaces' scrollbar (intent stated via `setEscapedFromLock`/`forceStick`; the native `scrollbar-gutter` reservation retired). Applies to MessageTimeline and ChannelView. Verified in-code on the rebuilt soak, two-pane glide: Layerize 1922→**84ms/12s**, main busy 340→**139ms/s**, Paint 57ms/12s, raster 220 tasks/10ms; tiles 9.3MB per pane scroller (36.1MB total active incl. 13.8MB root), matching the 9-14MB/pane prediction. Checkerboard hunt came up EMPTY: 12,000px/s input flings over 6000px legs (`probe scrollgesture <s> <i> <sel> fling`) and a 13,700px instant teleport to bottom (`probe jumpbottom`) all screenshot fully painted at t+60ms. Width structurally stable: `offsetWidth − clientWidth` = 0 on overflowed panes, so the first-overflow transition moves nothing. `architecture.test.ts` carve-out documents exactly this one will-change value (scroll-position promotes the scrollTop chokepoint's own mechanism; the ban targets TRANSFORM promotion — a second paint position). User-approved 2026-08-25.
- 2026-08-25 `5cc379f0`: **composited activity-run + subagent clips** — same mechanism one level down. While a long run streams, the clip glides with the follow spring and each scrollTop write ran a full Layerize. A/B/A on the soak with a prose-stripped long-run scenario (edit `~/.agent-overflow-soak/soak-scenario.json` to drop the text-message groups from the repeat step; original kept as `soak-scenario.orig.json`): Layerize 121/127 → 46 → 152ms per 12s. Both clip surfaces now carry `.pane-scroll-surface` (the duplicate `.activity-run-clip` scrollbar block deleted). Tile cost ~5MB per VISIBLE overflowing clip only — 2 of 14 mounted clips held tiles (NOW/SOON bins). No visible change (clips already drew the overlay bar at zero width). **Exonerated of the same-day follow-death report:** the user hit dead bottom-follow + paging ping-pong on this build; a live `will-change: auto !important` csshold reproduced the bug identically, so composited scrolling was ruled out. Actual cause: overlapping auto-load trigger zones under a degenerate outer range (giant single run), pre-existing, fixed `b08be13b` (`autoLoadZonesDisjoint`). Sibling in the same incident family, fixed `a4c9bed3`: the paged (click-driven) prunes evicted the on-screen conversation tail at MAX_ITEMS because a giant run makes item count a broken proxy for screens-of-content; they now tolerate the 1600 hard ceiling while the streaming prune keeps 800→500, so steady-state memory is unchanged and back-paging holds at most ~1-3MB of extra summary rows until settle.
- 2026-08-24 `7c70256d`: MeterRing viewBox matches its rendered 28px box, so the header rings are identity-scale svgs and their dashoffset ticks stop regenerating a scaled node's transform cache (default zoom only).
- 2026-08-25 `facc92a4`: the debug provider-events log (`AGENT_OVERFLOW_DEBUG` provider topic) embeds provider frames as `json.RawMessage` instead of re-escaping every byte into a quoted string — was 242MB/16min, ~24% of backend allocation, all in `json.appendString`. Verify with a heap `alloc_space` profile: `LogProviderEvent` should no longer show `appendString`.
- 2026-08-25 `04e5c74c`: `HighlightPatchTextPrimed` memoizes spliced-document parses per call (sha256 of splice bytes). When a patch matches the file — every persist-tap prime does — all H new-side splices are the identical full file content and were each parsed separately: 591MB/10min of agent edits, 37% of backend allocation, 100% of it under `Cache.PatchWithContext` (attribute with `pprof -peek`). Verify with an alloc profile after a restart: `PatchWithContext`'s share should roughly halve.
- 2026-08-25 `f3552fb8` + `da78203a`, both REVERTED the same day by `3e9bf20b`: two attempts to de-promote the ambient pulse dots' composited layers, and both were the trade this file's first ruling forbids. The trigger was a COUNT — 18 of the app's 26 layers were 6px dots under fleet load — with no cost behind it, and the cost was already measured two sections up: +1 layer per dot, no overlap cascade, 34.30 → 34.31MB of texture across a 0→30 dot sweep. What the flips bought instead: root custom properties invalidated style for the WHOLE document (381 passes × ~3,500 el × 22-30ms per 10s, ~195ms/s) and dropped the live app to 2fps springs; per-element inline opacity then cost ~31ms/s on the live app — 358 recalc passes, 181 Layerize (301ms), 244 Paint, 181 PrePaint/Commit per 20s, ~three quarters of the idle renderer main thread, driven entirely by an otherwise-idle 8Hz timer. The pulse is a `steps(8, jump-none)` CSS animation again. TWO rules, both already provable from this file before either flip: (1) never write an animated custom property on the document root — root writes are for one-shot, rare values only (the theme rewrite goes through a `<style>` element for the same reason); (2) an inline style write costs one WHOLE DOCUMENT lifecycle per tick regardless of element size or count (4 dots and 40 dots both 58.9ms/5s vs 0.0ms composited), so a compositable property belongs in a CSS animation and a layer census is not a reason to take it out of one. Cost layers, do not count them.
- 2026-08-25 `b69f858d`: the Codex resume rollout tail is armed-on-relevance instead of unconditional (store query for incomplete subagent launches, or a live `registerChildOwnership`), and its partial-line buffer reuses backing capacity with a 64KiB shed on line completion — the fresh copy per 150ms tick was ~28MB/h. Upstream PR adding `experimentalRawEvents` to `thread/resume` remains the clean kill.
- 2026-08-25 `5ccf3a2e`: all four mask-icon producers (the @lucide/svelte patch, ToolKindIcon, both brand marks) reference `<mask>` elements in a hidden same-document sprite `<svg>` (`utils/maskSprite.ts`; the patch carries its own copy of the registry) instead of per-icon data-URI mask-images. Each distinct data URI cost an isolated SVG document — internal page + LocalDOMWindow + singleton roster — and those ~57 documents' tiny long-lived singletons were the top identified pinning population of the Oilpan committed floor (each singleton type spread over ~28 near-empty pages). Element masks are mask-type:alpha, objectBoundingBox units, content scaled to the unit square; paint verified pixel-identical in the exact production shorthand (`spritecheck3`/`spritecheck4`). Verify post-restart: `Memory.getDOMCounters` documents ≈ 2-6 (was ~58), then `probe blinkpages` post-trim for the committed floor delta.
- 2026-08-25 `c776ab8c` + `271bc36d`: between-turns memory-reducing GC. 10s input-quiet in the frontend → `RequestWebviewMemoryTrim` (LocalOnly; skipped during active provider turns, 4min floor) → ephemeral loopback-only `webview:trim` directive → the Windows launcher fires `HeapProfiler.collectGarbage` via the fork's `CallDevToolsProtocol` (branch `ao-beta-memory-trim`, 2d9e0221958f). Send is input and open turns skip, so the earliest fire is ~10s after the last turn completes — active sessions return to floor after every turn. Windows/WSL reach only — native desktop and `--connect` emit into silence.

- 2026-08-25 soak streaming floor (post pulse-revert, `make soak`, 3 subagents streaming, zero input): main-thread busy 6.8ms/s, Oilpan churn 2.06MB/min, style recalc ~7.5 passes/s covering exactly the 14 ambient indicator elements (11 pulse dots + 3 LEDs). A `steps()` animation composites but still recalcs its own element's style at each step boundary on main (Interpolation vectors + ComputedStyle in the churn profile) — that is the steps() trade working as designed, ~1ms/s total. The streaming pipeline itself is lean; remaining active-use cost is interaction-driven.
- 2026-08-25 `a56ca00a`: the virtualizer's reading anchor stopped hit-testing. `refreshReadingAnchor` resolved its head anchor via `elementFromPoint` on every scroll/flush (2,090 HitTest events/15s during two-pane streaming); it now derives the anchor row from engine offsets (`headAnchorAt`: `findItemIndex` + `getItemOffset`, verified through `rowElementByIndex`) and samples sub-row rects only at consumption points. HitTest fell 2,090× → 30×/15s (−105ms). The A/B also taught the F1-class lesson properly: the 1,091-el whole-document recalc the anchor read used to flush just MOVED to the next reader (`targetScrollTop`), totals flat — a forced-recalc reader pays for whatever dirt is pending, so the lever is the DIRT'S SCOPE, not who reads first.
- 2026-08-25 `9eaa5465`: three global-sheet selectors with featureless compounds sat in Blink's UNIVERSAL invalidation sets (`allDescendantsMightBeInvalid`), so every sibling-list mutation anywhere scheduled a whole-subtree recalc — the mechanism behind the per-beat 1,091-element document-wide passes during streaming. `.markdown-body > :last-child > :last-child` (131 subtree invalidations/15s), `.run-map-spine > * + *::before` (44/15s with the overlay CLOSED), `> :first-child > :first-child` (12/15s). Fix: a feature in every compound — streamdown's theme stamps `md-blk` on every block-level element (`MD_BLOCK_MARKER`), the run-map connector keys on `li`/`.run-map-node`. Verified same rig, same two-pane burn: biggest recalc pass 1,092 → 44 el, total recalc 55.9k → 27.0k el/15s (−52%), recalc time 413 → 274ms/15s, main-thread busy 353 → 304ms/s, "invalidates subtree" trace events 187 → 0. Enforcement: `styleInvalidation.test.ts` sweeps the global sheets (structural pseudos in featureless compounds, universal siblings); `streamdownTheme.test.ts` pins the md-blk roster both directions. Svelte scoped styles are structurally immune (generated class per compound); only the global sheets are risk surface.
- 2026-08-25 `a3c4d7c1`: OverlayScrollbar sampled and wrote `style:top` on its opacity-0 thumb on every scroll event of its owner (× every mounted bar, every glide frame). A hidden bar is inert now — `onScroll` and the ResizeObserver sample only while revealed or dragging, and the reveal transition re-samples the geometry that went stale while hidden. Tripwire: `OverlayScrollbar.test.ts` "leaves the hidden thumb untouched while the owner drives".
- 2026-08-25 `00965f76`: perfprobe `done()` no longer `process.exit(0)`s inside finally blocks — a throwing probe died silently with rc=0 (bit twice: driveburn with invented flags). It sets `exitCode` and drains the loop; errors print and exit nonzero. Probes take POSITIONAL args (`driveburn "<thread title>" "<prompt>"`), not flags.
- 2026-08-26 `a3eee4d6`: **activity-run forced-layout reads off the streaming frames** — two of the three owners of the 165Hz attribution's 2-3 forced style passes per mutated frame. `onClipScroll` read clip geometry on every scroll event including the run's own follow writes (1310 forced recalcs/242ms per 120s live streaming); reads are event-sourced now (a reader gesture licenses them, authored writes state fade/position via `positionWritten`'s written-top path over a metrics cache the free post-layout RO callbacks refresh). `observeActivityRunExpansion`'s retarget read `offsetHeight` per disclosure body against the effect-dirtied tree — the timeline's only FULL forced layouts (27/120s, ~1ms each, one per window advance); retarget is attribute-only and the geometry lives in the RO delivery it schedules. Verified on harness traced benches: `readScrollMetrics` forced reads no longer scale with stream duration (2 mount seeds across a whole 3-agent fanout), retarget frames gone, all other counts at baseline. Instrument recipe for re-verification: unminified build (`pnpm exec vite build --minify false`), playwright chromium with `browser.startTracing` (categories incl. `disabled-by-default-devtools.timeline.stack`) around `bin/ao-harness bench <workload>`, count forced UpdateLayoutTree/Layout by top JS frame — headless frame histograms are 60Hz-vsync-quantized and see nothing.
- 2026-08-26 `fe19980f`: **virtualizer row-identity reuse** — the signal-fanout audit's one concentrated finding. The projection derived minted a fresh RenderRow per mounted row on every reveal tick/geometry bump; the keyed each writes each row into a per-key signal, so every mounted row's wrapper effects re-fired and its prop chain re-validated per streamed chunk, O(window) with values identical (same identity-churn class as `#20-23`'s node caches). A `rowReuse` map keeps the previous object when all five fields match. Steady-state streaming A/B (two runs per leg): genuine flush execution 89.8→60-70ms giant-turn, 78.4→61-64ms burst (−22-30%); total JS busy 172→~145ms, 159→~147ms. Tripwire: `TimelineVirtualizer.test.ts` "row identity reuse" via the harness's `onRowRender` snippet expression. **Steady-profile recipe** (bench reloads the page and kills a Profiler session; mount noise pollutes reload windows): playwright page on the harness URL → settle 2s → `ao-harness ui open --thread #1` → settle 2.5s → CDP `Profiler.start` (100µs) → `ao-harness scenario set bench-giant-turn` (or `bench-burst-stream`, no --provider flag) then `send --thread #1 --wait` → `Profiler.stop`. Classify flush as samples with an ancestor in {flush_queued_root_effects, flush_queued_effects, process_effects, update_effect, update_derived, execute_derived, update_reaction}; `internal_set`/`mark_reactions` are write-side marking from ANY state write (~2% of busy), not flush. Unminified names need `pnpm exec vite build --minify false` PLUS `go build -o bin/agent-overflow .` + harness reboot (the binary embeds dist).
- 2026-08-26 `e9454eb4`: **idle-trim metronome gated on activity** — the 717-GCs-overnight fix (each 30-51ms, 5-8 dropped frames, reclaiming ~1MB at floor). Frontend sends input-since-last-accepted-trim; backend ORs with a turn-lifecycle stamp (`recordActivity` start/complete/disconnect, never per-delta) and answers `skipped-no-activity` when neither. Post-turn return-to-floor unchanged. Verify post-restart: launcher.log overnight should show trims only after turns or input, not every ~5min.
- 2026-08-26 `7fea82c6`: **idle-trim waits for the reveal drain** — the mid-glide blind spot occurred live, so it is built now. 1h capture during active use (framedrops 3600, 165Hz): six of the twelve worst gaps (30-48ms, plus a script-less 58.2ms LoAF whose renderStart sits at its very end) landed within 90ms after a wall-clock 5s-grid point — the trim's `IDLE_TRIM_CHECK_MS` grid phased from page load, ~270ms of RPC→launcher→CDP latency after the tick — spaced 5-6min (reattempt+backend floors), several 3-15s after a turn completed, i.e. mid-drain, and the pattern went dormant exactly while turns ran continuously. `check()` now reads `revealDrainStats()` (the harness drain probe's pane fold) and skips while any pane drains, without stamping the floor. Wire-complete ≠ reader-complete, same lesson as the harness's drain-aware bench windows. Attribution recipe that closed it: recover the page's timeOrigin by joining LoAF `t` to worst-gap `at` (two pairs agree within 10ms), then read turn spans from a `clone`d store to classify each gap idle/drain/mid-turn.
- 2026-08-26 `2a318863`: **read-free delta deliveries (cached bottom-target arithmetic)** — the resize-storm class's main lever. Full anatomy and A/B under "residual-stutter sweep" below. The scroll controller's content-delivery path stopped reading scrollHeight/scrollTop/clientHeight per delta: the cached target advances by the delivery's own delta, `viewportHeight` rides the sample (scroller RO data) as the fallback trigger, every real read site resyncs, floored short-content disables it. distanceFromBottom forced passes 22 → 0 on the 3-pane clone replay; win lands on mid-glide and escaped panes (instant-pin writes force the same clamp layout either way). Tests: index.svelte.test.ts "cached bottom geometry" (lying-getter path proofs + escaped zero-read counter + floored fallback). REGRESSION FIXED `f712f65a` (same day): the delta arithmetic double-counted whenever a real-read resync landed between deliveries (a clamp scroll event rebases the cached target to DOM that already contains the next delivery's change, then the delta applies again) — bottom-followed surfaces rested 8px short, frozen there by the 4px idle-repin deadband. The cache is height-keyed now: read-path deliveries learn `offset = scrollHeight − clientHeight − deliveredHeight`, the hot path computes `target = height + offset`, resyncs maintain only scrollTop. Zero-read property preserved (same read-counter tests); tripwire "a real-read resync between deliveries cannot double-count the next delivery" (red on the delta version at exactly the double-counted value).
- 2026-08-26 `baa816dd` + `44ef8304` + `d1b09795`: **three mount-time forced-layout readers moved to RO initial-delivery timing** (coldload campaign, P6). An RO always delivers once per observed element, post-layout pre-paint in the same frame, so a mount sample taken there is free while a synchronous mount read forces a whole-document layout against the mid-mount dirty tree (~7-10ms each on a cold heavy-thread switch). OverlayScrollbar's mount `sample()` (176ms of scrollTop across 10 cold switches), ActivityRun's mount position write (159ms of scrollHeight; the settle/restore observers own the position now, the mount states flags only), UserMessageBody's `bind:clientWidth` (svelte's dimension binding takes one sync read at bind time; 108ms — also refired per windowing remount mid-scroll). Pattern is reusable: any component needing mount geometry should read it in its RO's first delivery, never in the mount effect.

## Ruled out or declined

- **Thread-switch coldload forced-layout elimination beyond the three RO fixes: STOP-LOSS 2026-08-26.** The cold rig (12 heavy threads, 6k-65k items, 10 sequential switches in one pane) holds ~2200ms of tall-task time and ~156ms of script-forced layout across all four profile generations — REMOVING READERS DOES NOT MOVE WALL TIME, because the mount frame's pending flush is simply paid by the next reader standing (whack-a-mole: scrollbar fix handed it to ActivityRun, ActivityRun's to bind:clientWidth, bind:clientWidth's to the scroll delivery path). The remaining roster is all scroll-package: the delivery read path's `refreshIsNearBottom` (~10ms each, n≈16/10 switches), first-fire `targetScrollTop` (n≈4), OverlayScrollbar `sample()` from scroll events during mount writes (~3ms, n≈28), intent-machine attach reads, `applyEngineCompensation` scrollTop. Eliminating them means re-timing the content-geometry delivery itself (post-flush effect → RO timing), a doctrine-heavy change with a measured ceiling of ~15ms per switch on a ~200ms one-shot gesture that is script-dominated (mount + engine measure + xterm). The three RO fixes stand for their OTHER frames (windowing remounts mid-scroll, expand-all, streaming), where the fixed reader was the lone forced flush. Do not resume without a user flag on switch latency specifically.
- Fragment-addressed SVG sprite for the mask icons: MEASURED DEAD 2026-08-25 (`probe spritecheck`/`spritecheck2`, soak rig) — `url(sprite#a)` vs `url(sprite#b)` spawns per-reference isolated SVG documents for data URIs and blob URLs alike; there is no sharing to exploit. The 56-Documents finding (Blink builds an internal page+frame+document per DISTINCT SVG-as-image resource; 47 icon URIs → ~57 documents) was instead fixed by same-document `mask: url(#id)` element references, which need zero documents (`5ccf3a2e`, see Fixed causes).
- Renderer 482MB transient (2026-08-25 18:47): +103MB live Oilpan, +29MB v8, +20MB JS heap in one 2-min sample window, fully settled by the next tick (309MB, live back to 33.7). Burst allocation from a heavy one-off (large render/import), reclaimed by the normal GC cycle. samplealert caught it; nothing to fix.
- Active-use JS allocation (23.3MB/60s sampled during real use, `probe alloc`): top owners are the row-UI retention prune (22% inclusive — its scalar signature legitimately moves every pass mid-stream, so the walk re-runs) and activity-run grouping (~17% — rebuilt per `timelineRevision` bump, i.e. per structural append, by design). Incrementalizing either is a correctness-critical refactor to save ~5MB/min of promptly-collected JS garbage; the Task Manager number is driven by Oilpan committed paging, not these. Declined 2026-08-25.
- Oilpan churn class attributions for active use: `HeapVectorBacking<Member<AgentGroupSchedulerImpl>>` allocates per main-thread task — task rate during interaction is 165Hz frame production, not app-schedulable work; `DisplayLockUtilities::ScopedForcedUpdate` allocates per JS geometry-read call (the spring's reads, already minimal, 0 forced layouts from JS at idle); `ScrollableArea` vector nodes follow recalc volume. No app lever behind any of them. OVERTURNED 2026-08-25 (same day, later): composited scrolling is the lever — the whole family vanishes from the churn census under `will-change: scroll-position`; FIXED `3e249b5a` — see Fixed causes, composited pane scrollers.
- Ambient animation ticks as a MEMORY lever: ruled out 2026-08-25. 25.8k of 27.1k StyleRecalcInvalidationTracking events were main-thread "Animation" ticks on the working-LED spans (~14k/15s) and pulse dots (~11.8k/15s) despite compositeFailed:0, but the A/B/A with `animation: none !important` sat ON the burn-progression trend line (17.3 → 25.1 → 29.6 MB/min) — animations dominate invalidation COUNT, not churn MB. CPU-only decision-sheet item at most; the will-change doctrine forbids the compositor-promotion fix anyway.
- Mask-sprite `<g transform="scale(1/24)">` groups as the `PlaneRootTransform` churn owner: ruled out 2026-08-25 (A/B/A on the soak, `transform: none !important` on the sprite groups — B's rate sat above both A arms). The hidden sprite's transform nodes are never geometry-mapped, so they never regenerate. The 16MB/min PlaneRootTransform churn during two-pane streaming burn is GeometryMapper cache regeneration across the property tree under continuous bottom-follow scrolling (~1,000 allocs/s ≈ property nodes × scroll updates/s, invalidated by the global cache-generation bump each scroll write) — Blink scroll bookkeeping proportional to visible streaming, the app's transform-node feed already minimal (70 transform elements: 39 hidden sprite groups, 18 chevrons, 13 misc). Remaining lever would be fewer paint property nodes inside pane subtrees; ceiling too low to chase. OVERTURNED 2026-08-25 (same day, later): the lever is not fewer nodes, it is stopping the per-scroll generation bump — a composited scrolling layer takes the direct scroll-offset path and the PlaneRootTransform churn goes to zero; FIXED `3e249b5a` — see Fixed causes, composited pane scrollers.

- GPU-process swings (observed 206→305MB and back within 15-second windows, 2026-08-25): attributed by dumping AT a caught peak (poll `PrivateMemorySize64` of the AO gpu-process — filter `msedgewebview2` command lines for BOTH `--type=gpu-process` AND `agent-overflow`, five WebView2 apps run on this box — and fire `memdump --gpu` on a threshold). Of an 83MB peak-over-floor, only 17MB is visible to memory-infra (malloc +16, tiles +1, shared images and skia flat); the rest is untracked D3D11 usermode-driver staging during raster/upload bursts. Transient, returns to floor in seconds, scales with repaint volume. Not a leak; only lever is rastering less (ruled out). Floor remains 185-230MB.

- Visible-pane-streaming trim gate (fire while only background threads stream, closing the continuous-fleet gap where zero-turn windows are rare): declined by ruling 2026-08-25 — most active panes are on screen anyway, and the any-turn gate stays. Do not re-propose.
- The spring's per-tick `targetScrollTop` forced flush (the third owner in the 165Hz attribution, 1384 recalcs/274ms per 120s): ruled out 2026-08-26, LOAD-BEARING, do not "optimize". The read is one forced pass per mutated frame and it is what makes (a) the write clamp against the CURRENT max-scroll, (b) the clamp-evidence witness see a clamp the same frame's layout applied (spring.ts ~998), (c) same-frame follow stay fresh, and (d) the post-write readback classify MOVED/REFUSED/INCONCLUSIVE for the write-refusal wedge guard. Clean-frame reads are free (no trace event), so a sentinel-skip saves nothing; the lever on this class is the DIRT'S SCOPE (see `a56ca00a`'s lesson), which `9eaa5465` already cut.
- The activity-run head-advance gBCR pair (`activityRunRowViewportTop` pre-read + `activityRunScrollTopHoldingRow` post-read, 26 forced/6ms per 120s): ruled out 2026-08-26 — the pre-read must price the OLD DOM and the post-read the NEW one, so the pair cannot share a pass or a cache (row heights vary). One forced pass per window advance, ~0.05ms/s, inherent to the compensation design.
- Incremental caching for `indexCompletions` / `filterRedundantNotifications` (the remaining uncached O(window) projection passes): ruled out 2026-08-26 by CPU profile over the harness benches. A 225-item giant-turn compressed into a 2s drive (worst-case delta density) shows indexCompletions 0.8ms inclusive, filterRedundantNotifications 0.6ms, ALL of groupActivityRuns 2.8ms — the `#20-23` node-reuse cache wave already reduced these to noise, and `parseJsonObject`'s string memo makes the per-item extraction a Map hit. Invalidation complexity for an unmeasurable win.
- DEBUG-build diagnostics on the user's daily build: `VITE_AGENT_OVERFLOW_UI_TRACE` + `UI_ORACLES` are on under `make dev-wsl DEBUG=1`; `snapshotChatDomForTrace` ran inside the live capture (~19ms/60s, 45 forced recalcs/120s). Not a code fix — launching with `UI_ORACLES=0` sheds the heavy tier; decision is the user's (surfaced 2026-08-26).
- Renderer memory-pressure simulation (`Memory.simulatePressureNotification`): A/B'd on the soak rig under streaming load (2026-08-25, `activetrim` probe) — `moderate` returned nothing (+5.3MB, churn noise), `critical` returned nothing (-0.5MB, blink_gc unchanged). WebView2's renderer does not run a memory-reducing GC on either level. `Memory.forciblyPurgeJavaScriptMemory` is the near-OOM intervention simulator (can kill page scripts) — disqualified on semantics, `HeapProfiler.collectGarbage` returns the same pages safely (-25.5MB private on a 127MB renderer, one 58ms stall, zero LoAF).
- WebView2 `MemoryUsageTargetLevel=Low`: declined. Its spec describes working-set trimming through disk swapping, not reclamation ("if script runs after we swapped related memory out, we will swap the memory in to ensure script can still run"), so it shrinks the Task Manager number and adds swap-in stalls. Blur-gating it also leaves active-state memory untouched.
- Mount fewer panes / scope the ticker: rejected by ruling.
- Overlap-layer restructuring (14 layers promoted by Overlap): ceiling ~5-10MB GPU and ~2% main thread; the fixes change scroll or paint behavior. Not worth an A/B unless the ceiling changes.
- Glide residue `rotate: 0.0001deg` in `chokepoint.ts`: gone with `7b29f9d6`, along with the rest of the content-transform path. Do not reintroduce it; the architecture test rejects it.
- `--js-flags=--heap-growing-percent=N` to cap V8's heap-limit growth factor: withdrawn before it was built, twice. It was first proposed against the 28MB/min churn rate that `7b29f9d6` removed. Re-proposed against the committed-page growth, it fails on the measurement above: major GCs already run every 10s, sweeping is what they do, and the growth is pages that sweeping never returns. More frequent GCs buy pauses and nothing else.
- Go backend RESIDENT memory: not a contributor — live heap holds flat ~15MB, goroutines flat ~57, RSS ~93MB. Backend ALLOCATION churn was a contributor (see the two 2026-08-25 fixed causes); GB-scale transient bursts balloon `heapSys` (observed pinned at ~248MB — committed pages the scavenger returns slowly), which is churn through the GC, not a leak. The remaining churn deliberately left: provider-stream `RawMessage` decode copies + `readBoundedLine` (structural to the streaming path, GC handles it).
- go-tree-sitter v0.25.0 (latest) binding overhead: `readUTF8` copies the input twice per parse — one Go-heap `string(payload.text)` (visible to pprof) plus one `C.CString` (C heap, invisible). Fixed 2× multiplier on all parse traffic; a local patch was declined, possible upstream contribution.
- Backend overnight idle allocators (4h diff, 2026-08-25, all attributed and left alone): `io.copyBuffer` ~334MB = os/exec pipe copies from the gitwatch 60s liveness probe (deliberate silent-death net, fs-watch is primary and 3s polling only engages on install failure) plus the background `git fetch` cadence (1-min tick, real fetch rate-limited per-repo/5-min); `io.ReadAll` ~218MB = dev-only WS render-trace batches (~170MB, absent in production) + `triage.readClaudeTaskOutputFile` (~43MB) + git untracked-count reads; pprof self-observation ~370MB cum from the samplers themselves. Idle backend CPU 0.38% of one core; heapSys steady 23-34MB.
- `triage.readClaudeTaskOutputFile`: event-driven, not a poll — one bounded (8MB) read per `system/task_notification` carrying an `output_file`, upserting one payload row. Repeated reads come from watch tasks / background monitors firing real notification events on growing files; ~10MB/h churn. An incremental-offset scheme would add stateful complexity to save noise. Accepted.
- Backend CPU during active turns: 1.43% of one core (30s profile mid-activity, 2026-08-25), ~70% inside SQLite VDBE on the persist path. Not a lever.
- Renderer active-turn churn post-fix-waves: 2.19 MB/min total above the 50KB class threshold (45s churn window during live agent turns, 2026-08-25); top class ~2MB/min, everything else 0.1-0.35. Active-turn private bounces 253-338MB with clean GC returns (was 550-600MB at the original symptom). No remaining churn target worth a fix.

## Open

- 2026-08-26 **165Hz frame-drop campaign — VERIFIED LIVE (post-restart 1h framedrops capture, build 91879d7f, 17:17-18:17).** 593,708 frames, median 6.1 / p99 6.2 identical to baseline. Verification recipe that closed it: `t − at ≈ 77.6s` (LoAF page-time vs capture-time offset, anchored by the one event present in both lists) joins gaps↔LoAFs, and launcher.log `GC done` lines date every trim. The three attributed sources and their outcomes:
  1. **Idle-trim GC metronome**: VERIFIED FIXED. launcher.log flips from a trim every ~5min around the clock (old build, 14:56-15:59) to one boot trim then a 40-min idle stretch with ZERO trims (activity gate `e9454eb4` holding), then one per ~5min only around active use. The 5 in-window trims each match a worst gap (36-56ms) — that stall is the trim's designed price, now landing only in gate-approved quiet windows (10s input-idle, no turn, no drain per `7fea82c6`); compositor-driven animation rides through it, so a static-read window makes it invisible. If stutters are STILL felt near trim times, the remaining lever is trim cadence/positioning (decision sheet).
  2. **Reveal-tick rAF frames at 12-15ms** (svelte flush 6-8.5ms + Paint 2-3ms, ~13/min during streaming): the fanout audit ran (user-approved) and its one concentrated find is FIXED `fe19980f` (virtualizer row-identity reuse, flush-exec −22-30% — see Fixed causes). What remains in a steady-state streaming profile is ~27ms/turn svelte-internal plumbing plus a legitimate long tail (the streaming row's markdown, geometry freshness via deliverSample→refreshIsNearBottom which is load-bearing engine design, run registry, keyedSignalRegistry) — no further concentrated fanout to cut. Post-restart: streaming reveal frames should sit measurably under the 12-15ms captures.
  3. **Scroll-metric read-after-write thrash** (~22 forced passes/s streaming): the activity-run half FIXED `a3eee4d6`; the spring's `targetScrollTop` half ruled out as load-bearing (see Ruled out). Post-restart re-probe: 120s trace during streaming should show forced recalcs ≈ targetScrollTop only (~11/s), readScrollMetrics near zero.
  - Clean baseline stands: outside trims and streaming, 150s rAF = 24,749 frames, median 6.1ms, p99 6.2, worst 11.6ms, zero LoAF ≥50ms. Probes: `framedrops`, `mainstalls`. MS Teams owns the machine's other big WebView2 processes.
  - Post-fix capture residuals: the 75ms streaming `handleResizeEntries` LoAF DID NOT RECUR (the only RO-heavy LoAF was a real pane drag-drop, 53.7ms total = ondrop 27.9 + RO 20.5 — modestly under the pre-fix 55-97ms, consistent with the RO half shrinking; drop handlers still the open item). The 114.8ms script-less LoAF at 18:04:37 is ATTRIBUTED to the uptime-scaling GC-pause class below.

- 2026-08-26 **residual-stutter sweep, remaining open items** (1h live capture + clone correlation):
  - **Turn-completion burst REFUTED as the felt stutter**: every worst gap in the 1h capture sits 2.3s-7min from the nearest turn completion (27 completions read from the cloned store). The 18-21ms completion tick measured on the harness is real but did not produce the user's worst gaps; the sidebar-resort split stays unbuilt. Do not re-derive.
  - **Virtualizer resize storm — ATTRIBUTED, main lever FIXED `2a318863`.** Unminified 3-pane clone replay (storm2/storm3 artifacts) split the class fully: `handleResizeEntries` and `engine.applyMeasurements` are INNOCENT (0.1ms even under forced width storms — the 25-29ms viewport-resize tasks are pure native rewrap, UpdateLayoutTree 19.6 + Layout 7.7, zero JS). The real anatomy of a bad frame: all panes' WS item commits flush in ONE rAF (`flushItemEventQueue` is rAF-scheduled) and the single svelte flush renders every pane's beat (~5ms/pane of genuine thousand-cuts render work, no concentrated app site left post-`fe19980f`), PLUS the scroll controller's per-delivery forced-layout reads — `distanceFromBottom`/`targetScrollTop` on the content-delivery path, 4 forced passes in the worst 27.8ms frame because other panes' writes re-dirty layout between deliveries, and the first reader pays the whole pending recalc. The live 75.8ms LoAF is this same flush model entered through the RO door (RO callbacks run pre-paint in the rendering task; the applyUpdate state writes flush inside it). Fix `2a318863`: delta deliveries decide from cached bottom-target arithmetic (distanceFromBottom ≡ target − scrollTop; the sample carries `viewportHeight` from the scroller RO so a viewport move falls back to real reads; every real read site resyncs the cache; floored short-content disables it). A/B on the same replay: distanceFromBottom forced passes 22 → 0, busy p95 4.25 → 3.25ms. Instant-pin deliveries stay flat by design (their scrollTop write forces the same clamp layout); the win is mid-glide (spring-chasing, no per-delivery write) and escaped panes. `spring.sentinelTarget()` also stopped reading clientHeight per resolver snapshot when no sentinel is armed (same commit).
  - **Pane drag-drop frame 55-79ms** (`PaneHost` `onHostDrop`/`onPaneDrop` ~26-32ms + the RO storm ~20-30ms in the same frame): one-shot gesture cost, observed three times live. The RO-storm half shrinks with `2a318863`; the drop handlers themselves remain unprofiled at fix depth.
  - **3-pane beat collision round-robin: BUILT AND REFUTED (2026-08-26), do not re-propose.** Rotating the item-event flush across mounted threads (one pane's commit per rAF, FIFO by oldest event, timeout backstop full-draining) was implemented and gated on a controlled clone-replay A/B/A/B — identical `from-thread` rule both legs, before-binary built from clean HEAD in a temp worktree, two runs per leg. Merged (HEAD): busy p95 3.0/3.0ms, max 26.2/27.3, 6ms-fit 97.4/98.0%. Rotated: p95 5.0/5.5ms, max 24.4/30.3, 6ms-fit 95.5/96.8%. Every aggregate worse. Mechanism: the tall frames are flood-shaped — one pane's own beat (streaming-row markdown + regroup) is 15-25ms by itself, so deferring the other panes cannot shrink the tall frame, and un-merging multiplies the per-flush fixed costs (svelte flush entry, regroup, notify pass) the single batch amortizes. The earlier "~5ms/pane thousand-cuts" shape is the steady state; the worst frames were never a sum of small beats. Tripwire: events.test.ts "applies every mounted pane's beat in the same frame" + the NOTE in eventsItemStream.ts#flushItemEventQueue. Consequence: the multi-hitch-during-output residual's levers are the per-beat cost itself (streaming-row markdown, regroup) and minor-GC churn — not commit scheduling.
  - **ComposerToolbar density measurer: FIXED `abda8210`.** Was: a subtree MutationObserver (childList+characterData, i.e. the token/limit text) scheduling a rAF measure per streaming beat, each forcing a full pass against the flush-dirty tree (19-21/storm run, plus the `data-compact` probe toggle's write→read→write). Now: RO entries on the toolbar + its direct children (width deliveries only, post-layout where the reads are free and the probe toggle relayouts a clean subtree); childList-only observer for control mount/unmount. Tripwire: ComposerToolbar.test.ts "remeasures on a child width delivery". Verified on a fixed-build 3-pane storm trace: zero measurer passes; the remaining forced-pass roster is fully attributed (spring-family targetScrollTop reads — ruled out as load-bearing, dense under storm replay at ~15ms/s of sub-ms passes; virtualizer RO first-read bill; terminal-fit RO gBCR, 0.1ms each).
  - **`handleResizeEntries` first-read bill (tiny, known):** the RO handler's `offsetParent` checks now pay the first forced pass in some frames (1.4ms×12 per storm run) since the delivery reads stopped cleaning layout ahead of it. Same dirt bill, different first reader — not worth chasing below the sidebar-FLIP `measure` cost (5-6ms/run) it sits beside.
  - Two mid-turn 36ms gaps sat on the trim's 5s grid but with a turn open (backend refuses trims there) — folded into the uptime-scaling GC-pause class below; the git-fetch/liveness contention theory found NO fetch bursts in a 6.7min spawn watch (2 status spawns only, 1 gitwatch watcher live).
- 2026-08-26 **sidebar FLIP forced layout in boundary frames — FIXED (a84ac3de); "per-beat machine-gun" cadence attribution CORRECTED.** Windowed CPU samples (tall RunTask ranges of the clone-rig storm captures) put ALL of the frames' `getBoundingClientRect` self-time (9-27ms per capture) under svelte's `animate:flip` measure()/apply() — the sidebar's two animated each-blocks, forced-layout reads over every visible row with layout already dirty. Trigger chain: activity bumps (`syncThreadActivity`) rewrote the threads AND projects arrays per bump → every project's tree re-derived → fresh each arrays → reconcile + FLIP even when nothing reordered (~75% of reconciles were no-ops). CADENCE CORRECTION: these bumps fire at BOUNDARIES only (reader-authored user_text, approval/user-input request, turn complete) — the item-stream per-beat path was already descaled (userTextCountsAsActivity gate; the earlier liveUsageSnapshot side-cache did the same for provider:usage). The "machine-gun during agents' final output" symptom is repeated boundary bursts (approvals + completions in quick succession), not per-token beats; rig steady-state beats fit the 6ms budget at 97-98% even at HEAD. Fix (zero visible change): per-entity live-activity keyed boxes (arrays silent for bumps), identity cutoffs at both each arrays (`sameSidebarVisibleNodes`/`sameThreadStatusPill` — FLIP measures only on real reorders), fine-grained time labels + shared minuteClock, and `syncThreadActivity` no longer replaces pane.thread. A/B/A/B (HEAD vs fix, same from-thread rule): FLIP-called gBCR samples 465→115 and 117→65; busy aggregates flat in the rig because its replay crosses only three boundaries. Residual in burst frames (sb-B legs): ~15ms inclusive of REAL-reorder FLIP (three threads jumping to the running tier — legitimate animation) + ~20ms diffuse svelte re-render of genuinely-changed rows + coldload/reveal work. Tripwires: ProjectThreadList "a live-activity beat does not reconcile the FLIP each-block", threads/projects store array-identity tests.
- 2026-08-26 **felt-stutter window ATTRIBUTED (user's return-from-away, 18:01-18:09) + uptime-scaling GC pauses named**. ui-render.jsonl mining decomposed all six unattributed gaps: 17:33:20 and 18:06:51 (30.4ms each) are thread-switch coldloads (`timeline.coldload` + `scroll.attach` + `contentRO.firstFire` at the epoch; the 18:06:51 one: 157 items, fetch 51ms, settle 130ms); 18:02:50 (48.4+36.3 pair) is opening a codex TERMINAL-mode pane — xterm mount plus all six panes re-pinning bottom (`virtualizer.scrollTarget` writes against six distinct scrollHeights in one 10ms burst); 18:04:37 (115.2), 18:06:19 (36.4, renderer totally silent ±3s), and the mid-glide 18:08:32 (30.4, chase maxGap 30.4 with ZERO longtasks) are the **uptime-scaling GC-pause class**. Evidence for that class: launcher.log trim GC durations reset to ~30ms on EVERY restart and climb with renderer uptime — session of 13:48: 32ms fresh → 100/82/74ms at ~3h (16:43-16:58); session of 17:15: 33 → 45/57/56 within the hour; session of 18:25: back to 32/30. Script-less LoAFs (zero scripts, zero blocking, renderStart delayed the whole stall) appear only past ~45min uptime and grow the same way (115 at 49min, 105 at 61min, 60/77 at 2.6/3.2h in the rotated file). Mechanism: V8/Oilpan pause time scales with heap size, heap grows with uptime (the mem-bloat campaign's open active-use leg). User independently reported "a lot less bad" right after restart. Instrument added: `frame.loaf` records now carry `heapBeforeMb`/`heapNowMb` (2s interval sampler ring in loafTrace.ts, `performance.memory`) — a heap DROP across a script-less stall is the GC verdict, flat heap exonerates the renderer. The FIX is the mem-bloat campaign's active-use heap decrease, not a scheduling change. Thread-switch coldload and xterm-mount costs are one-shot gesture prices; profile at fix depth only if the user flags them specifically.
  - `probe frames` cannot span long windows: the CDP trace reader string-concats and a 400s active-renderer trace overflows Node's max string length (`readStream` in `lib/cdp.mjs`). Keep frame traces ≤~120s or fix the reader to stream to disk + incremental parse.
- Scroll-intent asymmetry, probe-relevant: programmatic `scrollTop` writes are not reader intent in EITHER direction — a console write away from the bottom gets glided straight back (follow never disengaged), and a write to the bottom does not re-engage. Only real input (wheel) moves the engage state; `probe scrollgesture <secs> <idx> <sel> down` is the re-engage tool.
- Soak-rig artifact (2026-08-25): a single frozen 1876.8MB `partition_alloc/partitions/buffer/buckets/directMap_1` allocation in the soak renderer — stable across dumps, absent from the user's app (its partition_alloc: 14.7MB) — a DevTools/probe serialization leftover. While it stands, soak private-footprint ABSOLUTES are meaningless; only deltas count. Restart the soak to clear it.
- Upstream issue/PR for the reconnect double registration (svelte `main` matched 5.56.8 on 2026-08-23).
- Chromium perf bug for the `PlaneRootTransform` re-allocation (one-line reuse in `GeometryMapperTransformCache::Update`): not filed, and no longer reached by this app after `7b29f9d6`.
- Side observation, unfiled: synthetic wheel events on the first composited plane scrolled the whole app view, so the app root may be scrollable.
- `HeapVectorBacking<blink::PaintChunk>` is 0.55 of the residual 1.07 MB/min churn. It is rebuilt once per paint, so it falls with the frame rate rather than needing its own fix.
- Idle-trim reach gap: native desktop (macOS/Linux) and `--connect` sessions emit `webview:trim` into silence — no launcher-equivalent consumer exists there yet.
- Post-restart full sweep 2026-08-25 (pulse fix + trim live): renderer 213MB idle / 289 streaming with clean GC returns; trim verified end-to-end in launcher.log ("renderer GC done in 35-169ms"); CPU 17.9% active; churn 19.7MB/min streaming, top class 1.4MB/min; backend live heap 8MB. `probe frames 20` at idle then found the pulse regression above: main-thread busy 41.4ms/s, of which ~31 was the 8Hz ticker's write → recalc → paint → layerize → commit chain, and ZERO layouts forced from JS. Reverting the pulse to CSS should take idle main-thread busy to roughly 10ms/s — re-run `probe frames 20` after the restart to confirm. Remaining non-targets: scroll pipeline geometry reads ~13ms/s during glides (volume, not forced layout — structural to the spring, low ceiling); ChildListMutationAccumulator 0.5MB/min (streamdown mutations intersecting scoped observers — math/mermaid/toolbar; no global observer exists); PlaneRootTransform residual 1.4MB/min (Chromium reuse gap, app feeds fixed).
- Nav-rail `data-current`: `probe mutations` shows the write rate but not whether a write CHANGES anything, and the difference is the whole verdict. `probe attrflap data-current 15` split it: 142 writes across 111 tick elements, 2 value changes, 1 reversal. The marker was not oscillating (my first read of the counts, wrong); the rail was scrubbing every tick on every structural pass — O(thread length) redundant attribute writes for one moved marker. Fixed: the claim is element-keyed like the module's five other applied caches, so the scrub (and `reset()`) is gone. Rule for any imperative DOM writer: a per-member sweep beside a diff-only writer is worse than redundant, it strands the DOM (the writer's cache says "already applied" and skips the relight) — the tripwire pair is in `messageNavRailSync.test.ts`.
- Detached-DOM census: stable across an idle night (752→789 nodes, the drift is svelte cloneable fragment/TEMPLATE by design; the 252-node detached SECTION held exactly 252). Not a leak.
