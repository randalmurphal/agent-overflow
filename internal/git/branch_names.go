package git

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

const (
	AutoWorktreeBranchPrefix = "forge"
	autoBranchFallback       = "update"
)

var invalidBranchChars = regexp.MustCompile(`[^a-z0-9/_-]+`)

// BuildTemporaryWorktreeBranchName creates the forge-style temporary branch
// shape used until the first user turn can rename it descriptively.
func BuildTemporaryWorktreeBranchName() string {
	var token [4]byte
	if _, err := rand.Read(token[:]); err != nil {
		return AutoWorktreeBranchPrefix + "/00000000"
	}
	return AutoWorktreeBranchPrefix + "/" + hex.EncodeToString(token[:])
}

// IsTemporaryWorktreeBranch reports whether branch still has the temporary
// forge/<8-hex> placeholder shape.
func IsTemporaryWorktreeBranch(branch string) bool {
	normalized := strings.TrimSpace(strings.ToLower(branch))
	if !strings.HasPrefix(normalized, AutoWorktreeBranchPrefix+"/") {
		return false
	}
	suffix := strings.TrimPrefix(normalized, AutoWorktreeBranchPrefix+"/")
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
// forge/<fragment> form used for descriptive worktree branches.
func BuildGeneratedWorktreeBranchName(raw string) string {
	normalized := strings.TrimSpace(strings.ToLower(raw))
	normalized = strings.TrimPrefix(normalized, "refs/heads/")
	normalized = strings.TrimPrefix(normalized, AutoWorktreeBranchPrefix+"/")
	return AutoWorktreeBranchPrefix + "/" + SanitizeBranchFragment(normalized)
}

// SanitizeBranchFragment collapses arbitrary user-facing text into a safe
// lowercase git branch fragment.
func SanitizeBranchFragment(raw string) string {
	normalized := strings.TrimSpace(strings.ToLower(raw))
	normalized = strings.NewReplacer("'", "", `"`, "", "`", "").Replace(normalized)
	normalized = strings.Trim(normalized, "./ _-\t\r\n")
	normalized = invalidBranchChars.ReplaceAllString(normalized, "-")
	for strings.Contains(normalized, "//") {
		normalized = strings.ReplaceAll(normalized, "//", "/")
	}
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
