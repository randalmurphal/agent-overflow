//go:build !unix

package main

// refuseUnsafeHarnessDir is a no-op off unix. The check it performs is
// about POSIX ownership and mode bits, which Windows ACLs do not map onto
// — and the attack it defends against (a predictable world-writable /tmp
// path planted ahead of the boot) is a shared-multi-user-host shape that
// the Windows topology, where the harness runs inside the user's own
// profile under WSL, does not present in the same way.
func refuseUnsafeHarnessDir(string) error { return nil }
