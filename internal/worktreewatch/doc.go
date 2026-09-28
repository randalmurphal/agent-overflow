// Package worktreewatch watches every project's git worktree registry
// (`<commonDir>/worktrees`) and reports when a linked worktree is added or
// removed by any process, or when a registered worktree's directory
// disappears from disk. It reads the registry through gitroot, never by
// spawning git, and owns its filesystem watches, debounce, polling fallback
// and callback goroutines.
//
// Each report names the worktrees that vanished since the previous read,
// diffed against the last read the manager keeps per watched project. The
// consumer decides what a change means; the app reattaches the threads of a
// worktree that no longer exists to the project root and tells every client
// which worktree went.
package worktreewatch
