// Package prthread owns pure formatting helpers for pull/merge request
// text: the forge's own noun for a change request and a code fence that
// inner backtick runs cannot close.
package prthread

import "strings"

// ForgeNoun returns the short noun ("PR" / "MR") for the given forge
// id. Unknown / empty forge ids fall back to "PR" — the same fallback
// the frontend `forgeLabels` helper uses — so user-visible error
// strings stay readable when classification is incomplete.
func ForgeNoun(forgeID string) string {
	if forgeID == "gitlab" {
		return "MR"
	}
	return "PR"
}

// ForgeNounLong returns the long-form noun ("pull request" / "merge
// request") for the given forge id. Same fallback rule as ForgeNoun.
func ForgeNounLong(forgeID string) string {
	if forgeID == "gitlab" {
		return "merge request"
	}
	return "pull request"
}

// FenceForContent returns a backtick fence long enough to avoid
// colliding with any backtick run inside content. Standard markdown
// requires the closing fence to be at least as long as the opening
// one, and a content run that matches the fence will close it
// prematurely. We pick a fence strictly longer than the longest run
// we find (minimum 3 = standard triple-backtick) so the diff survives
// verbatim.
func FenceForContent(content string) string {
	longest := 0
	run := 0
	for _, r := range content {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	size := longest + 1
	if size < 3 {
		size = 3
	}
	return strings.Repeat("`", size)
}
