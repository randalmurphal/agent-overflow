package atomicfile

import "os"

// syncDir is a no-op on Windows. A directory cannot be opened for a flush
// there, and the filesystem exposes no equivalent guarantee to ask for — so
// the honest implementation is the one that does not pretend. Every caller
// that needs the guarantee runs on Linux or macOS: the supervisor is refused
// on Windows outright, where the WSL launcher already supervises the backend.
func syncDir(string) error { return nil }

func syncRootDir(*os.Root, string) error { return nil }
