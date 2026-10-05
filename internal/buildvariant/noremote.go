//go:build noremote

package buildvariant

// RemoteAccess reports whether this build can enable remote access.
const RemoteAccess = false

// Name is the variant label shown beside the version; empty for the
// standard build.
const Name = "noremote"
