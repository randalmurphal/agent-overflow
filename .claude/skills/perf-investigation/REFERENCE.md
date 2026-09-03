# Perf investigation reference

What a reading means, what has been ruled on, and which mechanisms in
this app and in Chromium have already been walked to their owner. It is
not a ledger and carries no baseline numbers: every figure measured on
this app is a snapshot of one build under one load on one day, and the
next investigation compares against a reading it takes itself, on the
same build and the same load, in the same session. A number from this
file is never the thing a user's observation is checked against.

## Reading a number

- Task Manager's memory column is private working set; memory-infra
  `private_kb` matches it within a few MB. Rows: "Agent Overflow (dev)"
  is the renderer, "GPU Process", "Manager" is the browser process,
  "WebView2 Manager (N)" is the group sum. Five other WebView2 apps run
  on the same box; identify the app's processes by `--user-data-dir`
  on the command line, never by image name.
- The machine is an Intel iGPU with ANGLE on D3D11 and GPU raster, so
  every texture byte is system RAM in the GPU process's private set.
  That process is raster tiles + shared images + skia (the part the app
  drives, screen-area-bound) over service malloc pools and untracked
  D3D/DirectComposition memory (window-size-bound). It takes ~10 min to
  settle down from a boot's paint burst and moves ±10-15MB with
  activity within an hour. Never A/B against one early sample, and never
  read one Task Manager tick of it as growth.
- Renderer private working set under streaming is churn rate × GC wait,
  not live size: V8 lets garbage pile to a multiple of the live heap
  before collecting. `blink_gc` committed over `blink_live` is Oilpan's
  fragmentation, not retention; sweeping empties pages but keeps them
  committed, and a memory-reducing GC returns only fully-empty 128KB
  pages, so the committed floor RATCHETS with session age as survivors
  scatter across pages (cppgc compacts only vector-backing spaces). Live
  flat while committed climbs is that mechanism; live climbing across a
  forced GC is a leak. `probe churn` splits the two in one shot.
- Ordinary major GCs run every ~10s here and sweep without returning
  pages. Only the memory-reducing path (page hidden, memory pressure,
  `HeapProfiler.collectGarbage`, which is `LowMemoryNotification`,
  Oilpan included) decommits. `Memory.simulatePressureNotification`
  does NOT trigger it in WebView2's renderer at either level.
- A memory-reducing GC re-pays itself: forced purges of the GPU service
  heap and the renderer both regrew past their pre-purge level under
  continuing use. User ruling: forced GCs are not the answer, the target
  is active memory; the lever is churn rate.
- The observer changes the number. A tracing session spawns a
  TracingService utility process and can retain hundreds of MB; a
  `Runtime.queryObjects` poll (the detached census) runs a full
  memory-reducing GC before it answers and pins the renderer at a floor
  it never reaches on its own, hiding every peak; a threshold-triggered
  detailed dump walks every heap in every process and reads as lag.
  Footprint curve and retention census are separate runs, and neither
  is a leg of a footprint measurement.
- Task Manager's `DEBUG=1` dev app carries `VITE_AGENT_OVERFLOW_UI_TRACE`
  + `UI_ORACLES`; the oracles snapshot chat DOM into the trace. Launch
  with `UI_ORACLES=0` to shed that tier before attributing renderer CPU.
- In a heap snapshot, detachedness is the node field
  (`detachedness === 2`); there is no `Detached ` name prefix. A path
  `system / Context → <moduleVar> → reactions → derived` is a
  module-scope svelte signal holding a reaction from a dead component;
  an effect with `fn === null` in the derived's parent chain was
  destroyed, so the derived outlived its owner.
- Trace-event counts are ground truth; cpuprofile self-times on native
  bindings are idle-attribution inflated (thousands of `TimerInstall`
  cannot cost hundreds of ms). Timeline trace locations are 1-based.
- Headless frame histograms are 60Hz-vsync-quantized and see nothing of
  a 165Hz stutter; forced-layout attribution needs an unminified build
  (`pnpm exec vite build --minify false`, then `go build -o
  bin/agent-overflow .` because the binary embeds dist) and Playwright
  `browser.startTracing` around the workload, counting forced
  UpdateLayoutTree/Layout by top JS frame.

