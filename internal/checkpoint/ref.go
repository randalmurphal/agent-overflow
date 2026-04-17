// Package checkpoint captures hidden Git-ref snapshots of a workspace at turn
// boundaries so the UI can diff against prior turns and roll back.
//
// Refs live under refs/agent-overflow/checkpoints/<b64url(threadID)>/turn/<N>.
// Thread IDs are base64url-encoded because UUIDs contain dashes that are legal
// in ref path components, but general thread IDs could be surprising — the
// encoding keeps every character path-safe (parity with forge's mechanism).
package checkpoint

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// RefsPrefix is the namespace under refs/ that we use for checkpoints. Hidden
// from normal `git log`/`git branch` traversals.
const RefsPrefix = "refs/agent-overflow/checkpoints"

// EncodeThreadID returns the base64url-encoded form of the thread ID used in
// ref path components.
func EncodeThreadID(threadID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(threadID))
}

// RefForThreadTurn builds the hidden ref name for (threadID, turnIndex).
func RefForThreadTurn(threadID string, turnIndex int) string {
	return fmt.Sprintf("%s/%s/turn/%d", RefsPrefix, EncodeThreadID(threadID), turnIndex)
}

// ThreadRefPattern returns the `for-each-ref` glob pattern that matches every
// checkpoint ref owned by the given thread.
func ThreadRefPattern(threadID string) string {
	return fmt.Sprintf("%s/%s/**", RefsPrefix, EncodeThreadID(threadID))
}

// ThreadRefPrefix returns the prefix that each of a thread's refs starts with;
// useful for filtering output from commands that don't accept globs.
func ThreadRefPrefix(threadID string) string {
	return fmt.Sprintf("%s/%s/", RefsPrefix, EncodeThreadID(threadID))
}

// IsThreadRef reports whether `ref` belongs to the given thread's checkpoint
// namespace.
func IsThreadRef(ref, threadID string) bool {
	return strings.HasPrefix(ref, ThreadRefPrefix(threadID))
}
