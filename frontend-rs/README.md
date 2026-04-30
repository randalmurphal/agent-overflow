# frontend-rs — GPUI rewrite spike

GPUI-based Rust frontend that talks to the existing Agent Overflow Go
backend over its HTTP+WebSocket transport. The backend stays 100%
unchanged — this crate is one of multiple possible consumers of the
same wire surface that the Svelte frontend uses.

## Why this exists

Wails+WebView2 is the dominant memory cost in the current app:

| Build | Total tree (RSS / WS) | Private bytes |
|---|---|---|
| Wails default (Win) | ~742 MB | ~700 MB |
| Wails `--single-process` | ~451 MB | ~272 MB |
| GPUI release (Linux/WSL2 idle) | 207 MB | 82 MB |
| **GPUI debug, Windows-native MSVC** | **131 MB** | **119 MB** |

The Svelte frontend itself is lean (4.5 MB JS heap, 239 DOM nodes) — so
nearly all of the prior overhead is Chromium baseline. GPUI replaces
Chromium with a thin Rust+DirectX-renderer process that hosts our actual
view code directly. The Windows-native MSVC build hits the
**<150 MB idle** target with margin and is a 5.7× reduction vs default
Wails / 3.5× vs the most-optimized Wails configuration. Release build
would land another 10–20% lower (debug binary is ~32 MB unoptimized).

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

## Cross-compiling to Windows from Linux

Wired up via `xwin` (Microsoft MSVC SDK reachable from a Linux host).
End-to-end working as of the spike — produces a Windows-native MSVC
debug `.exe` from a Linux host with no Windows-side compiler install.

System deps:

```
sudo apt install -y clang lld
. "$HOME/.cargo/env"
rustup target add x86_64-pc-windows-msvc
ln -sf /usr/bin/clang ~/.cargo/bin/clang-cl     # clang's MSVC driver
ln -sf /usr/lib/llvm-18/bin/llvm-lib ~/.cargo/bin/llvm-lib
cargo install xwin --locked
```

One-time MSVC SDK download (~640 MB, written to
`frontend-rs/.xwin/`, gitignored — each contributor accepts the EULA
themselves):

```
cd frontend-rs
xwin --accept-license splat --output .xwin
```

Then build:

```
cargo build --target x86_64-pc-windows-msvc --bin agent-overflow-rs
```

Output is at `target/x86_64-pc-windows-msvc/debug/agent-overflow-rs.exe`.

Two things `frontend-rs/.cargo/config.toml` does that vanilla `xwin`
docs don't cover:

1. **Static CRT linking** (`-Ctarget-feature=+crt-static`) — embeds
   VCRUNTIME140 so the binary doesn't depend on the user's installed
   VC++ Redistributable version.
2. **Manifest-via-link-arg** — see `frontend-rs/build.rs`. GPUI 0.2.2's
   build.rs only embeds its Common-Controls-v6 manifest when the host
   is Windows (`#[cfg(target_os = "windows")]`), so cross-compiles from
   Linux skip it. Without that manifest, `comctl32.dll` v5 is loaded
   instead of v6 and `TaskDialogIndirect` is missing →
   `STATUS_ENTRYPOINT_NOT_FOUND` at startup. We pass `/manifest:embed`
   + our own copy of the manifest to lld-link to fix this.

Note: `frontend-rs/.cargo/config.toml` uses absolute paths because
cc-rs runs build scripts in each crate's source dir, not the workspace
root. The committed paths reference `/home/rmurphy/...` — non-rmurphy
contributors edit the `-Lnative=` paths and the `CFLAGS_/CXXFLAGS_`
include paths to point at their own `.xwin/`. (Could be templated via
an env var indirection later; not urgent.)

### Debug build runtime caveat (cross-compiled only)

GPUI 0.2.2's debug-build shader-compile path uses `D3DCompileFromFile`
with a path baked at compile time via `env!("CARGO_MANIFEST_DIR")`. On a
Linux→Windows cross-compile, that bakes in the Linux registry path
(`/home/rmurphy/.cargo/registry/.../gpui-0.2.2/src/platform/windows/`).
At runtime on Windows, `canonicalize()` interprets that as
`C:\home\rmurphy\.cargo\...` and fails with `ERROR_PATH_NOT_FOUND`.

Workaround until release builds work or upstream switches to in-memory
`D3DCompile`: mirror the gpui shaders to that exact path on Windows:

```
mkdir -p "/mnt/c/home/rmurphy/.cargo/registry/src/index.crates.io-1949cf8c6b5b557f/gpui-0.2.2/src/platform/windows"
cp ~/.cargo/registry/src/index.crates.io-1949cf8c6b5b557f/gpui-0.2.2/src/platform/windows/*.hlsl \
   "/mnt/c/home/rmurphy/.cargo/registry/src/index.crates.io-1949cf8c6b5b557f/gpui-0.2.2/src/platform/windows/"
```

A release build avoids this entirely (shaders are pre-compiled into
`OUT_DIR/shaders_bytes.rs` and `include!`'d at compile time), but
release requires `fxc.exe` from the Windows SDK — currently not
available on the Linux side. Solving that with `dxc` (Microsoft's
open-source HLSL compiler that runs on Linux) is the natural follow-up
when release-build measurements are needed.

## Known issues

- **WSLg + Wayland**: GPUI 0.2.2's wayland_client binding panics on
  WSLg's compositor (`UnsupportedVersion` at `wayland/client.rs:151`).
  Workaround: unset `WAYLAND_DISPLAY` to force the X11 fallback.
- **llvmpipe software renderer (WSL only)**: WSLg lacks hardware
  Vulkan; we end up on llvmpipe. Performance is fine for a chat UI but
  real GPU on a Windows host will be cheaper still. Has no impact on
  memory beyond the indirect cost of CPU-side render buffers.
- **Debug build vs release** (Linux): Debug pulls 182 MB RSS, release
  146 MB. Cross-compile to Windows currently produces only debug
  binaries (release blocked on `fxc.exe` for shader pre-compile).
- **mingw-gnu Rust target on Windows is unsupported by GPUI 0.2.2**:
  attempting `x86_64-pc-windows-gnu` produces a binary that crashes at
  startup with `STATUS_ACCESS_VIOLATION` — GPUI's Windows code paths
  are tested only on the MSVC ABI. Use `x86_64-pc-windows-msvc` (via
  the xwin setup above).

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