## Rulings (never re-propose)

- Never trade performance for memory. Off-view prepaint or tile
  shedding, `animation-play-state` pausing off-view, mounting fewer
  panes, scoping or slowing a ticker, conditional unmounting: all
  rejected, repeatedly. Pane-bounce is the common case. Make the unit
  cheaper.
- Layer promotion doctrine: the only authored `will-change` is
  `scroll-position` on `.pane-scroll-surface`, where the scroll offset IS
  the chokepoint's value. Transform promotion of timeline content gave a
  second paint position and left WebView2 presenting stale pixels;
  `architecture.test.ts` rejects it. Cost layers, never count them: a
  composited opacity animation promotes only its own element and adds no
  measurable texture, so a layer census is not a reason to demote one.
- Ambient indicators are CSS keyframes, opacity-only, `steps()`. An
  inline style write per tick costs one WHOLE-DOCUMENT lifecycle
  regardless of element count (4 dots and 40 dots cost the same); a
  compositable property belongs in a CSS animation. `steps()` pins the
  present rate to the art's frame rate, which is what keeps the
  2026-07-04 vsync-present incident closed; a smooth infinite animation
  reopens it. Never animate a custom property on the document root: a
  root write invalidates style for the whole document. The glow stays on
  the JS ticker because box-shadow spread growth has no opacity
  re-expression, and the ticker sleeps when no glow is mounted.
- Any animation, anywhere in the document, flips the compositor into
  drawing on the frame deadline instead of waiting for raster. It is
  binary (one animation costs what thirty do) and document-wide. The
  user accepted the mode because the sprite, LED and spin are wanted;
  there is no middle position.
- The item-event flush applies every mounted pane's beat in one rAF.
  Round-robining panes across frames was built and refuted by a
  controlled A/B/A/B: tall frames are one pane's own beat, un-merging
  multiplies the per-flush fixed costs. Tripwire in `events.test.ts`.
- The spring's per-tick `targetScrollTop` forced read is load-bearing
  (same-frame clamp, clamp witness, write-refusal classification). The
  lever on forced-read cost is the DIRT'S SCOPE, never who reads first:
  a forced reader pays for whatever invalidation is pending, and
  removing one reader hands the bill to the next one standing.
- Thread-switch coldload forced-layout elimination past the three
  ResizeObserver-timing fixes: stop-loss. Removing readers moved no wall
  time; the rest is scroll-package reads with a low ceiling on a
  one-shot, script-dominated gesture. Resume only on a user flag about
  switch latency specifically.
- Chromium flags tried and closed: `BlinkHeapYoungGeneration` makes it
  far worse (barrier cost, delayed majors); `--in-process-gpu` forces
  software compositing in WebView2; `RawDraw` renders a blank window
  and costs more; `PartitionAllocBackupRefPtr` off is a few MB for a
  use-after-free mitigation; `--force-gpu-mem-discardable-limit-mb`
  never binds (retention is pool inventory, not cache policy);
  `NetworkServiceInProcess2` works but was rejected by the user (sandbox
  trade); WebView2 `MemoryUsageTargetLevel=Low` swaps rather than
  reclaims. Launch-flag door for future arms:
  `AGENT_OVERFLOW_WEBVIEW_EXTRA_ARGS` (launcher, WSLENV-forwarded).
- `--js-flags=--heap-growing-percent=N` is the one flag that measured
  as a real lever on the streaming sawtooth (halved the renderer
  plateau, frame health identical, ~1% of a core in GC CPU). Making it
  a launcher default is an OPEN decision-sheet item, the user's call.
- Sidebar: a live-activity bump must not rewrite the `threads` or
  `projects` arrays; those bumps are boundary events and live in
  per-thread boxes, so the animated each-blocks reconcile and FLIP only
  on a real reorder. Tripwires in `ProjectThreadList` and the store
  identity tests.
