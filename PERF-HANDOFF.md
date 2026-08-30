# Perf/memory campaign result - Agent Overflow, 2026-08-29

## Current result

This section supersedes the open work and tentative recommendations in the
2026-08-27 starting brief below. The detailed experiment ledger remains in
`.claude/skills/perf-investigation/REFERENCE.md`.

The production-shaped workload is six open panes with four active panes. Each
active pane streams a long Markdown response while its pane and nested activity
run follow the live edge with the normal spring. No output is skipped, reveal
cadence is unchanged, Markdown stays rich while streaming, and hidden panes are
not shed.

### Measured outcome

- The final clean WebView2 leg ran the production-shaped workload for five
  minutes with only exact-profile process sampling. It used no trace, heap
  snapshot, or forced GC. During the active window, group private working set
  measured **576.6 MB p50, 630.3 MB p95, and 643.4 MB max**. This is the counter
  Task Manager shows for the WebView2 group and lands inside the revised
  600-650 MB heavy-use target.
- The benchmark then interrupts all four infinite mock turns at once. That
  deliberately synchronous teardown reached 675.3 MB after the active window;
  ordinary completed full-row updates now drain through the readable cursor,
  while killed/errored turns still snap immediately by design. Active-window
  per-role maxima were 296.6 MB GPU and 337.4 MB renderer. The process census
  stayed at six.
- The clean blank-window floor was 160.1 MB private working set before the
  workload. A prior five-minute leg started at 720.1 MB because WebView2 had
  briefly restored the preceding giant fixture before `HarnessReset`; that leg
  is measurement contamination, not an app regression, and is excluded.
- The final five-minute run fit 86.1% of work inside 6 ms, 92.8% inside 8 ms,
  and 99.9% inside 16 ms. Busy p95 was 8.8 ms; frame-gap p95/p99 were both
  12 ms. The only long task/frame was the simultaneous four-turn interrupt
  (110 ms), not a steady streaming tick.
- Removing visually inert syntax spans cut the same five-minute DOM peak from
  41,484 to 32,416 nodes (**-21.9%**) while every 30-second scroll-height sample
  stayed equal. JS heap max also fell from 65.3 to 64.5 MB. This change keeps
  text, color, copy, selection, and code-block controls unchanged.
- Forced layouts fell from **919 / 178.4 ms per 15 seconds** after the first
  scheduling pass to **28 / 2.0 ms**. Pane springs, reveal work, and nav-rail
  sync share one frame owner; spring geometry runs before DOM updates so its
  scroll writes consume the prior clean virtualizer sample.

### Correctness and flicker validation

- The photographed code-fence spill was an ordering bug, not a Markdown feature
  gap. Provider item events now serialize in wire order, and the incremental
  parser carries exact boundary state across arbitrary chunks.
- The longest DOM watcher ran for 100 seconds over 124,856 streamed code units
  and 16,918 canonical advances. The exact final build then ran a 50-second
  pass over 58,496 units and 7,759 advances. Both observed zero source
  regressions, source rewrites, large DOM drops, fence-state mismatches,
  transport warnings, or fence-spill frames.
- The final concurrent 15-second checkerboard capture observed 1,531 render
  passes with zero missing-tile or checkerboard signals. Selection-preservation
  browser tests cover direct-text append and parser-owned block migration.
- Differential tests compare the incremental result with full Marked parsing at
  every append boundary, including arbitrary wire splits, nested blockquotes,
  lists, tables, code fences, Unicode, links, inline code, and incomplete syntax.

### Causes removed in this campaign

- Streaming Markdown retains committed block tokens and reparses only the one
  volatile tail. Safe literal suffixes append directly to the existing text
  node; syntax-bearing suffixes still go through the parser. Completed blocks
  compact to static HTML without changing the rendered result.
- Completed code islands retire independently as soon as their own highlight is
  ready. One pending current fence no longer retains every older Svelte code
  component. Retirement stays bounded to one island per Streamdown owner per
  coordinated frame.
- Syntax capture families with no CSS rule now coalesce into inherited-color
  text instead of redundant spans. A source/CSS tripwire prevents a future
  visual rule from being silently collapsed.
- Reveal state is entity-owned and bounded. Direct DOM updates and Svelte-owned
  migrations share one ordered queue, so the renderer cannot briefly paint an
  older parser result over newer literal text.
