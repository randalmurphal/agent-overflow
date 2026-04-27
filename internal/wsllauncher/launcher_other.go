//go:build !windows

package wsllauncher

import (
	"context"
	"fmt"
	"os/exec"

	"agent-overflow/internal/editor"
)

// isWSL is a package-level seam so tests can inject WSL membership
// without depending on the host's /proc/sys/kernel/osrelease (which
// editor.IsWSL caches via sync.Once and so cannot be reset between
// test cases).
var isWSL = editor.IsWSL

// ListDistros returns the user's WSL distros when called from inside a
// WSL distribution (the agent-overflow Linux backend the Windows
// launcher just spawned), and an empty slice + nil error otherwise.
//
// Detection reads /proc/sys/kernel/osrelease through editor.IsWSL.
// That's the same source the Settings UI uses to decide whether to
// render the WSL distro switcher — keeping the two in sync prevents a
// drift where the UI is visible but the binding returns nothing
// because of a divergent detection signal.
//
// Why empty + nil rather than ErrNotSupported off-WSL: the picker UI
// flow on Windows treats a zero-length distro list as "WSL not
// installed" and shows install instructions. Mirroring that behaviour
// off-platform keeps integration tests and the Windows binary's
// fallback path on the same code branch.
func ListDistros(ctx context.Context) ([]Distro, error) {
	if !isWSL() {
		return nil, nil
	}
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		// Inside WSL but wsl.exe interop isn't on PATH (some users
		// disable interop in /etc/wsl.conf). Treat as "no distros to
		// list" rather than erroring — the Settings UI hides the
		// switcher in that case.
		return nil, nil
	}

	listCtx, cancel := context.WithTimeout(ctx, listDistrosTimeout)
	defer cancel()

	cmd := exec.CommandContext(listCtx, "wsl.exe", "-l", "-v")
	// No SysProcAttr.HideWindow here — that's a Windows-only field
	// for hiding console flashes when wsl.exe is spawned from a GUI
	// process. The WSL-side caller is a backend Linux process; there
	// is no Windows console to flash.
	return runListDistrosCmd(cmd)
}

// Launch errors out on non-Windows hosts. The Linux/macOS desktop
// build never spawns a WSL backend — it runs the Linux binary
// directly via main.go's existing path.
func Launch(ctx context.Context, opts LaunchOptions) (*Launcher, *Bootstrap, error) {
	_ = ctx
	_ = opts
	return nil, nil, fmt.Errorf("wsllauncher: %w", ErrNotSupported)
}

// InstallPayload errors out on non-Windows hosts. Bundling the Linux
// payload only matters when the host is Windows and we need to drop
// the binary into the WSL filesystem.
func InstallPayload(ctx context.Context, distro, hostPath, wslPath string) error {
	_ = ctx
	_ = distro
	_ = hostPath
	_ = wslPath
	return fmt.Errorf("wsllauncher: %w", ErrNotSupported)
}

// stubPlatformLauncher satisfies the platformLauncher interface on
// non-Windows hosts so launcher_test.go can build a Launcher value.
// adopt is never called on these hosts in production; it returns nil
// for tests that go through the launch path with a stub command.
type stubPlatformLauncher struct{}

func (stubPlatformLauncher) adopt(*exec.Cmd) error { return nil }
func (stubPlatformLauncher) close() error          { return nil }

// newPlatformLauncher returns a no-op platformLauncher. The Windows
// build supplies the real Job Object implementation.
func newPlatformLauncher() (platformLauncher, error) {
	return stubPlatformLauncher{}, nil
}
