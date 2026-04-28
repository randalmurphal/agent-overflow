//go:build windows

package shellenv

import "context"

// doSync is the Windows stub: the cmd/agent-overflow-windows launcher
// never spawns provider children — providers run inside the WSL Linux
// backend (root main.go cross-compiled to linux/amd64), where doSync
// runs through shellenv_unix.go. There's nothing to probe on the
// Windows side.
func doSync(_ context.Context) error { return nil }
