// Package hostmemory reads, on Linux, how much memory this process may
// still use: the host's MemAvailable and MemTotal, and the memory limit and
// headroom of its cgroup and every cgroup above it. It is the one reader of
// those numbers: the memory governor's capacity and the test namespace's
// RAM temp directory admission both use it. Standard library only.
package hostmemory