- A terminal full-row echo that matches or extends a live assistant/reasoning
  stream is absorbed into that stream's readable cursor. Completed turns no
  longer jump over the remaining reveal or flash a wholesale replacement;
  killed, errored, and declined turns retain their immediate-stop semantics.
- Pane springs share one native frame callback. The global frame coordinator
  runs scroll geometry before reveal/nav DOM work, keeping scroll writes on the
  clean prior layout sample while all DOM updates still land before paint.
- The row ResizeObserver consumes its delivered `contentRect`. It no longer
  reads `offsetParent` for every visible row, which forced layout even though the
  observer had already supplied the needed visibility/size evidence.
- Process memory sampling re-censuses the exact WebView2 profile on every tick,
  follows replacement children, and reports incomplete counter reads. Profiling
  processes are therefore visible instead of being mistaken for app growth.

### Closed leads and measurement limits

- Direct `content-visibility:auto` on completed Markdown blocks was measured,
  not merely rejected on risk. Four 82,271 px timelines collapsed to roughly
  35,000 px and displaced their scroll positions by about 47,000 px because the
  already-offscreen blocks had no remembered intrinsic size. A safe form would
  require measured chunk virtualization plus compensation across find,
  selection, width/font changes, and historic mounts. That is a visible/browser
  behavior project, not a safe optimization for this campaign.
- Do not drop programmatic scroll events. Browser find, selection, and native
  scrolling do not carry reliable causal metadata, so filtering would corrupt
  intent handling.
- Keep `will-change: scroll-position`. Removing it previously raised Layerize
  from 84 ms to 1,922 ms per 12 seconds.
- Trace capture is not a footprint measurement. Chromium's tracing utility can
  retain hundreds of MB, which produced the reported 1.5 GB monitoring peak.
  `probe checkerboard` is capped at 15 seconds and all clean footprint runs use
  only `probe webviewmem` or `bench --meter memory`.
- No concentrated safe CPU or memory lead remains. At five minutes the residual
  cost grows with the physically mounted rich DOM and is native Blink text
  layout, prepaint, paint, and raster work; Markdown/parser CPU is below 1%.
  Moving materially below the measured 8.8 ms p95 / 643 MB active max now means
  within-message display locking/virtualization or a lower reveal cadence. Both
  change visible or browser-native behavior and require an explicit product
  decision.

### Completion state

- The isolated perf profile was reset, verified empty, and shut down through
  its exact instance file. No benchmark or sampler remains running.
- Final gates passed: 10,208 frontend unit tests, 234 Chromium tests,
  `svelte-check`, production frontend build, Go build, full Go tests, and five
  repeated race-detector runs over the ordered EventBus cases.
- This branch contains every safe no-UX-change lead found in the campaign.
  The archived brief below is retained as experiment history. Its open tasks,
  decision sheet, rig state, and teardown notes are not current instructions.

## Archived 2026-08-27 starting brief

> Historical record only. The rigs and open tasks in this section are retired.
> Never run an unscoped harness teardown from these notes. Current destructive
> work requires the exact `--instance <data-root>` target selected by the
> caller.

Second-opinion brief. Full measured ledger: `.claude/skills/perf-investigation/REFERENCE.md`
(floors, every fixed cause with commit, every refuted arm with method). This file condenses it
plus tonight's unledgered findings.

## The complaint

WebView2 desktop app (Wails v3, Go backend in WSL, Svelte 5 frontend, Windows webview).
User is on a 165Hz 2560×1369 monitor, typically 4-6 chat panes with streaming agent output.
Task Manager group total observed to ~810MB, renderer alone 405-552MB, spikes to ~1GB.
GPU process 210-297MB. Plus periodic slight stutter (~5min cadence) during streaming.
User verdict: not a leak (confirmed by measurement), fully attributed, STILL UNACCEPTABLE.
The deliverable is measured MB off the peak, not more attribution. Secondary: they are
angry enough to consider a Go→wasm rewrite; they asked for brutal honesty about what is
actually wrong and every lever that exists, product decisions no longer sacred.

## Ground truth (measured, 4-pane real-content replay on the rig)

Live content is tiny; the rest is churn economics and pools:

