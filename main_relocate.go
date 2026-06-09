package main

import (
	"log"
	"os"
	"strings"

	"agent-overflow/internal/platform"
)

// relocateOffWindowsDriveMount moves the WSL backend's working directory
// off a Windows drive mount when the Windows launcher handed it one.
//
// `wsl.exe -d <distro> -- <bin>` defaults the Linux process's cwd to the
// *translated* Windows cwd. For the production launcher that's the .exe's
// install dir under /mnt/c — a 9p (drvfs) mount with slow, uncached I/O.
// Left uncorrected, the backend and every child it spawns without an
// explicit cwd (the shell-env PATH probe, provider MCP-status probes such
// as `claude mcp list`) run rooted on that mount. A git child there trips
// WSL's "git on a Windows drive" performance notification even though the
// user's repositories all live on the Linux filesystem.
//
// Best-effort and self-correcting: it runs once at startup, before any
// subsystem spawns a child, and degrades to leaving the cwd in place (with
// a log line) if the home directory can't be resolved. No-op off WSL and
// whenever the cwd is already Linux-native — native Linux, macOS, `make
// dev`, and the `--cd`-pinned launcher path all start on a Linux cwd and
// skip the chdir.
func relocateOffWindowsDriveMount() {
	relocateCwd(platform.IsWSL(), os.Getwd, os.UserHomeDir, os.Chdir)
}

// relocateCwd is the OS-injected core of relocateOffWindowsDriveMount, kept
// separate so the WSL gate, the mount check, and both degrade paths are
// unit-testable without invoking the process-global os.Chdir.
func relocateCwd(
	isWSL bool,
	getwd func() (string, error),
	userHomeDir func() (string, error),
	chdir func(string) error,
) {
	if !isWSL {
		return
	}
	cwd, err := getwd()
	if err != nil {
		log.Printf("relocate: cannot read working directory to check for a Windows drive mount: %v", err)
		return
	}
	if !isUnderWindowsDriveMount(cwd) {
		return
	}
	home, err := userHomeDir()
	if err != nil || home == "" {
		log.Printf("relocate: working directory %q is on a Windows drive mount, but the home directory is unavailable (%v); leaving it in place", cwd, err)
		return
	}
	if err := chdir(home); err != nil {
		log.Printf("relocate: failed to move working directory off Windows drive mount %q to %q: %v", cwd, home, err)
		return
	}
	log.Printf("relocate: moved working directory from %q (Windows drive mount) to home %q to avoid slow 9p git I/O", cwd, home)
}

// isUnderWindowsDriveMount reports whether p sits on a WSL drvfs automount
// of a Windows drive — i.e. /mnt/<letter> (such as /mnt/c). WSL2 mounts
// Windows drives there over the 9p protocol, where git and other stat-heavy
// I/O is slow and uncached.
//
// The first path segment after /mnt/ must be a single ASCII letter: that's
// what distinguishes a real drive mount (/mnt/c, /mnt/d) from WSL's own
// Linux-backed tmpfs mounts under the same root (/mnt/wsl, /mnt/wslg),
// which must NOT be relocated away from.
func isUnderWindowsDriveMount(p string) bool {
	rest, ok := strings.CutPrefix(p, "/mnt/")
	if !ok || rest == "" {
		return false
	}
	segment := rest
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		segment = rest[:slash]
	}
	if len(segment) != 1 {
		return false
	}
	c := segment[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
