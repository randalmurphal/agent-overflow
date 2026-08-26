// Package procrss reads resident-set sizes for a process and the webview
// children it spawned, straight out of a /proc-shaped filesystem.
//
// It exists for one question the harness perf run has to answer on linux:
// "how much memory is the WEBVIEW holding". The Go runtime's own heap says
// nothing about it — WebKitGTK runs the renderer in a separate
// `WebKitWebProcess`, and that process is what a Task-Manager-style
// complaint is actually about. gopsutil (internal/sysstat) reports host
// totals, not a process tree, so it cannot answer either.
//
// The walk reads TWO of the kernel's files per sample and the division is
// deliberate: `stat` (one line) for every pid on the host, because the
// parent map needs them all to find a re-parented renderer, and `status`
// (the expensive, many-line one) only for the processes that turned out
// to be ours. RSS is always VmRSS from `status`.
//
// The walk is a plain filesystem read of a directory tree, so the whole of
// it is exercised against canned testdata; only the exported entry point
// carries a build split, because /proc exists on linux alone.
package procrss