- Renderer after a full GC: **27MB live Oilpan + ~57MB v8 live** + ~40MB DOM + malloc.
- Renderer peak (control leg): **654MB** private → forced GC → 364 → regrows 124MB in 30s.
- Allocation churn while streaming: **Blink ~58MB/min** (whole-paragraph reshape per
  streamed append: HarfBuzz ShapeResult, LayoutResult ~27k/30s, PhysicalBoxFragment,
  ComputedStyle) + **JS ~70-107MB/min** (breakdown below). None retained.
- V8 default heap policy lets old-gen garbage pile to ~2-4× live before a major GC →
  standing garbage of hundreds of MB is the sawtooth peak. Peak ≈ churn rate × GC patience.
- Oilpan committed floor: ~180MB committed over ~27MB live under load (128KB pages pinned
  by scattered survivors; fragmentation; decommit only on memory-reducing GC, compaction
  only covers vector-backing spaces). Plateaus, does not leak (2.5-day curve flat).
- GPU process: window-area-bound, NO app content. ~45MB platform floor + ~15 window-size
  staging + 30-50 raster-history pools (PA direct-map + Intel driver win_heap) + ~64MB
  untracked DComp swapchain/ANGLE + tiles (~7.2MB per visible overflowing pane scroller,
  13.8 root). Fully attributed three-point floor ladder in REFERENCE.md. No app lever
  found that doesn't trade perf (purge regrows PAST pre-purge under load: 109→122MB).
- Frame health under full load: median 6.1ms, p99 12.2ms, worst 18.2ms (minor-GC
  one-hitches every ~13s), zero LoAFs (>50ms). Renderer main busy ~22%.
- CPU self-time leaders: native layout/paint ~63% "(program)"; spring scroll's
  `targetScrollTop` forced scrollHeight/scrollTop reads ~15% of busy (165Hz × panes);
  svelte flush ~7.5%; markdown lexing ~0.4% (markdown is an ALLOCATION problem, not CPU).

### JS churn breakdown (alloc sampling, 107MB/60s at 4 streaming panes)

- ~21MB/min: markdown re-lex inside Streamdown's doc-level derived. Vendored
  svelte-streamdown has an incremental append lexer (parseBlocks cache + trailing
  list/table descent) and it WORKS (benched: 70-560B re-lexed per append across scenarios,
  bench at /tmp/mdbench). The cost is CALL FREQUENCY (165Hz reveal flush × panes) times
  per-call minting (table descent replays header+last-row tokens per tick; fresh token
  objects each time). Table tokenizer (processRows/splitCells/splitRow) dominates.
- ~49MB/min: svelte flush bookkeeping — dependency-tracking Set/Map/array allocations per
  reaction re-run. Framework cost per flush × flush rate; nothing in newer svelte removes it.
- ~12MB/min: timeline grouping pipeline (`groupItemsBySubagent` 6, `groupActivityRuns` 3).
  SUSPECT DEFECT: profile shows these re-running per TEXT DELTA under a derived chain
  (bundle index:40284-40297) when they should re-run on membership change only. The
  entity-keyed design (pane.items `$state.raw` + per-row boxes) was supposed to prevent
  exactly this. Unverified; worth a second pair of eyes. If real, ~12MB/min free.
- ~9MB/min: row-UI retention sweeps (`pruneOffscreenRowUiState`/`collectTimelineRowUiRetention`)
  allocate fresh Sets per scheduled pass.
- ~7.5MB/min: row mounts (create_effect chains under renderNode → TimelineLeaf →
  ToolCallCard → CommandOutput). Believed to be legitimate NEW-row mounts at streaming
  cadence + window-edge churn; not verified as recreation. Note: every Streamdown/Block
  instance holds per-COMPONENT lex caches, so any remount re-parses that row's whole
  markdown from scratch — remount frequency matters.

## Tonight's A/B (the first big reduction win)

`--js-flags=--heap-growing-percent=20` via `AGENT_OVERFLOW_WEBVIEW_EXTRA_ARGS` (launcher
env, WSLENV-forwarded; flag verified on the browser process command line).

