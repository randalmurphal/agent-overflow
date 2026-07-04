//go:build windows

// payload_validate.go runs at process init() on the Windows launcher
// to surface a loud warning when the embedded Linux payload is the
// 68-byte placeholder shipped in the repo (vs. a real Linux ELF
// produced by `task -d build/windows build:wsl`). Without this, a
// developer running `go build ./cmd/agent-overflow-windows` followed
// by an actual launch sees a vague "exec format error" from inside
// WSL with no UI feedback.
//
// We deliberately do NOT os.Exit() — placeholder builds are useful
// for iterating on the picker UI and the launch pipeline without
// rebuilding the Linux side. The warning prints to stderr (which the
// launcher mirrors to %APPDATA%\agent-overflow\launcher.log) so a
// developer running the .exe from a console sees it immediately.
package main

import (
	"fmt"
	"os"
)

// minPayloadBytes is the rough size threshold below which the
// payload is almost certainly the placeholder. A real release build
// of agent-overflow on Linux is ~30 MiB; 1 MiB is conservative for
// "this isn't a stub".
const minPayloadBytes = 1 * 1024 * 1024

// elfMagic is the byte signature of an ELF executable. Real Linux
// agent-overflow binaries start with this. The placeholder is plain
// ASCII so the magic check fails loudly.
const elfMagic = "\x7fELF"

func init() {
	if len(linuxPayload) < minPayloadBytes {
		fmt.Fprintln(os.Stderr,
			"agent-overflow.exe: WARNING — embedded Linux payload is < 1 MiB; "+
				"this is the placeholder. Run `task -d build/windows build:wsl` "+
				"before shipping; WSL will fail with `exec format error` at install.")
	}
	if len(linuxPayload) >= len(elfMagic) && !hasELFMagic(linuxPayload) {
		fmt.Fprintln(os.Stderr,
			"agent-overflow.exe: WARNING — embedded payload does not start with "+
				"ELF magic; install will succeed but `wsl.exe -- agent-overflow` "+
				"will fail with `exec format error`.")
	}
}

func hasELFMagic(payload string) bool {
	return len(payload) >= len(elfMagic) && payload[:len(elfMagic)] == elfMagic
}
