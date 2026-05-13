// Package prthread owns the pure formatting helpers behind
// `CreateThreadFromPR`: rendering the sidebar title, building the first
// user message, picking a code fence that won't be closed by inner
// backtick runs, and truncating oversized titles / diffs on rune
// boundaries.
//
// The App-coupled glue — forge CLI invocation, project resolution,
// store reads/writes — stays in `app_thread_from_pr.go`.
package prthread

import (
	"fmt"
	"strings"
	"unicode/utf8"

	gitops "agent-overflow/internal/git"
)

// MaxInlinedDiffBytes caps the number of patch bytes we inline into the
// first user message on a PR-seeded thread. Oversized PRs (vendored dep
// bumps, generated lockfile churn) used to be inlined verbatim and
// cause SQLite row or frontend render explosions. Beyond this
// threshold we truncate and append a marker so the agent sees explicit
// evidence of the omission instead of a silently-cut patch.
const MaxInlinedDiffBytes = 256 * 1024

// MaxTitleRunes caps thread titles at 120 user-perceived characters
// (runes). The SQLite column is wide, but the sidebar row truncates
// anything that doesn't fit — a 120-rune ceiling keeps the title
// readable while leaving room for the "PR #N: " prefix in the common
// case.
const MaxTitleRunes = 120

// FormatTitle renders the sidebar title prefix per forge: "PR #N" for
// GitHub, "MR !N" for GitLab — matching each forge's native conventions
// for referencing change requests.
func FormatTitle(forgeID string, number int, prTitle string) string {
	prTitle = strings.TrimSpace(prTitle)
	if forgeID == "gitlab" {
		return fmt.Sprintf("MR !%d: %s", number, prTitle)
	}
	return fmt.Sprintf("PR #%d: %s", number, prTitle)
}

// BuildUserMessage composes the first user message persisted on the
// new thread. Keeps the PR title + author + bodies compact, then
// dumps the patch into a fenced code block so providers can reason
// about the actual changes.
func BuildUserMessage(ref gitops.PRReference, meta gitops.PRMetadata, diff string) string {
	var b strings.Builder
	header := "PR"
	numberSigil := "#"
	if ref.Forge == "gitlab" {
		header = "MR"
		numberSigil = "!"
	}
	fmt.Fprintf(&b, "# %s %s%d: %s\n\n", header, numberSigil, ref.Number, strings.TrimSpace(meta.Title))
	if meta.URL != "" {
		fmt.Fprintf(&b, "Link: %s\n", meta.URL)
	}
	if meta.AuthorLogin != "" {
		fmt.Fprintf(&b, "Author: @%s\n", meta.AuthorLogin)
	}
	if meta.BaseRefName != "" || meta.HeadRefName != "" {
		fmt.Fprintf(&b, "Branches: %s → %s\n", meta.HeadRefName, meta.BaseRefName)
	}
	if len(meta.Files) > 0 {
		fmt.Fprintf(&b, "Files changed: %d\n", len(meta.Files))
	}
	b.WriteString("\n")
	body := strings.TrimSpace(meta.Body)
	if body != "" {
		b.WriteString("## Description\n\n")
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	truncated := strings.TrimRight(TruncateDiff(diff), "\n")
	fence := FenceForContent(truncated)
	b.WriteString("## Patch\n\n")
	b.WriteString(fence)
	b.WriteString("diff\n")
	b.WriteString(truncated)
	b.WriteString("\n")
	b.WriteString(fence)
	b.WriteString("\n")
	return b.String()
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

// TruncateDiff clips diff output at MaxInlinedDiffBytes and appends a
// clear marker recording how many bytes were dropped. Shorter inputs
// are returned unchanged.
func TruncateDiff(diff string) string {
	if len(diff) <= MaxInlinedDiffBytes {
		return diff
	}
	omitted := len(diff) - MaxInlinedDiffBytes
	return fmt.Sprintf(
		"%s\n\n<!-- diff truncated at %d KB; %d bytes omitted -->",
		diff[:MaxInlinedDiffBytes],
		MaxInlinedDiffBytes/1024,
		omitted,
	)
}

// TruncateTitle shortens a thread title to at most MaxTitleRunes runes,
// appending an ellipsis marker. Crucially, it truncates on rune
// boundaries so multibyte codepoints (CJK, combining marks, emoji)
// don't end up split into an invalid UTF-8 sequence.
func TruncateTitle(title string) string {
	if utf8.RuneCountInString(title) <= MaxTitleRunes {
		return title
	}

	const suffix = "..."
	keep := MaxTitleRunes - utf8.RuneCountInString(suffix)
	if keep < 1 {
		keep = 1
	}

	count := 0
	end := 0
	for i := range title {
		if count == keep {
			end = i
			break
		}
		count++
	}
	if count < keep {
		// The whole string fit inside keep runes — shouldn't happen given
		// the length check above, but be defensive.
		return title
	}
	return title[:end] + suffix
}
