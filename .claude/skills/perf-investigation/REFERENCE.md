# Perf investigation reference

Measured on the 2026-08-22/23 investigation (Windows 11, WebView2, window 2560x1369, dev build). Numbers are floors to subtract, not targets. Append to the ledgers at the bottom when an investigation moves them.

## Floors

Renderer ("Agent Overflow (dev)") at first start, two panes visible: 112-128MB private. cc/tile_memory 69MB, v8 29MB, blink_gc 23MB, malloc 36MB, partition_alloc 11MB, gpu/transfer_buffer 16MB.

- Raster tiles are visible content only. Until 2026-08-23 `7b29f9d6` every visible timeline plane was its own composited layer: ~7MB (3 tiles of 896x704) per plane, 2.4MB per visible activity run, 13.8MB for the root layer (4 tiles of 2560x352), 0.2-0.9MB per Overlap-promoted fade or divider, 54-64MB active in total. That commit deleted the `will-change` promotion, so timeline content paints into the root layer: the promoted-element census fell from ~40MB of estimated texture over five planes to 0.6MB, renderer `cc/tile_memory` from 60-86MB to 27.5MB, and the GPU process from 297-336MB to 198-217MB (the last pair is flattered by six panes on a fresh start against 13-17 panes on an hour-old one). Panes scrolled out of the strip hold no tiles (cc's policy is ALLOW_PREPAINT_ONLY with a 512MB soft limit, and it only keeps NOW and SOON bins). memory-infra's `cc/tile_memory` also counts the pool of recently freed tiles that streaming re-raster double-buffers; the GPU process mirrors the same bytes as `gpu/shared_images`.
- blink_gc (Oilpan) committed is page fragmentation, not live data and not a leak. Measured 2026-08-24 on the post-`7b29f9d6` build: `probe memdump` reported 117.8MB committed against 24.4MB of objects, and the detailed dump's per-page rows put 920 pages at 21% fill, with 303 of them (39.4MB) completely empty and only 41 over three-quarters full. Sweeping empties objects out of pages, the pages stay committed, and fresh allocation takes new pages instead of refilling sparse ones. Two class censuses 28 minutes apart settle it: committed went 90.5 to 109.9MB while objects went 19.9 to 15.4MB over the same 1208 classes, every top class flat or shrinking (`PlaneRootTransform` 6397 to 461 instances). Objects down, committed up, so nothing is retained. It is a sawtooth, not a ratchet: committed ran 28.8 to 167.6 to 69.2 to 104MB across one hour, because Chromium purges on page-hidden and on system memory pressure. Only a memory-reducing GC returns the pages, and the peak is set by the churn rate.
- Ordinary major GCs run about every 10 seconds in this app (`probe frames`: 2 MajorGC in 20s, 5.5ms total, 2.8ms worst, plus 189 incremental marking steps at 0.7ms max). They sweep; they do not return pages. Any proposal that works by making major GCs more frequent is answering the wrong question.
- Detached DOM nodes hover around 3.2k in a dev build and stay flat. A step up that persists after a pane closes is the leak signal.

GPU process: about 185-230MB at first start and it does not fall below that. Roughly 100-120MB is fixed (D3D11/ANGLE driver heap, DirectComposition swap chain at 2560x1369 ≈ 14MB per buffer, skia GPU cache ≈ 16MB, shared images ≈ 13MB, transfer buffers), 40-50MB is heap slack, and cc/resource_memory (tiles, 40MB at start) scales with composited planes. It is not attributable to app code below that line.

Browser process ("Manager"): 40-55MB, mostly IndexedDB/leveldb and malloc. Network, Storage, Crashpad: under 10MB each.

Go backend: 13MB live heap on 6363. Lean; one confirmation profile per investigation is enough.

## Interpretation

- Task Manager's column is private working set; memory-infra `private_kb` matches it within a few MB.
- JS is under 2% of wall time in steady state. Per-frame cost is native: the reveal smoother, the 8Hz ambient ticker, the ~20fps sprite, the spring, and the composited layer set (25-27 layers, 14-15 promoted by Overlap).
- `PlaneRootTransform` was the top Oilpan churn class until 2026-08-23 `7b29f9d6` (21.6 of 28 MB/min, 1,300 allocations/s). Chromium `main` (`geometry_mapper_transform_cache.cc` `Update`) calls `MakeGarbageCollected<PlaneRootTransform>` on every cache regeneration, both for a flat non-2D-translation node and for every 2D-translation node under one, with no reuse. Any transform or clip node change anywhere bumps the global generation, and the next paint, hit test or IntersectionObserver query re-allocates for every node it walks. The app fed that loop through `.scroll-composited-content { will-change: transform, translate, rotate }`, which made every timeline plane a flat non-2D-translation node over a subtree of 2D-translation descendants, with the spring writing a transform every glide frame. Removing the rule removed the feed — at idle. An earlier claim that the class was "absent from churn now" was an idle-only measurement: WHILE SCROLLING it was still 72% of Oilpan churn on 2026-08-24 (26.44MB/min, `scrolldrift` probe), because every `<svg>` root whose rendered size differs from its viewBox qualifies as such a node (`NeedsReplacedContentTransform` gives it a scale) and the app mounted ~400 scaled lucide roots; scroll-driven paint-property updates bump the generation. Fixed 2026-08-24 by converting lucide to CSS-mask spans and matching MeterRing's viewBox to its rendered box (see Fixed causes).
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
- 2026-08-24 `7c70256d`: MeterRing viewBox matches its rendered 28px box, so the header rings are identity-scale svgs and their dashoffset ticks stop regenerating a scaled node's transform cache (default zoom only).

## Ruled out or declined

- WebView2 `MemoryUsageTargetLevel=Low`: declined. Its spec describes working-set trimming through disk swapping, not reclamation ("if script runs after we swapped related memory out, we will swap the memory in to ensure script can still run"), so it shrinks the Task Manager number and adds swap-in stalls. Blur-gating it also leaves active-state memory untouched.
- Mount fewer panes / scope the ticker: rejected by ruling.
- Overlap-layer restructuring (14 layers promoted by Overlap): ceiling ~5-10MB GPU and ~2% main thread; the fixes change scroll or paint behavior. Not worth an A/B unless the ceiling changes.
- Glide residue `rotate: 0.0001deg` in `chokepoint.ts`: gone with `7b29f9d6`, along with the rest of the content-transform path. Do not reintroduce it; the architecture test rejects it.
- `--js-flags=--heap-growing-percent=N` to cap V8's heap-limit growth factor: withdrawn before it was built, twice. It was first proposed against the 28MB/min churn rate that `7b29f9d6` removed. Re-proposed against the committed-page growth, it fails on the measurement above: major GCs already run every 10s, sweeping is what they do, and the growth is pages that sweeping never returns. More frequent GCs buy pauses and nothing else.
- Go backend: not a contributor.

## Open

- Upstream issue/PR for the reconnect double registration (svelte `main` matched 5.56.8 on 2026-08-23).
- Chromium perf bug for the `PlaneRootTransform` re-allocation (one-line reuse in `GeometryMapperTransformCache::Update`): not filed, and no longer reached by this app after `7b29f9d6`.
- Side observation, unfiled: synthetic wheel events on the first composited plane scrolled the whole app view, so the app root may be scrollable.
- `HeapVectorBacking<blink::PaintChunk>` is 0.55 of the residual 1.07 MB/min churn. It is rebuilt once per paint, so it falls with the frame rate rather than needing its own fix.
