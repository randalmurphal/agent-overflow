# frontend-rs — GPUI rewrite spike

GPUI-based Rust frontend that talks to the existing Agent Overflow Go
backend over its HTTP+WebSocket transport. The backend stays 100%
unchanged — this crate is one of multiple possible consumers of the
same wire surface that the Svelte frontend uses.

## Why this exists

Wails+WebView2 is the dominant memory cost in the current app:

| Build | Total tree RSS | Private bytes |
|---|---|---|
| Wails default (Win) | ~742 MB | ~700 MB |
| Wails `--single-process` | ~451 MB | ~272 MB |
| **GPUI release (Linux/WSL2 idle)** | **207 MB** | **82 MB** |

The Svelte frontend itself is lean (4.5 MB JS heap, 239 DOM nodes) — so
nearly all of the prior overhead is Chromium baseline. GPUI replaces
Chromium with a thin Rust+Blade-renderer process that hosts our actual
view code directly.

## What works in the spike

- Bootstrap: spawns `bin/agent-overflow --print-url-fd 0` headless,
  parses the `__AO_BOOTSTRAP__:` stdout sentinel.
- Transport client: WebSocket + JSON frames matching `internal/transport/frame.go`,
  with reconnect backoff and replay-on-reconnect. Method IDs are FNV-1a
  32-bit of `main.App.<MethodName>` — verified against pinned vectors
  from the Go dispatcher's `TestFnvHash_MatchesWails`.
- Cold-start RPCs: `ListProjects`, `ListThreads`, `ListRecentThreadItems`.
- Auto-select first thread, render its timeline. Click a thread row in
  the sidebar to switch.
- Status bar with live transport state.

## What's deliberately not in the spike

- Event channel subscriptions: `subscribe()` exists on the Transport
  but no view subscribes yet. Live timeline updates and provider
  status pushes are the next layer.
- Composer + send. The view is read-only.
- Markdown rendering, syntax highlighting, diff viewer. Items render
  the backend `summary` string verbatim.
- Settings panel, project picker, projects sidebar header.
- Approvals, design options, plan revision, terminal panel.

These are work, not unknowns — the wire is proven and the GPUI surface
mirrors the Svelte component tree more or less directly. The scope here
is "validate the path"; full UI parity comes after the path is chosen.

## Layout

```
src/
├── lib.rs            module root (so examples/tests can import)
├── main.rs           GPUI entry: tokio runtime + Application::new().run(...)
├── theme.rs          color tokens
├── app.rs            AppState + UiUpdate bridge between tokio and GPUI
├── transport/
│   ├── mod.rs
│   ├── wire.rs       Frame serde definitions
│   ├── fnv.rs        FNV-1a 32-bit method-id hash (pinned-vector test)
│   ├── bootstrap.rs  spawn backend, parse stdout sentinel
│   └── client.rs     WS supervisor: connect, RPC tracking, fanout
├── rpc/
│   └── mod.rs        Typed wrappers over call_id(method_id, params)
├── models/           Domain types — mirror Go store/models.ts shapes
└── ui/
    ├── mod.rs
    ├── root.rs       top layout (sidebar | main | status)
    ├── sidebar.rs    thread list with click-to-select
    ├── timeline.rs   item rows by lane (user/assistant/tool/system)
    └── status_bar.rs transport status pill + counters
```

## Threading model

Two runtimes coexist:

- **Tokio** runs the transport, bootstrap subprocess, RPC futures, and
  the WS supervisor. It owns the `Transport` clone-and-hold handle.
- **GPUI** runs the foreground app: window, layout, render. Entities
  (`Entity<*Model>` on `AppState`) are mutated only on the foreground.

The bridge is one-way `mpsc::UnboundedSender<UiUpdate>` from tokio
into GPUI. A `cx.spawn(...)` task drains the queue and applies each
message to the right entity. UI clicks reach back into tokio via a
plain `Handle::spawn` call — those tasks return their result through
the same `UiUpdate` channel.

`AsyncApp` deliberately never crosses thread boundaries — GPUI's
foreground context is `!Send` by design.

## Running

System deps (one-time, on Debian/Ubuntu/WSL2):

