# Perf investigation reference

Measured on the 2026-08-22/23 investigation (Windows 11, WebView2, window 2560x1369, dev build). Numbers are floors to subtract, not targets. Append to the ledgers at the bottom when an investigation moves them.

## Floors

Renderer ("Agent Overflow (dev)") at first start, two panes visible: 112-128MB private. cc/tile_memory 69MB, v8 29MB, blink_gc 23MB, malloc 36MB, partition_alloc 11MB, gpu/transfer_buffer 16MB.

- Raster tiles are visible content only. `probe tiles` on 2026-08-23: ~7MB (3 tiles of 896x704) per visible timeline plane, 2.4MB per visible activity run, 13.8MB for the root layer (the non-composited UI, 4 tiles of 2560x352), 0.2-0.9MB per Overlap-promoted fade or divider; 54-64MB active in total. Panes scrolled out of the strip hold no tiles (cc's policy is ALLOW_PREPAINT_ONLY with a 512MB soft limit, and it only keeps NOW and SOON bins). memory-infra's `cc/tile_memory` (~100MB) also counts the pool of recently freed tiles that streaming re-raster double-buffers; the GPU process mirrors the same bytes as `gpu/shared_images`.
- blink_gc (Oilpan) is a GC cycle, not a plateau. Measured 2026-08-23 with four panes streaming: garbage accumulates at 28-70MB/min, V8 waits until the global heap reaches roughly 4x live before a major GC, and the freed pages stay committed. Renderer private footprint swung 479MB (326MB committed, 31MB allocated) → 267MB after a forced memory-reducing GC, and 401 → 245MB in another run. A minimized window gets that GC from Chromium; an active one does not, because the app's steady allocation rate (spring, sprite, ticker, streaming) stays above V8's MemoryReducer idle threshold. Task Manager's renderer number is wherever the cycle is when you look.
- Detached DOM nodes hover around 3.2k in a dev build and stay flat. A step up that persists after a pane closes is the leak signal.

GPU process: about 185-230MB at first start and it does not fall below that. Roughly 100-120MB is fixed (D3D11/ANGLE driver heap, DirectComposition swap chain at 2560x1369 ≈ 14MB per buffer, skia GPU cache ≈ 16MB, shared images ≈ 13MB, transfer buffers), 40-50MB is heap slack, and cc/resource_memory (tiles, 40MB at start) scales with composited planes. It is not attributable to app code below that line.

Browser process ("Manager"): 40-55MB, mostly IndexedDB/leveldb and malloc. Network, Storage, Crashpad: under 10MB each.

Go backend: 13MB live heap on 6363. Lean; one confirmation profile per investigation is enough.

## Interpretation

- Task Manager's column is private working set; memory-infra `private_kb` matches it within a few MB.
- JS is under 2% of wall time in steady state. Per-frame cost is native: the reveal smoother, the 8Hz ambient ticker, the ~20fps sprite, the spring, and the composited layer set (25-27 layers, 14-15 promoted by Overlap).
- `PlaneRootTransform` is the top Oilpan churn class in steady state (21.6 of 28 MB/min measured 2026-08-23, 1,300 allocations/s). Chromium `main` (`geometry_mapper_transform_cache.cc` `Update`) calls `MakeGarbageCollected<PlaneRootTransform>` on every cache regeneration, both for a flat non-2D-translation node and for every 2D-translation node under one, with no reuse. Any transform or clip node change anywhere bumps the global generation (the spring's per-frame translate during a glide does), and the next paint, hit test or IntersectionObserver query re-allocates for every node it walks. Every `<svg>` root is such a node whenever its rendered size differs from its viewBox (`NeedsReplacedContentTransform` → scale), so icons allocate on every regeneration regardless of the glide residue; the residue adds the plane itself and its 2D-translation descendants. The fix is upstream (reuse the object); app-side levers only trade the glide or the icons.
- In a heap snapshot, detachedness is a node field (`detachedness === 2`). The `Detached ` name prefix is absent in current snapshots; a census keyed on it reports everything attached.
- A retaining path `system / Context → <moduleVar$1> → reactions → derived` means a module-scope svelte signal still holds a reaction from a dead component. Read the derived's `parent` chain: an effect with `fn === null` was destroyed, so the derived outlived its owner and is held only through the signal.

## Red/green recipe for a svelte patch hunk

Prove the bug on the previous patch, the fix on the new one, without touching node_modules by hand:

1. Keep copies of the new patch and lock outside the tree (`cp frontend/patches/svelte@*.patch frontend/pnpm-lock.yaml /tmp/...`).
2. `git show HEAD:frontend/patches/svelte@<v>.patch > frontend/patches/svelte@<v>.patch`, same for `pnpm-lock.yaml`, then `cd frontend && pnpm install --offline`. Run the suite: it must fail.
3. Copy the saved files back, `pnpm install --offline`, run again: green.

Edit a hunk with `pnpm patch svelte@<v> --edit-dir <dir>` (applies the existing patch first) and `pnpm patch-commit <dir>`; only the patch hash changes in the lock. `svelte/internal/client` exports `get/set/state` in its types but `derived/effect/effect_root` only at runtime (import the namespace and cast).

## Fixed causes (do not re-derive)

- 2026-08-23 `761452b6`: MessageNavRail ticks rested at `translateY(-50%) scaleX(0.38)`, 540 non-2D transform nodes regenerating per scroll frame (~85KB/frame, ~300MB/min while scrolling). Rest state has no transform now.
- 2026-08-23 `6cbfb341`: `pane.items` is `$state.raw` with per-row boxes; the deep proxy re-minted per-index sources every batch.
- 2026-08-23 `cf33baf2`: dev-only detached-DOM leak from probe Maps, now WeakMaps.
- 2026-08-23 `b0f34fe7`: composer textarea autosizes with `field-sizing: content`; the JS measurement forced two layouts per keystroke.
- 2026-08-23 `3e5984ce`: upstream svelte bug, a reconnecting dirty derived was registered twice on deps new to that run and kept a closed pane's DOM alive through the global `accounts` signal (patch hunk 5, reconnect-dedupe).
- 2026-08-23 `0e6eefc4`: `CommandOutput.svelte` re-splits the command only when its text changes (7MB/min of JS garbage while streaming before).

## Ruled out or declined

- Blur-gated WebView2 `MemoryUsageTargetLevel=Low`: declined, it does not touch active-state memory.
- Mount fewer panes / scope the ticker: rejected by ruling.
- Overlap-layer restructuring (14 layers promoted by Overlap): ceiling ~5-10MB GPU and ~2% main thread; the fixes change scroll or paint behavior. Not worth an A/B unless the ceiling changes.
- Glide residue `rotate: 0.0001deg` in `chokepoint.ts`: deliberate, defeats compositor pixel snapping, and already removed at rest. Leave it.
- Go backend: not a contributor.

## Open

- Upstream issue/PR for the reconnect double registration (svelte `main` matched 5.56.8 on 2026-08-23).
- Chromium perf bug for the `PlaneRootTransform` re-allocation (one-line reuse in `GeometryMapperTransformCache::Update`); not filed.
- Side observation, unfiled: synthetic wheel events on the first composited plane scrolled the whole app view, so the app root may be scrollable.
