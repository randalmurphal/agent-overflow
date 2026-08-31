// Package threadapp owns store-backed thread application policy and the
// process-local keyed lock registry that serializes thread actions.
//
// `internal/app` retains the stable Wails methods and explicit adapters for
// provider sessions, git subprocesses, worktree setup, settings, and event
// projection.
package threadapp
