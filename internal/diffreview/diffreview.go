// Package diffreview owns the pure helpers behind the diff-review
// comment flow: rendering draft comments as a plain-text prompt block
// the agent can read, picking the best line anchor, and projecting a
// comment slice into its ID list.
//
// The App-bound CRUD methods, store reads/writes, and the
// `SendDiffReviewComments` saga that turns drafts into a follow-up
// message stay in `app_diff_review_comments.go`.
package diffreview

import (
	"strconv"
	"strings"

	"agent-overflow/internal/store"
)

// BuildPrompt renders draft diff-review comments into a plain-text
// block the agent can read directly. Each comment becomes:
//
//	<file_path>[:<line>]:
//	comment: <body>
//
// The line number is the new-side line when present, otherwise the
// old-side line; file-level comments emit no line. Multiple comments
// are separated by a blank line. The agent gets the diff itself in
// the same turn, so we deliberately omit `side` and the
// `selectedText` echo — the file:line anchor is enough to locate the
// comment.
func BuildPrompt(comments []store.DiffReviewComment) string {
	return buildPrompt(comments, nil)
}

func BuildPromptWithPRContext(comments []store.DiffReviewComment, pr *store.DiffReviewPRContext) string {
	return buildPrompt(comments, pr)
}

func buildPrompt(comments []store.DiffReviewComment, pr *store.DiffReviewPRContext) string {
	var b strings.Builder
	if pr != nil {
		b.WriteString("PR #")
		b.WriteString(strconv.Itoa(pr.Number))
		if strings.TrimSpace(pr.URL) != "" {
			b.WriteString(" - ")
			b.WriteString(strings.TrimSpace(pr.URL))
		}
	}
	hunks := hunkExcerptsByCommentID(pr)
	for _, comment := range comments {
		body := strings.TrimSpace(comment.Body)
		if body == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(comment.FilePath)
		b.WriteString(":")
		if line := CommentLine(comment); line > 0 {
			b.WriteString(strconv.Itoa(line))
			b.WriteString(":")
		}
		b.WriteString("\ncomment: ")
		b.WriteString(body)
		if hunk := strings.Trim(hunks[comment.ID], "\r\n"); hunk != "" {
			b.WriteString("\n\nhunk:\n")
			b.WriteString(hunk)
		}
	}
	return b.String()
}

func hunkExcerptsByCommentID(pr *store.DiffReviewPRContext) map[string]string {
	if pr == nil || len(pr.Comments) == 0 {
		return nil
	}
	out := make(map[string]string, len(pr.Comments))
	for _, entry := range pr.Comments {
		id := strings.TrimSpace(entry.CommentID)
		if id == "" {
			continue
		}
		out[id] = strings.Trim(entry.HunkExcerpt, "\r\n")
	}
	return out
}

// CommentLine returns the line number to use as the comment's anchor.
// Prefers the new-side line; falls back to the old-side line; returns
// 0 for file-level comments so `BuildPrompt` can omit the line entirely.
func CommentLine(comment store.DiffReviewComment) int {
	if comment.NewLine > 0 {
		return comment.NewLine
	}
	if comment.OldLine > 0 {
		return comment.OldLine
	}
	return 0
}

// IDsOf projects a comment slice into its ID list, preserving order.
func IDsOf(comments []store.DiffReviewComment) []string {
	ids := make([]string, 0, len(comments))
	for _, comment := range comments {
		ids = append(ids, comment.ID)
	}
	return ids
}