- The idle memory trim (between-turns memory-reducing GC through the
  launcher's DevTools bridge) fires only after input or a turn, never
  while any provider turn or round is open (provider-agnostic gate,
  Claude never emits turn-start), and never while a pane is still
  draining its reveal queue. Wire-complete is not reader-complete. Its
  reach is Windows/WSL only; native desktop and `--connect` have no
  consumer (open).
  It installs only once the bootstrap manifest has answered host
  presence (`pageGrantsResolved()`): the remote-access merge read
  `hasScope('host')` at mount, before that answer existed, and the trim
  was a no-op for the life of every page (fixed 2026-09-03). The first
  thing to check when idle renderer memory stops returning to floor is
  the launcher log: no `webview trim: renderer GC done` line for an
  hour of use means the detector never installed.
- Direct `content-visibility: auto` on completed markdown blocks loses
  scroll position (no remembered intrinsic size off-screen); a safe
  version is a product project, not an optimization.
- Backend residency is not a contributor; backend ALLOCATION churn was
  (fixed causes below). Remaining churn left deliberately: provider
  stream `RawMessage` copies, `readBoundedLine`, the gitwatch liveness
  probe's exec pipes, `readClaudeTaskOutputFile` (event-driven, bounded).

## Mechanisms and their owners (fixed; do not re-derive)

Each line is the class, where it lived, and what enforces it now. Commit
hashes are for `git show`, not for the numbers in them.

- Timeline planes as composited layers fed Chromium's
  `PlaneRootTransform` re-allocation on every property-tree generation
  bump (`GeometryMapperTransformCache::Update`, no reuse). Removed the
  `will-change` promotion (`7b29f9d6`); motion goes through `scrollTop`.
- Pane and activity-run scrollers without a composited scrolling layer
  ran a full main-frame lifecycle per scroll offset write (JS or wheel
  alike). `.pane-scroll-surface` with `will-change: scroll-position`
  takes Blink's direct scroll-offset-transform path (`3e249b5a`,
  `5cc379f0`); that also zeroed the transform-cache churn family.
- Every scaled `<svg>` root is a non-2D-translation transform node whose
  cache regenerates per bump; ~400 lucide roots did that while
  scrolling. Icons are CSS-mask spans (pnpm patch, `acc09802`), and each
  distinct data-URI mask cost an isolated SVG document whose singletons
  pinned Oilpan pages, so masks reference a same-document sprite
  (`5ccf3a2e`). Fragment-addressed sprites do not share documents.
- Blink retains one edit command per typed character for the element's
  lifetime, pinning Oilpan pages; the composer swaps its `<textarea>`
  after every send (`04c0af7e`; `editcmdpages` verifies).
- Featureless compounds in the GLOBAL sheets (`> :last-child`,
  `* + *::before`) land in Blink's universal invalidation sets, so any
  mutation anywhere recalcs whole subtrees. Every compound keys on a
  feature; `styleInvalidation.test.ts` sweeps (`9eaa5465`).
- The per-beat wholesale projection rebuild was the renderer streaming
  churn root cause: `groupItemsBySubagent`'s fast path disqualified on
  ordinary completions, card nodes minted fresh poisoned the run cache,
  `buildRun` re-walked per beat. Reference-stable nodes and lean builds
  (`93582cd5`); the virtualizer reuses row identity when fields match
  (`fe19980f`). The secondary Blink churn classes vanished downstream.
- Mount-time geometry reads force a whole-document layout against the
  mid-mount dirty tree; a ResizeObserver's first delivery is post-layout
  and free. Read mount geometry there (`baa816dd`, `44ef8304`,
  `d1b09795`), and never query `offsetParent` for visibility the RO
  entry already answers with 0×0 (2026-08-28 fix).
- Scroll delivery reads: content deliveries decide from cached
  bottom-target arithmetic keyed on height (`2a318863` + `f712f65a`,
  which fixed the delta double-count that rested surfaces 8px short);
  the activity-run clip reads geometry only on reader gestures
  (`a3eee4d6`); the reading anchor derives from engine offsets instead
  of `elementFromPoint` (`a56ca00a`); the hidden overlay scrollbar
  thumb is inert (`a3c4d7c1`); the tail-clamp slide uses RO geometry
  (`0d23c1b3`); the toolbar density measurer is RO-driven (`abda8210`).
