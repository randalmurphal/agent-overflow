//go:build nogui

// main_nogui.go provides the runDesktop / runClient stubs the headless
// WSL backend payload links against. The payload is invoked exclusively
// by the Windows launcher with `--print-url-fd 0`, which routes through
// runHeadless — these stubs only fire if someone runs the WSL binary
// directly without that flag, in which case the right behaviour is to
// fail with a clear message rather than silently exit or attempt to
// open a window that cannot exist (the binary is built without the
// Wails application package, so there is nothing to attach a window
// to).
//
// The matching desktop implementations live in main_desktop.go behind
// the inverse `!nogui` tag.
package main

func runClient(_ string) {
	fatalf("--connect mode is not available in this build; the WSL backend payload only runs headless. Launch the desktop binary on your host OS instead.")
}

func runDesktop(_ string) {
	fatalf("desktop mode is not available in this build; the WSL backend payload only runs with --print-url-fd to publish its bootstrap line. Use the Windows launcher (agent-overflow.exe) to drive it.")
}
