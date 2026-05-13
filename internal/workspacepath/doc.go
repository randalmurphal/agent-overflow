// Package workspacepath validates user-supplied workspace-relative
// paths. The single exported helper rejects empty / absolute / parent-
// escaping inputs and returns the OS-cleaned relative form callers can
// safely join under a workspace root.
package workspacepath