- Timers: the scrollend fallback and the quiet-work scheduler keep one
  standing deadline instead of clear+set per event (`3cfb6131`,
  `e2a72036`); reveal smoothers share a wall-clock grid so concurrent
  panes render in the same frame (`b88fb54b`); springs, smoothers and
  rail sync share one rAF owner with a before-DOM-update phase for
  scroll writes (`animationFrameBatcher.ts`).
- Svelte patch hunks (frontend/AGENTS.md § Vendor patches): reconnect
  dedupe (a dirty derived registered twice kept a closed pane's DOM
  alive), flip-phases (animated-each apply interleaved read/write per
  row, N forced passes per reorder), destroy-pass errors, flush-loop
  caps, ownerless roots. `sidebarFlip` replaces stock `flip()`'s
  per-row computed-style reads with rect math.
- Send lag: the composer's textarea swap defers to idle (`7a0919ee`);
  the rest of the Enter task is the tier-move FLIP plus row mount.
- `@formkit/auto-animate` cold-polled the sidebar (forced layout +
  IntersectionObserver rebuild per element per 2s); replaced by
  `animate:flip` (`54d04e72`).
- Terminal/code islands: a completed row that extends the smoother's
  source enters the readable cursor instead of replacing the row; code
  islands retire per record. Inherited-color syntax spans collapse to
  plain text (`syntax.css` cross-check).
- Backend allocation: the debug provider log embedded frames as
  `json.RawMessage` (`facc92a4`); `HighlightPatchTextPrimed` memoizes
  spliced-document parses (`04e5c74c`); the Codex resume rollout tail
  arms on relevance and reuses its buffer (`b69f858d`). go-tree-sitter's
  `readUTF8` copies input twice per parse (upstream, declined locally).
- Nav rail `data-current` scrubbed every tick on every pass; the claim
  is element-keyed like the module's other applied caches. Rule for any
  imperative DOM writer: a per-member sweep beside a diff-only writer
  strands the DOM.
- Streaming markdown: one serialized dispatch queue per wire channel,
  one ordered reveal queue across direct appends and parser migrations,
  completed blocks retained, only the volatile tail reparses.

## Classes named and left (attributed, no app lever)

- Whole-paragraph reshape per streamed append (HarfBuzz glyphs,
  ShapeResult, LayoutResult, fragments, ComputedStyle) is the product's
  own layout churn. DOM writes are tens of characterData/s; there is no
  app-side amplifier.
- Post-layout hover `HitTest` at the last cursor position re-runs per
  dirtied frame; no `elementFromPoint` caller is behind it.
- Svelte flush dependency-tracking allocation per reaction re-run is
  framework-fixed; the remaining JS churn split (markdown re-lex of the
  tail, grouping per text delta, retention sweeps) is the open
  decision-sheet map below.
- The activity-run head-advance gBCR pair must price old and new DOM
  separately; one forced pass per window advance is inherent.
- Overlap-promoted pane chrome (rails, fades, the floating composer)
  must take its own layer to keep paint order over a composited
  scroller, and its tiles cover painted regions only.
- Uptime-scaling GC pauses (script-less LoAFs growing with session
  age) are bounded by the churn sawtooth peak, not by live growth; 8h
  and multi-day curves show live flat. Lever is churn, never leak
  hunting. `frame.loaf` records carry `heapBeforeMb`/`heapNowMb`: a heap
  drop across a script-less stall is the GC verdict.

## Instruments and gotchas

