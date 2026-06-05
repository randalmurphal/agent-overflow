// Package gitwatch maintains live git status streams per workspace via
// filesystem watching of both the workspace and git metadata roots, with a
// transparent polling fallback when the OS cannot install watchers.
package gitwatch
