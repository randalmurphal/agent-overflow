// Cross-compile-friendly manifest embedding for the Windows MSVC target.
//
// GPUI 0.2.2 has its own manifest (resources/windows/gpui.manifest.xml)
// containing the Common Controls v6 dependency that `TaskDialogIndirect`
// + DPI-aware behaviour require. Its build.rs only runs the embed step
// on a Windows build host (`#[cfg(target_os = "windows")]`), so a
// Linux→Windows cross-compile leaves the binary without it.
//
// Without the v6 manifest, the loader binds `comctl32.dll` v5.x which
// lacks `TaskDialogIndirect`, and the binary panics with
// `STATUS_ENTRYPOINT_NOT_FOUND` before main() runs.
//
// We sidestep that by handing lld-link the manifest directly via the
// /manifest:embed + /manifestinput:<file> options. Both work on the
// Linux side because lld-link is the linker we already use for the
// MSVC target.

use std::env;
use std::path::PathBuf;

fn main() {
    let target_os = env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    let target_env = env::var("CARGO_CFG_TARGET_ENV").unwrap_or_default();
    if target_os != "windows" || target_env != "msvc" {
        return;
    }

    let manifest_path: PathBuf = PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap())
        .join("resources/windows/agent-overflow.manifest.xml");
    println!("cargo:rerun-if-changed={}", manifest_path.display());

    println!("cargo:rustc-link-arg=/manifest:embed");
    println!(
        "cargo:rustc-link-arg=/manifestinput:{}",
        manifest_path.display()
    );
}