- Leg A (control, 32min, 4-worker replay of the user's real day): renderer 449MB@3min,
  plateau 558-577, capture peak 654. GPU ~270-290.
- Leg B (flag, same replay): 174-207MB@7min, plateau 265-277MB from 14min through 28min
  (leg A same phase: 558-626). GPU unchanged (no V8 there).
- Leg B end-state churn capture (28min in): 287MB before forced GC → 235 after
  (blink live 13MB, v8 43.8), regrew only 14.4MB in 30s — vs leg A's 654→364 and
  124MB/30s regrowth. The flag halves the plateau AND caps the sawtooth amplitude.
- Frame health IDENTICAL (6.1/12.2/18.3, zero LoAFs). Price: GC CPU 23→296ms per 30s
  (~1% of a core), busy 21.9→24.0%.
- IMPORTANT HISTORY: REFERENCE.md "Ruled out" says this flag was withdrawn twice — that
  entry is now REFUTED BY MEASUREMENT for the streaming-churn workload. The earlier
  reasoning (majors already run every 10s) applied to committed-page growth, not the
  garbage sawtooth. The ledger entry needs updating when this ships.
- User's reaction: still not where they want memory to be. ~200-270MB streaming renderer
  and ~210-290 GPU remain after the flag.

## What has already been fixed (do not re-derive; commits in REFERENCE.md)

Renderer/JS: per-beat wholesale projection rebuild (93582cd5: JS alloc 176→4.9MB/60s on
soak, the biggest churn fix); virtualizer row-identity reuse (fe19980f); composited pane
scrollers via will-change: scroll-position (3e249b5a — killed Layerize-per-scroll-write
and the GeometryMapper churn family); composited activity-run clips (5cc379f0); mask-icon
isolated-SVG-document floor pinning (5ccf3a2e, ~57 docs → 2-6); lucide icons → CSS mask
spans (acc09802); universal-invalidation CSS selectors (9eaa5465, document-wide recalc per
beat → 44 el); reading-anchor hit-testing (a56ca00a); activity-run forced reads
(a3eee4d6); read-free delta deliveries (2a318863 + f712f65a); three mount-time forced
readers → RO timing; sidebar FLIP batching (a84ac3de + svelte flip-phases patch); edit
command page retention (04c0af7e); CommandOutput re-split memo; storm timer churn batch.

Trim system (between-turns memory-reducing GC via launcher CDP): between-turns trim
(271bc36d), activity gate (e9454eb4), reveal-drain wait (7fea82c6), and TONIGHT
070545bc — the gate read only `activeTurns`, which only Codex increments (Claude never
emits EventTurnStart), so for Claude threads every idle trim landed MID-TURN as a
60-133ms GC stall on a ~5min metronome. That was the user's "periodic slight stuttering".
Gate is now three arms (triage.AnyInFlightTurnOrRound / activeTurns / 5s lastActivity).
Verified on the rig: zero trims during 30min of continuous streaming. NOT yet verified
on the user's own app (needs their restart).

Backend (Go): event-log RawMessage embed (facc92a4), primed splice memo (04e5c74c).
Backend is lean: RSS ~93MB, idle CPU 0.4%, active 1.4% of a core. Not a factor.

## Refuted / ruled out (method in REFERENCE.md — do not re-run without new evidence)

BlinkHeapYoungGeneration (3× WORSE, +200MB); in-process GPU (forces software compositing);
RawDraw (blank screen + more memory); BRP disable (2-7MB, security trade); discardable cap
(never binds); storage merge (flag removed upstream); memory-pressure signals (no-op in
WebView2); MemoryUsageTargetLevel=Low (swap thrash); forced purge (regrows past pre-purge);
--single-process / low-end-device-mode / max-old-space-size=128 (launcher notes: crash/
regression risks); per-layer GPU bookkeeping theory (disproven by layer-count comparison);
detached-DOM/v8-bloat/DOM-size fronts (all measured tight); leak hypothesis (dead: live
flat across 2.5 days).

## User rulings in force (violating these wastes everyone's time)

- NEVER trade performance for memory. No off-view/hidden conditional work shedding —
  rejected 7-8 times, permanently banned (pane-bounce is the common case).
- Forced GC / purge is not the solution; target is ACTIVE-use memory.
- Reveal-queue doctrine: nothing skips/rushes/pops the readable drain. (Uniform cadence
  changes are NOT automatically banned — but they are a user decision, offered tonight.)
- NetworkServiceInProcess2 rejected (sandbox trade for 13MB). Don't re-propose.
- The user's running app (CDP 9223) is read-only for mutations; A/Bs run on the rig.
- BUT: in the latest message the user said "forget product decisions I've made" — they
  want every option on the table with honest tradeoffs, THEY pick.

## Archived decision sheet

1. Ship heap-growing-percent (proven: 554→~265 streaming; suggest 30-35 to trade back
   some GC CPU). Needs a launcher change to make it default + REFERENCE.md correction.
2. Reveal flush cadence cap ~30Hz (batch the drain to every ~5th frame, all panes
   uniformly, same chars/sec): cuts ALL churn rows + native layout CPU ~5×. The single
   biggest lever in the system. Visual: text steps slightly chunkier on a 165Hz panel.
3. "Dumb volatile tail": while a block streams, append text imperatively to a text node
   (no markdown/svelte per tick), parse once at block boundary. Kills markdown churn +
   much flush churn. Tail loses rich formatting until its block commits.
4. Spring reads engine geometry (virtualizer totalSize) instead of forced
   scrollHeight/scrollTop per frame: pure CPU win (~15% of busy), zero visual change.
   CAUTION: REFERENCE.md has a "load-bearing, do not optimize" entry for targetScrollTop's
   read (clamp freshness + write-refusal witness) — the fix must preserve those
   properties, e.g. engine-sourced height with real-read resync at consumption points,
   same pattern as 2a318863's cached bottom-target arithmetic.
5. Churn cuts: verify + fix the grouping per-delta re-run defect; reuse Sets in retention
   sweeps; token reuse in the markdown table descent. Est. ~20-30MB/min combined.
6. Go→wasm rewrite: advised AGAINST with reasons (half the cost is Blink text layout,
   identical from wasm; Go's wasm GC is worse in-browser; every real fix is architectural
   and framework-independent).

## Archived venues and tooling

- Historical rig: `~/.agent-overflow-harness` cloned REAL session content (4.6GB, never
  committed). It used CDP 9225 and a transient systemd unit
  `ao-clone-window` running `make harness-wsl` (env: PATH, WSL_INTEROP, HOME,
  WSL_DISTRO_NAME; add AGENT_OVERFLOW_WEBVIEW_EXTRA_ARGS for flag legs).
  It no longer exists. Do not reuse its old unscoped teardown command.
- Replay driver: `/tmp/ao-dayreplay7.sh` as unit `ao-dayreplay7` (4 workers, real-cadence
  turn replay from the cloned DB; threads occasionally WEDGE and are abandoned — known,
  accepted). Mem curve: unit `ao-memsample` → `/tmp/ao-dayreplay-mem.csv` (HH:MM:SS,gpuMB,rendMB
  via WMI). Leg markers are comment lines in the CSV.
- Probes: `scripts/perfprobe/probe <name>` (staged to Windows node; WSL cannot reach
  Windows loopback — a refused curl to 9225 does NOT mean the app died). Key probes:
  overview, memdump [--gpu], churn 30 (forces GC), alloc N (set AO_ALLOC_INTERVAL=131072
  under heavy churn or the CDP socket drops mid-reply — fix committed ba3619f3),
  cpu N, framedrops N, tiles, layersfull, gpuheap, gpuinfo, procinfo.
- Powershell from zsh: inline -Command quoting silently fails; stage a `.ps1` under
  `%LOCALAPPDATA%/Temp` and run `-File`. Historical helpers were `wv2census.ps1`,
  wv2age.ps1, wv2flags.ps1, wv2sample.ps1.
- Launcher log (trim events, GC durations): `%APPDATA%/agent-overflow/launcher-harness.log`
  (user app: `launcher.log` in same dir).
- Markdown lexer bench: /tmp/mdbench (instrumented copy of the vendored lexer counting
  re-lexed bytes per append; scenarios for table/list/prose/code tails).

## Archived state at first handoff

- Leg B still running (timer ~21:29); final plateau + churn capture pending.
- Trim fix (070545bc) awaiting the USER's app restart for live verification (join
  launcher.log trim stamps vs items.created_at ±5s — expect zero mid-stream trims).
- Tasks open: GPU residue memlog armed on the user's dev app (run `probe gpuheap` after
  hours of real use); grouping per-delta defect unverified; decision sheet unanswered.
- Parked separately: run→prose spring-jump bug (memory file run-prose-spring-jump-bug.md).
