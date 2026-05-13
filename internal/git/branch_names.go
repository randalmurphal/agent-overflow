package git

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

const (
	AutoWorktreeBranchPrefix = "ao-"
	autoBranchFallback       = "update"
)

var invalidBranchChars = regexp.MustCompile(`[^a-z0-9_-]+`)
var invalidBranchNameChars = regexp.MustCompile(`[^A-Za-z0-9_/-]+`)

// BuildTemporaryWorktreeBranchName creates the default temporary branch
// shape used until the first user turn can rename it descriptively.
func BuildTemporaryWorktreeBranchName() string {
	return BuildTemporaryWorktreeBranchNameWithPrefix(AutoWorktreeBranchPrefix)
}

// BuildTemporaryWorktreeBranchNameWithPrefix creates a flat, prefixed
// temporary branch name.
func BuildTemporaryWorktreeBranchNameWithPrefix(prefix string) string {
	prefix = normalizeWorktreeBranchPrefix(prefix)
	var token [4]byte
	if _, err := rand.Read(token[:]); err != nil {
		return prefix + "00000000"
	}
	return prefix + hex.EncodeToString(token[:])
}

// IsTemporaryWorktreeBranch reports whether branch still has the temporary
// default placeholder shape.
func IsTemporaryWorktreeBranch(branch string) bool {
	return IsTemporaryWorktreeBranchWithPrefix(branch, AutoWorktreeBranchPrefix)
}

// IsTemporaryWorktreeBranchWithPrefix reports whether branch is a temporary
// prefixed placeholder branch.
func IsTemporaryWorktreeBranchWithPrefix(branch, prefix string) bool {
	prefix = strings.ToLower(normalizeWorktreeBranchPrefix(prefix))
	normalized := strings.TrimSpace(strings.ToLower(branch))
	if !strings.HasPrefix(normalized, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(normalized, prefix)
	if len(suffix) != 8 {
		return false
	}
	for _, r := range suffix {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// BuildGeneratedWorktreeBranchName normalizes a raw branch fragment into the
// default prefixed form used for descriptive worktree branches.
func BuildGeneratedWorktreeBranchName(raw string) string {
	return BuildGeneratedWorktreeBranchNameWithPrefix(raw, AutoWorktreeBranchPrefix)
}

// BuildGeneratedWorktreeBranchNameWithPrefix normalizes a raw branch fragment
// into the configured prefixed form used for descriptive worktree branches.
func BuildGeneratedWorktreeBranchNameWithPrefix(raw, prefix string) string {
	prefix = normalizeWorktreeBranchPrefix(prefix)
	normalized := strings.TrimSpace(strings.ToLower(raw))
	normalized = strings.TrimPrefix(normalized, "refs/heads/")
	normalized = strings.TrimPrefix(normalized, strings.ToLower(prefix))
	return prefix + SanitizeBranchFragment(normalized)
}

// BranchFragmentFromUserMessage extracts a branch-fragment-ready short
// title from the first user message of a worktree thread. The returned
// string is the raw input to `BuildGeneratedWorktreeBranchName*` — it
// has been line-clipped to the first line, sentence-clipped to the
// first sentence-ending punctuation, whitespace-collapsed, and falls
// back to `autoBranchFallback` ("update") on empty input. Further
// sanitation (lowercase, forbidden chars, length cap, prefix wrap) is
// the caller's job via `BuildGeneratedWorktreeBranchName*`.
func BranchFragmentFromUserMessage(content string) string {
	title := strings.TrimSpace(content)
	if title == "" {
		return autoBranchFallback
	}
	if line, _, ok := strings.Cut(title, "\n"); ok {
		title = line
	}
	title = firstSentenceFromMessage(title)
	title = strings.Join(strings.Fields(title), " ")
	title = strings.TrimSpace(title)
	if title == "" {
		return autoBranchFallback
	}
	return title
}

// firstSentenceFromMessage returns the prefix of content up to and
// including the first `.`, `!`, or `?`. With no terminator, the full
// content is returned unchanged. Used by BranchFragmentFromUserMessage
// to keep branch fragments short even when the user message rambles.
func firstSentenceFromMessage(content string) string {
	for i, r := range content {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		return strings.TrimSpace(content[:i+1])
	}
	return content
}

// SanitizeBranchFragment collapses arbitrary user-facing text into a safe
// lowercase git branch fragment.
func SanitizeBranchFragment(raw string) string {
	normalized := strings.TrimSpace(strings.ToLower(raw))
	normalized = strings.NewReplacer("'", "", `"`, "", "`", "").Replace(normalized)
	normalized = strings.Trim(normalized, "./ _-\t\r\n")
	normalized = invalidBranchChars.ReplaceAllString(normalized, "-")
	for strings.Contains(normalized, "--") {
		normalized = strings.ReplaceAll(normalized, "--", "-")
	}
	normalized = strings.Trim(normalized, "./_-")
	if len(normalized) > 64 {
		normalized = strings.TrimRight(normalized[:64], "./_-")
	}
	if normalized == "" {
		return autoBranchFallback
	}
	return normalized
}

// SanitizeBranchNamePreservingSlashes collapses user-entered branch names into
// a git-friendly branch while preserving path separators and letter case. It is
// intended for explicit branch-name input, not generated names derived from
// prompts.
func SanitizeBranchNamePreservingSlashes(raw string) string {
	normalized := strings.TrimSpace(raw)
	normalized = strings.TrimPrefix(normalized, "refs/heads/")
	normalized = strings.NewReplacer("'", "", `"`, "", "`", "").Replace(normalized)
	normalized = strings.Trim(normalized, "./ _-\t\r\n")
	normalized = invalidBranchNameChars.ReplaceAllString(normalized, "-")
	for strings.Contains(normalized, "//") {
		normalized = strings.ReplaceAll(normalized, "//", "/")
	}
	for strings.Contains(normalized, "--") {
		normalized = strings.ReplaceAll(normalized, "--", "-")
	}
	parts := strings.Split(normalized, "/")
	kept := parts[:0]
	for _, part := range parts {
		part = strings.Trim(part, ".-_")
		if part != "" {
			kept = append(kept, part)
		}
	}
	normalized = strings.Join(kept, "/")
	if len(normalized) > 96 {
		normalized = strings.TrimRight(normalized[:96], "./_-")
	}
	if normalized == "" {
		return autoBranchFallback
	}
	return normalized
}

func normalizeWorktreeBranchPrefix(prefix string) string {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		return AutoWorktreeBranchPrefix
	}
	return strings.ToLower(trimmed)
}
