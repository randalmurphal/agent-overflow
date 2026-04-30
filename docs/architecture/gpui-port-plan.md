# GPUI Port Plan

Reference for porting Agent Overflow's frontend from Wails+Svelte+WebView2 to Rust+GPUI. This document captures what was validated on the `gpui-spike` branch, what wasn't, the architectural reasoning, and the path to a proper port.

The spike is read-only and limited in surface but proves the core question: **can we hit the user's <150 MB idle / <300 MB active memory targets without giving up native desktop UX?** Answer: yes, on Windows-native MSVC the idle tree is **131 MB**, a 5.7× reduction vs default Wails / 3.5× vs the most-optimized Wails configuration.

This is a spike branch, not a finished port. Read it as a recipe + a set of validated decisions.

## TL;DR

- **Backend stays unchanged.** The Go side (transport, SQLite, providers, all of `internal/`) is reused as-is. The proper port replaces only `frontend/` with a new `frontend-rs/` Rust crate.
- **Wire-level compatibility is proven.** FNV-1a 32-bit method-id hashes match Wails byte-for-byte against pinned vectors; the `gpui-spike` end-to-end path connects, lists projects + threads + items from the real database, and renders them.
- **Memory target is hit.** Native Windows MSVC debug build idle is 131 MB tree (107 MB GUI + 24 MB Go backend), under the user's `<150 MB idle` target.
- **Toolchain story is reproducible.** Linux→Windows MSVC cross-compile via `xwin` works with no Microsoft tooling on the corporate Windows machine.
- **The spike is intentionally narrow.** Read-only, no event subscriptions, no composer, no markdown rendering. The proper port plans those out below.

## Why this spike

The investigation that preceded the spike (memory profiling of the Wails+WebView2 build on Windows + WSL) established that the dominant memory cost is **Chromium baseline**, not the user's code. The Svelte frontend's own runtime is lean (4.5 MB JS heap, 239 DOM nodes); Chromium contributes the remaining ~700 MB.

Two paths could have addressed this:

1. **Strip Chromium features via `--single-process` + flag list.** Tried — saves ~290 MB. Lands at 451 MB / 272 MB private. Still in "Electron family" territory the user explicitly didn't want to be in.
2. **Replace Chromium with a non-browser GUI runtime.** This is the GPUI path.

Alternatives evaluated and rejected:

- **Servo embeddable browser** (`servo` 0.1.0 on crates.io). Tested — laggy scroll, visible rendering issues, ~494 MB single-process. Doesn't beat WebView2 meaningfully and brings its own bugs.
- **Verso** (Servo-based Tauri runtime). Archived (last commit Oct 2025); `tauri-runtime-verso` dead. Not viable.
- **Custom OpenGL / WGPU GUI from scratch.** Discarded — multiple person-years of work to reach feature parity with what GPUI ships today.

GPUI is Apache 2.0, on crates.io, and powers Zed in production. Other agentic-UX apps (Arbor for agentic coding workflows; hunk as a Codex orchestrator) ship on it. A production-grade component library exists (`gpui-component` from longbridge). Risk profile is acceptable for a real port.

## Architecture

```
                     +-----------------------------------+
                     |  Existing Go backend (unchanged)  |
                     |                                   |
                     |  - 148 RPC methods                |
                     |  - HTTP + WS transport            |
                     |  - Provider sessions              |
                     |  - SQLite persistence             |
                     +-----------------------------------+
                                ^
                                |  WebSocket (loopback)
                                |  JSON frames per /internal/transport/frame.go
                                v
                     +-----------------------------------+
                     |  frontend-rs (new)                |
                     |                                   |
                     |  tokio runtime:                   |
                     |    - transport supervisor         |
                     |    - bootstrap subprocess         |
                     |    - RPC futures                  |
                     |                                   |
                     |  ---  mpsc<UiUpdate>  --->        |
                     |                                   |
                     |  GPUI foreground:                 |
                     |    - Entity<*Model>               |
                     |    - Views (Render impl)          |
                     |    - Window event loop            |
                     +-----------------------------------+
                                |
                                v
                          Native window
                       (DirectX 11 / Win32)
```