- Online probes need an ownership manifest (`AO_PERFPROBE_MANIFEST`,
  README § Prepare a probe manifest) naming a harness instance and a
  page whose URL carries `?page=<marker>`. The `make dev-wsl` window's
  URL carries no marker and the dev app is not a harness instance, so
  since 2026-08-29 NO online probe can attach to the user's dev app;
  only harness and soak instances are probeable. Read-only OS-level
  reads (a PowerShell `Get-CimInstance Win32_Process` filtered by the
  app's `--user-data-dir`) are the dev app's only instrument until that
  gap is closed.
- One tracing session per browser: `sample --every`, `memdump`,
  `churn`, `tiles`, `frames`, `ab` collide. Stop the sampler first.
  `probe frames` traces must stay ≤~120s (the reader string-concats).
  `checkerboard` rejects durations over 15s.
- A/B on a rig, two runs per leg, before-binary built from clean HEAD in
  a temp worktree; single runs on a busy desktop carry contention noise.
  A/B/A when the effect is small.
- `probe sample --detached` forces a GC per tick and flattens the curve
  it measures; run the census separately. Detach long samplers with
  `systemd-run --user` (SKILL.md step 2).
- memlog (`--memlog=gpu`) attaches after GPU boot: it attributes growth
  only, never the boot floor. `probe gpuheap` reads it.
- Probes take POSITIONAL args (`driveburn "<thread title>" "<prompt>"`),
  and a throwing probe exits nonzero (`00965f76`).
- Mock scenarios reach only sessions created after they are set; a
  thread whose session already played one replays it. Provider must
  match the thread (codex thread + claude scenario = canned reply).
  Reveal backlog decouples wire timing from paint timing; stage
  "mid-glide" via reveal time, not wire time.
- Stopping the WSL `make` unit leaves the Windows launcher and webview
  alive; tear down with `bin/ao-harness down`. WSL cannot reach Windows
  loopback (a refused curl is not a dead app). PowerShell inline
  `-Command` quoting from zsh fails silently; stage a `.ps1` under
  `/mnt/c/.../Temp` and run `-File`.
- The soak renderer can carry a frozen multi-GB `directMap` allocation
  left by a probe serialization; while it stands, soak absolutes are
  meaningless and only deltas count. Restart the soak to clear it.
- Programmatic `scrollTop` writes are not reader intent in either
  direction; `probe scrollgesture ... down` is the re-engage tool.
- Analysis recipes: windowed cpuprofile attribution over tall-RunTask
  ranges; classify svelte flush as samples with an ancestor in
  {flush_queued_root_effects, flush_queued_effects, process_effects,
  update_effect, update_derived, execute_derived, update_reaction};
  steady-state profile = settle, `ao-harness ui open`, `Profiler.start`,
  `scenario set` + `send --wait`, `Profiler.stop` (bench reloads the
  page and kills a Profiler session); join LoAF page time to capture
  time by the one event in both lists; date GC stalls from launcher.log
  `GC done` lines; join launcher trim stamps against `items.created_at`
  to prove a trim landed mid-turn.

## Open

- Streaming-churn decision sheet (JS: markdown tail re-lex at flush
  rate, svelte flush bookkeeping, grouping pipeline re-running per text
  delta (suspected defect, unverified), retention-sweep Sets; levers:
  flush cadence cap, dumb volatile tail, engine-geometry spring reads,
  `heap-growing-percent` default). All awaiting user picks.
- Idle-trim reach gap on native desktop and `--connect`.
- The dev-app probe gap above.
- Upstream: svelte reconnect double registration (issue/PR), Chromium
  `PlaneRootTransform` reuse (not filed, no longer reached).
- Side observation, unfiled: synthetic wheel events on the first
  composited plane scrolled the whole app view; the app root may be
  scrollable.

## Red/green recipe for a svelte patch hunk

Prove the bug on the previous patch and the fix on the new one without
touching node_modules by hand: keep copies of the new patch and lock
outside the tree; `git show HEAD:frontend/patches/svelte@<v>.patch >
frontend/patches/svelte@<v>.patch`, same for `pnpm-lock.yaml`, `pnpm
install --offline`, run the suite (must fail); copy the saved files back,
`pnpm install --offline`, run again (green). Edit a hunk with `pnpm patch
svelte@<v> --edit-dir <dir>` then `pnpm patch-commit <dir>`; only the
patch hash changes in the lock. `svelte/internal/client` exports
`get/set/state` in its types but `derived/effect/effect_root` only at
runtime (import the namespace and cast).