```
sudo apt install -y \
  build-essential pkg-config libfontconfig-dev libssl-dev \
  libwayland-dev libxkbcommon-dev libxkbcommon-x11-dev \
  libxcb-render0-dev libxcb-shape0-dev libxcb-xfixes0-dev \
  libxcb-cursor-dev libvulkan-dev mold
```

Build the Go backend (only needed once or after Go-side changes):

```
cd /home/rmurphy/repos/agent-overflow-gpui
cp -r ../agent-overflow/frontend/dist frontend/dist  # or build it
go build -o ./bin/agent-overflow .
```

Build + run the GPUI client:

```
cd frontend-rs
cargo build --release --bin agent-overflow-rs

# WSLg's Wayland version trips GPUI 0.2.2 today — force X11:
AGENT_OVERFLOW_BIN=../bin/agent-overflow WAYLAND_DISPLAY= \
  ./target/release/agent-overflow-rs
```

The client spawns the backend automatically. To attach to a separately-
running backend:

```
AGENT_OVERFLOW_WS_URL=ws://127.0.0.1:NNNN/ws \
  AGENT_OVERFLOW_TOKEN=... \
  ./target/release/agent-overflow-rs
```

## Smoke test (no GUI)

Pure-transport integration test — useful when iterating on the wire:

```
cd frontend-rs
AGENT_OVERFLOW_BIN=../bin/agent-overflow \
  cargo run --example smoke
```

Expected output: connect in <200 ms, list projects + threads, dump the
first few items of the most recent thread.

## Unit tests

```
cargo test --bin agent-overflow-rs
```

Of note: `transport::fnv::tests::matches_wails_pinned_vectors` proves
our hash matches the Go dispatcher's `fnvHash` and the Wails internal
`hash.Fnv` byte-for-byte. If it ever fails, every RPC stops routing.

## Memory measurement

Linux measurements (no PowerShell screen captures):

```
pid=$(pgrep -f /target/release/agent-overflow-rs)
grep -E "^Vm(RSS|HWM)|^Rss" /proc/$pid/status
grep -E "^(Rss|Pss|Private_(Dirty|Clean)|Anonymous):" /proc/$pid/smaps_rollup
```

`Pss` is the most accurate "real cost" — divides shared pages by their
sharer count. `Private_Dirty` is the truly exclusive memory (heap +
modified data). Rough idle baseline: 146 MB RSS, 65 MB private-dirty
for the GUI; +60 MB RSS / +16 MB private-dirty for the Go backend.

## Cross-compiling to Windows

Not done in the spike. GPUI 0.2.2 supports `windows-manifest` as a
default feature; the Blade renderer uses Vulkan via DXGI on Windows.
Path: `cargo build --release --target x86_64-pc-windows-gnu` after
installing the toolchain. Cross-tree memory comparisons need to land
on Windows for an apples-to-apples vs Wails.

## Known issues

- **WSLg + Wayland**: GPUI 0.2.2's wayland_client binding panics on
  WSLg's compositor (`UnsupportedVersion` at `wayland/client.rs:151`).
  Workaround: unset `WAYLAND_DISPLAY` to force the X11 fallback.
- **llvmpipe software renderer**: WSLg lacks hardware Vulkan; we end
  up on llvmpipe. Performance is fine for a chat UI but real GPU on a
  Windows host will be cheaper still. Has no impact on memory beyond
  the indirect cost of CPU-side render buffers.
- **Debug build vs release**: Debug pulls 182 MB RSS (no opt, full
  symbols mapped). Release strips debuginfo and lands at 146 MB.

## Comparison to existing reference apps

GPUI in production by other teams:

- **Zed** itself (zed-industries/zed) — reference for general patterns.
  Look at `crates/agent`, `crates/agent_ui`, `crates/acp_thread` for
  agentic chat UI shapes.
- **Arbor** — agentic coding workflows in a fully-native desktop GPUI
  app. The spec match for this product.
- **hunk** — Codex orchestrator built on GPUI. Smaller scope than
  Arbor, also a useful reference for the "talk to a CLI agent and
  render its events" pattern.
- **gpui-component** (longbridge) — ready-made shadcn-style component
  library on top of GPUI. Not used in the spike but added as a dep so
  it compiles in the dependency graph; switching individual primitives
  to its components is a single-line change at adoption time.