### What stays exactly as-is

- The entire `internal/` tree — transport, dispatcher, providers, store, triage, settings, etc. Nothing changes.
- `internal/transport/frame.go` defines the wire. The new client honors it byte-for-byte.
- `app.go` and `app_*.go` continue to be the bound App methods. They become the Go backend's RPC surface unchanged.
- `bin/agent-overflow` (Linux) and `bin/agent-overflow.exe` (Windows) continue to be the headless backend binary the GUI spawns.

### What goes away

- `main.go`'s Wails-window construction — replaced by the Rust binary's GPUI window construction. The headless mode (`--print-url-fd`) is what survives from `main.go`.
- `frontend/` (Svelte+Vite) — replaced by `frontend-rs/`.
- `cmd/agent-overflow-windows/` (the WSL launcher that invokes WebView2) — replaced by direct Windows-native execution of the Rust binary, which spawns the WSL backend itself when needed.

The transport boundary stays clean. The new client has the same access privileges as the Svelte one — same method allow-list, same `LocalOnlyMethods` enforcement on non-loopback peers.

### Threading model

GPUI's foreground context (`AsyncApp`) is `!Send` by design — it must stay on a single thread. tokio futures are typically `Send` and run on a thread pool. These two don't compose directly.

The spike resolves this with a **one-way mpsc bridge**:

- **Tokio side** owns the transport client, bootstrap subprocess, and every RPC future. Runs on a small worker pool (2 threads).
- **GPUI side** owns `Entity<*Model>` instances and the window event loop.
- An `mpsc::UnboundedSender<UiUpdate>` lives on the tokio side. The GPUI side runs a foreground async task (`cx.spawn(...)`) that drains the receiver and applies each `UiUpdate` to the right entity, calling `cx.notify()` to trigger re-render.
- UI events (sidebar click, retry button) call back into tokio via the runtime `Handle::spawn(future)` directly. Those tasks return their result through the same `UiUpdate` channel.

`AsyncApp` is never sent across threads. That's a hard constraint — every attempt to make it `Send`-friendly fights GPUI's design.

`UiUpdate` is the entire vocabulary the bridge supports. New variants are added as new state surfaces show up. Concrete shape (current):

```rust
pub enum UiUpdate {
    TransportStatus(TransportStatus),
    Projects(Result<Vec<ProjectWithCounts>, String>),
    Threads(Result<Vec<Thread>, String>),
    SelectThread(Option<String>),
    Timeline { thread_id: String, result: Result<PagedItems, String> },
    Bootstrap(Result<(), String>),
}
```

The proper port will grow this with `ItemEvent`, `TurnStarted`, `TurnCompleted`, `ApprovalRequested`, etc.

### View tree

GPUI uses an entity model: state lives in `Entity<T>`, views `cx.observe(&entity, |_, _, cx| cx.notify())` to re-render when the entity changes. There is no separate redux/store layer — entities ARE the store.

Current shape:

```
ui::root::Root  (top-level container)
  |
  +-- Sidebar     (observes threads, selection)
  +-- Timeline    (observes timeline, threads, selection)
  +-- StatusBar   (observes status, threads, timeline)
```

`AppState` (a sibling of the views, not an entity itself) holds the `Entity<ProjectsModel>`, `Entity<ThreadsModel>`, etc. plus the `Transport` handle. The views take an `Arc<AppState>` and read from it.

This pattern keeps "store" updates and view refresh tightly coupled — when the `mpsc` drain receives a `UiUpdate::Threads(...)`, it does `threads.update(cx, |m, cx| { m.list = ...; cx.notify(); })` and every observing view re-renders next tick. No prop drilling.

## Wire layer details

### Frame format

Pinned to `internal/transport/frame.go`. Three frame types:

```
Client → Server:
  {type: "rpc",    id, methodId? | method?, params: [...]}
  {type: "replay", lastSeqByChannel: {chan: seq}}

Server → Client:
  {type: "rpc",   id, result | error: {code, message}}
  {type: "event", channel, seq, data, gap?}
```

