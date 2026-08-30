// Package procrss reads resident-set sizes for an application process and the
// helper processes it owns.
//
// It exists for one question the harness perf run has to answer:
// "how much memory is the WEBVIEW holding". The Go runtime's own heap says
// nothing about it — WebKitGTK runs the renderer in a separate
// `WebKitWebProcess`, and that process is what a Task-Manager-style
// complaint is actually about. gopsutil (internal/sysstat) reports host
// totals, not a process tree, so it cannot answer either.
//
// On Linux the walk reads TWO of the kernel's files per sample and the division is
// deliberate: `stat` (one line) for every pid on the host, because the
// parent map needs them all to find a re-parented renderer, and `status`
// (the expensive, many-line one) only for the processes that turned out
// to be ours. RSS is always VmRSS from `status`.
//
// On macOS the native sampler reads the kernel process table, joins ordinary
// descendants with processes assigned to their macOS responsible process, and
// asks libproc for RSS. The responsibility join is load-bearing: WebKit's XPC
// renderer, networking, and GPU services are reparented to launchd even though
// the OS still assigns them to the application.
package procrss
