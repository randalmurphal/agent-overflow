package main

import "syscall"

// maxRSSBytes is the largest resident set of a process and the children it
// waited for; Linux counts it in KiB.
func maxRSSBytes(usage *syscall.Rusage) uint64 { return uint64(usage.Maxrss) << 10 }