Stable error codes: `method_not_found`, `bad_params`, `method_error`, `internal`, `shutting_down`. Match `frame.go` constants exactly.

### Method-id hash

`methodId` is FNV-1a 32-bit of `main.App.<MethodName>`. The Go dispatcher and the Wails binding generator both use this same hash. Verified in `frontend-rs/src/transport/fnv.rs`:

```rust
#[test]
fn matches_wails_pinned_vectors() {
    assert_eq!(fnv1a_32("main.App.ArchiveProject"), 1352159878);
    assert_eq!(method_id("ArchiveProject"), 1352159878);
}

#[test]
fn matches_observed_binding_ids() {
    assert_eq!(method_id("ListProjects"), 2_721_360_259);
    assert_eq!(method_id("ListThreads"), 1_090_132_042);
    assert_eq!(method_id("ListRecentThreadItems"), 2_604_956_482);
    assert_eq!(method_id("GetThread"), 1_098_302_047);
    assert_eq!(method_id("ListRecentTurns"), 1_083_162_294);
}
```

Both tests pass against the actual IDs the Svelte production bindings emit. If this ever breaks, the dispatcher will route nothing.

### Bootstrap shape

Two paths exist on the Go side:

- **Headless**: `agent-overflow --print-url-fd 0 --listen 127.0.0.1:0` writes `__AO_BOOTSTRAP__: {"port":N,"token":"..."}` to stdout (with a leading newline so partial-line preceding log output doesn't break the prefix match). The spike spawns this.
- **HTTP**: `GET /bootstrap.json?t=<token>` returns `{"wsUrl":"ws://...","token":"..."}` from the running Wails-shell server. The Svelte app hits this on first load.

The Rust client accepts both: a `RawBootstrap { wsUrl?, port?, token }` deserializes either, and `Bootstrap::from_raw` synthesizes `wsUrl = "ws://127.0.0.1:<port>/ws"` when only port is present.

Connect: append `?token=<token>` to the wsUrl, dial. Server side validates token via constant-time comparison and only accepts loopback Host.

### Cold-start RPC sequence

What the spike does on launch:

1. Spawn Go backend, parse bootstrap line.
2. Connect WebSocket.
3. In parallel (each on its own tokio task): `ListProjects()`, `ListThreads()`.
4. Auto-select the most-recently-touched non-archived thread, dispatch `ListRecentThreadItems(threadId, 50)`.
5. Render the resulting timeline.

99 ms from spawn to "Connected" status on a warm machine, end-to-end.

## Build toolchain

Three target environments matter:

1. **Linux dev loop** — fast iteration, hardware GPU on real Linux, llvmpipe under WSLg.
2. **Linux→Windows MSVC cross-compile** — the path that produces a Windows-native binary without touching the Windows machine.
3. **Windows-native MSVC build** — works but requires installing Visual Studio Build Tools (~2 GB, IT-visible). Avoided in the spike.

### Linux dev loop

System deps (Debian/Ubuntu/WSL2):

```
sudo apt install -y build-essential pkg-config libfontconfig-dev \
  libssl-dev libwayland-dev libxkbcommon-dev libxkbcommon-x11-dev \
  libxcb-render0-dev libxcb-shape0-dev libxcb-xfixes0-dev \
  libxcb-cursor-dev libvulkan-dev mold
```

Rust toolchain:

```
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y \
  --default-toolchain stable --profile minimal
```

Build the Linux SPA dist (Go's `//go:embed` requires `frontend/dist` to exist at compile time even though we don't use it):

```
cd frontend
corepack pnpm install
corepack pnpm build
```

Build the Go backend and then the GUI:

```
cd ..
go build -o bin/agent-overflow .
cd frontend-rs
cargo build --release --bin agent-overflow-rs
```

Run, with WSLg quirk workaround:

```
AGENT_OVERFLOW_BIN=../bin/agent-overflow WAYLAND_DISPLAY= \
  ./target/release/agent-overflow-rs
```

GPUI 0.2.2's wayland_client binding panics on WSLg's compositor with `UnsupportedVersion`; unsetting `WAYLAND_DISPLAY` forces the X11 fallback which works.

### Linux→Windows MSVC cross-compile via xwin

This is the path the spike uses to produce Windows binaries. Lets you build a Windows-native binary from a Linux host with no Microsoft tooling on the Windows side — only the binary needs to land there.

#### One-time setup

System deps (also need `clang` and `lld` from apt):

```
sudo apt install -y clang lld
. "$HOME/.cargo/env"
rustup target add x86_64-pc-windows-msvc
ln -sf /usr/bin/clang ~/.cargo/bin/clang-cl
ln -sf /usr/lib/llvm-18/bin/llvm-lib ~/.cargo/bin/llvm-lib
cargo install xwin --locked
```

`clang-cl` and `llvm-lib` need to exist as their own commands on PATH; Ubuntu's clang package ships them as drivers but doesn't install the symlinks by default.

Download Microsoft's MSVC SDK (~640 MB) into the worktree:

```
cd frontend-rs
xwin --accept-license splat --output .xwin
```

`.xwin/` is gitignored — each contributor accepts the EULA themselves.

#### What `frontend-rs/.cargo/config.toml` does

Two things beyond what xwin's docs typically show:

1. **Static CRT linkage** (`-Ctarget-feature=+crt-static`) — embeds VCRUNTIME140 into the binary so it doesn't depend on the user's installed VC++ Redistributable version. Without this we hit a real-machine entrypoint-mismatch class of bug that's hard to reproduce.

2. **Manifest embedding via `lld-link`** (see `frontend-rs/build.rs`). GPUI 0.2.2's own `embed_resource()` step is gated `#[cfg(target_os = "windows")]` — meaning it only runs when the **build host** is Windows. Cross-compile from Linux silently skips it. Without the Common-Controls-v6 manifest, Windows loads `comctl32.dll` v5 instead of v6, `TaskDialogIndirect` is missing, and the binary crashes at startup with `STATUS_ENTRYPOINT_NOT_FOUND` before any user code runs.

   The fix: a tiny `build.rs` that emits `cargo:rustc-link-arg=/manifest:embed` and `cargo:rustc-link-arg=/manifestinput:<path>`, pointing at our copy of the manifest under `resources/windows/agent-overflow.manifest.xml`.

#### Build

```
cd frontend-rs
cargo build --target x86_64-pc-windows-msvc --bin agent-overflow-rs
```

Output is `target/x86_64-pc-windows-msvc/debug/agent-overflow-rs.exe` (~32 MB debug). Copy to Windows side and run.

The Go backend also needs to be cross-compiled — that's just `GOOS=windows GOARCH=amd64 go build -o bin/agent-overflow.exe .` from the repo root. modernc.org/sqlite is pure Go so this works without CGO.

#### Debug-build runtime caveat

GPUI 0.2.2's debug-mode shader compile uses `D3DCompileFromFile` with a path baked at compile time via `env!("CARGO_MANIFEST_DIR")`. On a Linux→Windows cross-compile, that bakes the Linux registry path:

```
/home/rmurphy/.cargo/registry/src/index.crates.io-1949cf8c6b5b557f/gpui-0.2.2/src/platform/windows/shaders.hlsl
```

Windows interprets this as `C:\home\rmurphy\...\shaders.hlsl` and `canonicalize()` errors with `ERROR_PATH_NOT_FOUND`. GPUI panics in `DirectWriteTextSystem::new()` because the GPU pipeline initialization (which compiles shaders) failed.

Workaround for the spike: mirror the gpui shader sources at the expected Windows path:

```
mkdir -p "/mnt/c/home/rmurphy/.cargo/registry/src/index.crates.io-1949cf8c6b5b557f/gpui-0.2.2/src/platform/windows"
cp ~/.cargo/registry/src/index.crates.io-1949cf8c6b5b557f/gpui-0.2.2/src/platform/windows/*.hlsl \
   "/mnt/c/home/rmurphy/.cargo/registry/src/index.crates.io-1949cf8c6b5b557f/gpui-0.2.2/src/platform/windows/"
```

Awful path, but cheap.

A **release** build avoids this entirely — GPUI's release path uses pre-compiled shaders embedded via `include!(concat!(env!("OUT_DIR"), "/shaders_bytes.rs"))`. But generating that file requires `fxc.exe` from the Windows SDK, which isn't available on Linux. The natural follow-up is using `dxc` (Microsoft's open-source HLSL compiler that runs on Linux) to produce the bytecode and writing a shim build.rs that emits the right `shaders_bytes.rs` shape. Estimate: 1 day of work.

### Why mingw-gnu doesn't work on Windows

This was the first cross-compile path attempted. It produces a binary that crashes at startup with `STATUS_ACCESS_VIOLATION` before any logging, even with debug build, even after working through:

- The `dlltool.exe` not being in rust-mingw's bundled tools (worked around by swapping in llvm-mingw's `llvm-dlltool` which doesn't need `as.exe`).
- Missing CRT and system libs (worked around with `-C link-self-contained=yes` to force rustc to use bundled libs).
- `libktmw32.a` not being in rust-mingw's lib set (copied from llvm-mingw).
- Manifest not being embedded (would have needed the same build.rs fix).

After all the surface fixes, the binary still AVs at startup. Likely root causes (in decreasing probability):

1. **SEH vs dwarf unwinding incompatibility.** GPUI/Windows code throws Win32 exceptions; mingw-gnu's panic mechanism uses dwarf-based unwinding which doesn't fully interop with Windows SEH.
2. **ABI quirks in `extern "C"` struct returns.** Some Windows API calls return small structs by value; mingw-gnu and MSVC handle this differently.
3. **Static initializer ordering.** Subtle differences in how the two toolchains walk init lists.

GPUI's CI and Zed's release pipeline only test MSVC on Windows. mingw-gnu is officially "Tier 2" Rust support but second-class for Windows-API-heavy libraries in practice. **Don't try to make this work.** The xwin/MSVC path is the right answer.

## Memory measurements

### Methodology

- Idle measurement = process started, window rendered, transport connected, no user interaction. Empty thread list (fresh DB on first launch on Windows because `%APPDATA%` is virgin).
- Linux: `/proc/<pid>/smaps_rollup` for PSS/private-dirty.
- Windows: `Get-Process` for working set, private bytes (`PrivateMemorySize64`).
- "Tree" = GUI process + Go backend process. Wails baseline includes WebView2 children too.

No PowerShell screen captures used per the user's IT constraints.

### Results

| Setup | Tree (RSS / WS) | Private | Notes |
|---|---|---|---|
| Wails default (Win) | 742 MB | 700 MB | Prior baseline; Chromium full |
| Wails `--single-process` | 451 MB | 272 MB | Prior baseline; flag-stripped |
| GPUI Linux release (WSL) | 207 MB | 82 MB | x11 fallback, llvmpipe software Vulkan |
| GPUI Linux debug (WSL) | 241 MB | 174 MB | For comparison |
| **GPUI Windows MSVC debug** | **131 MB** | **119 MB** | This spike's headline number |

The Windows-native debug build is **smaller than the Linux release** because:

- Native DirectX 11 path (no Vulkan-via-llvmpipe overhead).
- Windows-side process accounting doesn't include vmmem-overhead the WSL measurement carried.
- MSVC debug has lighter symbol bundling than rust-gnu debug.

A release build with shaders pre-compiled would land another 10–20% lower on top.

### What's NOT measured (yet)

- **Active state** during a real provider turn (typing, streaming, tool calls). Spike has read-only data so this isn't testable. The user's `<300 MB active` target is unverified — but with a 119 MB private-bytes idle baseline and ~150 MB of headroom to the active target, it's plausibly fine.
- **Memory after long-running session** (24+ hr). No data on whether GPUI/our state grows over time.
- **Memory under heavy timeline scroll** (100 turns, 10K items). The spike loads 50 items max.

These open questions don't block the port decision but should be answered early in implementation.

## Spike scope: what's there, what isn't

### Built

- `frontend-rs/src/transport/`: bootstrap, wire frames, FNV hash, WebSocket client with reconnect backoff and replay-on-reconnect.
- `frontend-rs/src/rpc/`: typed wrappers for the cold-start RPCs (`ListProjects`, `ListThreads`, `ListRecentThreadItems`, `GetThread`).
- `frontend-rs/src/models/`: Rust mirrors of `Thread`, `Project`, `ProjectWithCounts`, `Item`, `PagedItems`, `ItemLane`. Field-by-field match with `internal/store/models`.
- `frontend-rs/src/app.rs`: AppState, the `UiUpdate` enum, the tokio↔GPUI bridge, click handlers.
- `frontend-rs/src/ui/`: `Root`, `Sidebar`, `Timeline`, `StatusBar`. Theme tokens. Click-to-select wiring.
- `frontend-rs/examples/smoke.rs`: integration test that spawns backend, connects, runs the cold-start RPCs, prints results. No GPUI in the loop.
- xwin cross-compile config + manifest embedding (`build.rs`).

### Deliberately NOT built

| Surface | Why deferred | Rough size |
|---|---|---|
| Event channel subscriptions | Out-of-scope for memory measurement | 1-2 days |
| Composer + send | Read-only spike | 1 week |
| Markdown rendering (`pulldown-cmark`) | Item summary suffices for spike | 2-3 days |
| Diff viewer (Shiki replacement) | Not on the cold-start path | 1 week |
| Syntax highlighting (`syntect` or `tree-sitter`) | Same | 3-5 days |
| Settings panel | RPC surface known, UI not wired | 1 week |
| Approvals / design options | Complex flow; reference forge | 2 weeks |
| Terminal panel | Niche; defer until prioritized | 1-2 weeks |
| Live plan / todo panel | Recently shipped on Svelte side | 3-5 days |
| Active-turn registry, streaming | The actual heart of the app | 1-2 weeks |
| Keybindings | Have parser; need binding layer | 3-5 days |
| Release-build path (fxc/dxc) | Blocking before a real ship | 1 day |

Sums to roughly **8-12 weeks of full-time work** for a feature-complete port. Not every surface is required for the v1 port; see the phased plan below.

## The proper port — phased plan

Each phase ends with a milestone that's user-evaluable. Memory measurements at each phase boundary.

### Phase 1: Live data (1–2 weeks)

Wire what already exists in the spike to event subscriptions.

- Subscribe to `provider:item_event`, `provider:turn_started`, `provider:turn_completed`, `thread:updated` channels.
- Apply incoming events to the timeline + threads models. Mirror `frontend/src/lib/stores/events.ts` semantics for upserts, deltas, gap recovery.
- Surface transport status changes in the UI (the model exists; the UI just observes it).

Done state: the spike app, but with live updates. You can run a turn elsewhere (e.g., the existing Svelte UI in another window) and see the timeline update in real time on the GPUI side.

### Phase 2: Composer + send (1 week)

Add the composer to the bottom pane and let the user actually start turns from this UI.

- Text input, multi-line, Tab/Enter behaviors.
- `SendMessage` / `SendMessageWithOptions` RPC wrappers.
- Draft persistence (`SaveDraft`/`GetDraft`/`ClearDraft`).
- Per-thread runtime mode (chat / plan / design).

Done state: this UI can drive a real Claude Code or Codex session end-to-end on simple text prompts.

### Phase 3: Render fidelity (2 weeks)

- Markdown rendering for assistant text via `pulldown-cmark`. Custom GPUI elements for code blocks (no syntax highlighting yet — just `<pre>`-style).
- Tool call expand/collapse with full payload load on demand (`GetPayloadPreview`/`GetPayloadChunk`/`GetPayloadData`).
- Approval prompts inline.
- Active turn working indicator (parity with the recent forge ship).

Done state: visual quality good enough to use as a daily driver for chat-style workflows.

### Phase 4: Syntax + diff (1.5 weeks)

- `syntect` for code-block syntax highlighting (or `tree-sitter` if better for incremental parsing — measure first).
- Diff viewer using `GetCheckpointRangeDiff`/`GetSessionAgentDiff`/`GetWorkspaceCurrentDiff`.
- Side-by-side and inline diff modes.

Done state: code-aware features at parity with the Svelte UI.

### Phase 5: Settings + control plane (1.5 weeks)

- Settings panel (`GetSettings`/`UpdateSettings`/`GetContextSettings`).
- Network settings (LAN bind toggle).
- Provider model picker.
- Keybindings UI.

Done state: full control over the agent runtime.

### Phase 6: Approvals + design + plan revision (2 weeks)

- Approval flow (`RespondToApproval`).
- Design options (`ChooseDesignOption`, `ListDesignArtifacts`, `GetDesignArtifactHTML`).
- Plan revision comments, proposed plan view.

Done state: full agentic-workflow surface.

### Phase 7: Polish + release-build path (1 week)

- Get release builds working: `dxc` on Linux to pre-compile shaders, fix the `OUT_DIR/shaders_bytes.rs` shape, switch to release for distribution.
- Active-state memory measurement during real turns; chase any leak surfaces.
- Crash reporting, logging discipline, panic boundaries.
- Window state persistence.
- Project sidebar (currently we only render threads; projects sidebar is part of the shipped UX).
- Dock the WSL backend lifecycle into the Rust binary properly (today the spike's bootstrap is fine but error paths aren't polished).

Done state: shippable.

## Repository layout for the port

When the port becomes the official path, the suggested layout:

```
/                          (existing Go side stays)
  /internal/                  unchanged
  /main.go                    keep --print-url-fd; remove Wails window wiring
  /app.go, /app_*.go          unchanged (RPC surface)
  /frontend-rs/               new — was the spike
    /src/                       (modules as in the spike)
    /resources/windows/         manifest, icons
    /Cargo.toml
    /.cargo/config.toml         xwin cross-compile config
    /build.rs                   manifest embed
  /frontend/                  REMOVE after Phase 6
  /cmd/agent-overflow-windows/ REMOVE after Phase 7 (Rust binary becomes the launcher)
  /docs/architecture/         (this doc lives here; add new ones as phases land)
```

The cutover happens at Phase 6 boundary: everything that's deferred is non-blocking for daily use, and we can sunset the Svelte path with confidence the user isn't losing critical functionality.

## Library choices

### Locked in (used by the spike)

- **gpui** (Apache 2.0, crates.io 0.2.2) — the runtime. Not pinned to git mainline because we want a stable ABI for the duration of the port. Bump opportunistically.
- **gpui-component** (Apache 2.0, longbridge) — added as a dep but not actively used in the spike. Available when component-library primitives become useful.
- **tokio** with `rt-multi-thread`, `macros`, `process`, `io-util`, `sync`, `time` — transport runtime.
- **tokio-tungstenite** with only `connect` (no TLS) — loopback ws:// only; TLS termination is out-of-process per the existing transport design.
- **serde** + **serde_json** — wire encoding.
- **fnv** — method-id hash.
- **anyhow** + **thiserror** — error types.
- **tracing** + **tracing-subscriber** — logging.
- **parking_lot** — fast Mutex/RwLock.

### Recommended for the port

- **pulldown-cmark** — markdown parsing. Apache 2.0, widely used in Rust ecosystem.
- **syntect** — syntax highlighting. MIT, used by Zed.
- **pretty-diff** or build-our-own — diff rendering. Look at Zed's `crates/diff` for prior art; might be small enough to vendor.
- **tree-sitter** — already a transitive dep of gpui-component; useful if we want incremental parse for editor surfaces.
- **dirs** or **directories** — config/data dir resolution on Windows/macOS/Linux.

### Avoid

- **reqwest** — heavy. Our transport doesn't need a full HTTP client (the WS path is enough); if we add HTTP later, prefer `ureq` or hyper directly.
- **eframe / egui** — alternative immediate-mode GUI. Don't switch off GPUI mid-port; keep one runtime.

## Open questions for the port

These don't block starting; they need answers as implementation progresses.

1. **GPUI version pin.** Crates.io 0.2.2 is from October 2025 and stable. Zed mainline is more current. Stick with crates.io until we hit a missing feature; bump when needed.
2. **Error UI surface.** Currently transport errors live as `Option<String>` on each model. Need a unified toast/notification surface; defer to forge's pattern.
3. **Settings persistence.** Go backend owns this via SQLite + JSON files. Just call the existing RPCs.
4. **Window state persistence.** GPUI doesn't have a built-in "restore last window position." Need to write to disk on close, restore on open. Trivial.
5. **Multi-window support.** Forge has it; the Svelte app has it. GPUI supports multiple windows natively. Defer to phase 7.
6. **macOS support.** GPUI supports macOS first-class (Zed is a macOS app primarily). The Linux→Windows xwin pipeline is the same shape as Linux→macOS via osxcross or doing it on a Mac. Out of scope for the v1 port.
7. **Auto-update.** Wails v3 ships an auto-updater (currently deferred per CLAUDE.md). For the GPUI binary, look at `self_update` crate or similar. Defer to post-port.
8. **Keybindings parser reuse.** The Svelte `keybindingParser.ts` is well-tested; consider porting the test cases verbatim and reimplementing in Rust.

## Reference apps and libraries to study

- **Zed** (`github.com/zed-industries/zed`) — `crates/agent`, `crates/agent_ui`, `crates/acp_thread` for agentic chat UI patterns. Reference, not copy.
- **Arbor** — agentic coding workflows app on GPUI; closest spec match to Agent Overflow.
- **hunk** — Codex orchestrator on GPUI; useful for "talk to a CLI agent and render its events" patterns.
- **gpui-component** (`longbridge/gpui-component`) — production-grade shadcn-style UI components; pull primitives in as needed.
- **awesome-gpui** repo — directory of GPUI apps and libraries.

## Lessons logged

Things to remember from this spike that aren't obvious from the code:

- **Don't try to make `AsyncApp` Send.** GPUI's foreground context is single-thread by design. Bridge with channels.
- **Don't try mingw-gnu on Windows.** It crashes at startup; debugging it is a tar pit. xwin/MSVC is the only reliable path.
- **D3DCompileFromFile bakes a CARGO_MANIFEST_DIR path.** Cross-compiles need either runtime path mirroring (debug) or pre-compiled shader bytecode (release).
- **GPUI's `embed_resource` step is host-gated.** Linux→Windows cross-compile silently skips manifest embedding; binary crashes with `STATUS_ENTRYPOINT_NOT_FOUND`. We bypass with our own `build.rs`.
- **`fxc.exe` is a real blocker for release.** Linux-side Microsoft tools aren't first-class, but `dxc` (open-source) does cover this case if we're willing to wrap it.
- **The transport client is the easy part.** The wire is documented, the FNV hash is pinned, the bootstrap is well-defined. Most of the real work is in the UI and event-handling surface.
- **GPUI on WSLg via Wayland is broken.** Use X11 fallback (`WAYLAND_DISPLAY=`) until upstream fixes the version negotiation.
- **The Svelte frontend is genuinely lean.** 4.5 MB JS heap. The win comes entirely from dropping Chromium, not from rewriting the user's code. The Rust port replaces only the runtime, not the architecture; the UI shape ports over almost 1:1.

## Status of this branch

`gpui-spike` contains:

- `frontend-rs/` — the spike code (~1,500 lines of Rust).
- xwin cross-compile setup (`.cargo/config.toml`, `build.rs`, `resources/windows/agent-overflow.manifest.xml`).
- `frontend-rs/README.md` — operational reference (build, run, troubleshoot).
- This document — strategic reference for the port.

Three commits on top of `main`:

```
ca8efe8 build(spike): Linux→Windows MSVC cross-compile via xwin
f64200b build(spike): drop TLS features from tokio-tungstenite
782fcbf feat(spike): GPUI Rust frontend on existing Go transport
```

When the proper port begins, branch from `gpui-spike` (not main) so the scaffolding is preserved. Phase 1 (live data wiring) is the first meaningful piece of new work.
