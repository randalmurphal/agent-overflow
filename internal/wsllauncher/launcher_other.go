//go:build !windows

package wsllauncher

import (
	"context"
	"fmt"
	"os/exec"
)

// ListDistros returns an empty slice and nil error on non-Windows hosts
// so the package's parser tests can still run on macOS / Linux. The
// cross-platform contract is documented in launcher.go.
//
// Why empty + nil rather than the not-supported error: the picker UI
// flow on Windows treats a zero-length distro list as "WSL not
// installed" and shows install instructions. Mirroring that behaviour
// off-platform keeps integration tests and the Windows binary's
// fallback path on the same code branch.
func ListDistros(ctx context.Context) ([]Distro, error) {
	_ = ctx
	return nil, nil
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
