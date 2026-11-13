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
//	<file_path>[:<line>[-<line>]]:
//	comment: <body>
//
// The line number is the new-side line when present, otherwise the
// old-side line; a range names its first and last line on the half they
// share; file-level comments emit no line. A held reply names the thread
// it answers (BuildPromptWithPRContext adds the thread's comments). Multiple comments
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
	threads := threadsByCommentID(pr)
	for _, comment := range comments {
		body := strings.TrimSpace(comment.Body)
		if body == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if comment.ReplyThreadID != "" {
			writeReply(&b, comment, threads[comment.ID])
			continue
		}
		b.WriteString(comment.FilePath)
		b.WriteString(":")
		if line := commentLines(comment); line != "" {
			b.WriteString(line)
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

func threadsByCommentID(pr *store.DiffReviewPRContext) map[string]*store.DiffReviewThreadContext {
	if pr == nil {
		return nil
	}
	out := map[string]*store.DiffReviewThreadContext{}
	for _, entry := range pr.Comments {
		if id := strings.TrimSpace(entry.CommentID); id != "" && entry.Thread != nil {
			out[id] = entry.Thread
		}
	}
	return out
}

// commentLines is a comment's line ("12") or range ("12-18"), on the new
// side when both its lines have a new number, else the old side; empty for
// a file comment.
func commentLines(comment store.DiffReviewComment) string {
	end := CommentLine(comment)
	if end == 0 {
		return ""
	}
	if comment.StartOldLine == 0 && comment.StartNewLine == 0 {
		return strconv.Itoa(end)
	}
	start, last := comment.StartNewLine, comment.NewLine
	if start == 0 || last == 0 {
		start, last = comment.StartOldLine, comment.OldLine
	}
	return strconv.Itoa(start) + "-" + strconv.Itoa(last)
}

// writeReply renders a held reply under the thread it answers:
//
//	<file_path>[:<line>[-<line>]]: reply to this thread
//	> <author>: <comment>
//	comment: <body>
//
// A conversation thread has no file ("PR conversation"). With no thread
// context the reply names the thread by id.
func writeReply(b *strings.Builder, comment store.DiffReviewComment, thread *store.DiffReviewThreadContext) {
	path, lines := comment.FilePath, ""
	if thread != nil {
		path = thread.Path
		switch {
		case thread.StartLine > 0 && thread.Line > thread.StartLine:
			lines = strconv.Itoa(thread.StartLine) + "-" + strconv.Itoa(thread.Line)
		case thread.Line > 0:
			lines = strconv.Itoa(thread.Line)
		}
	}
	if path == "" {
		path = "PR conversation"
	}
	b.WriteString(path)
	b.WriteString(":")
	if lines != "" {
		b.WriteString(lines)
		b.WriteString(":")
	}
	if thread == nil {
		b.WriteString(" reply to thread ")
		b.WriteString(comment.ReplyThreadID)
	} else {
		b.WriteString(" reply to this thread")
		for _, said := range thread.Comments {
			text := strings.TrimSpace(said.Body)
			if text == "" {
				continue
			}
			b.WriteString("\n> ")
			b.WriteString(strings.TrimSpace(said.Author))
			b.WriteString(": ")
			b.WriteString(strings.ReplaceAll(text, "\n", "\n> "))
		}
	}
	b.WriteString("\ncomment: ")
	b.WriteString(strings.TrimSpace(comment.Body))
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
