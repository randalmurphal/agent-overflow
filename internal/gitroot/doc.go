// Package gitroot resolves a filesystem path to the MAIN git repository root
// it belongs to, and enumerates a repository's registered worktrees, by
// reading git's own on-disk layout — never by spawning git.
package gitroot
