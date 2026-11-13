package store

import (
	"errors"
	"fmt"
	"strings"
)

// The shapes a draft can take beyond one line or a file: a range of lines
// and a held reply to a forge thread (migration v148).

// MaxDiffReviewReplyThreadIDBytes bounds a held reply's thread id (a GitHub
// node id or a GitLab discussion id).
const MaxDiffReviewReplyThreadIDBytes = 256

// ErrDiffReviewReplyExists is a second draft reply to a thread that already
// has one for the same owner and source: the pane edits the one it holds.
var ErrDiffReviewReplyExists = errors.New("this thread already has a draft reply")

// diffReviewLineSides are the halves of the diff a row is on: a removed
// line the left, an added line the right, an unchanged line both.
func diffReviewLineSides(oldLine, newLine int) (left, right bool) {
	return oldLine > 0, newLine > 0
}

// validateDiffReviewRange checks a range's first line against its last
// (oldLine, newLine): the first is a line of the diff, the two share a
// half, and on every half they share the first comes before the last.
func validateDiffReviewRange(side string, oldLine, newLine, startOld, startNew int) error {
	if startOld < 0 || startNew < 0 {
		return fmt.Errorf("range line numbers must be positive")
	}
	if startOld == 0 && startNew == 0 {
		return nil
	}
	switch side {
	case "old", "new", "context":
	default:
		return fmt.Errorf("a %s comment has no range", side)
	}
	startLeft, startRight := diffReviewLineSides(startOld, startNew)
	endLeft, endRight := diffReviewLineSides(oldLine, newLine)
	sharesLeft, sharesRight := startLeft && endLeft, startRight && endRight
	switch {
	case !sharesLeft && !sharesRight:
		return fmt.Errorf("a range starts and ends on the same side of the diff")
	case sharesLeft && startOld >= oldLine, sharesRight && startNew >= newLine:
		return fmt.Errorf("a range starts before the line it ends on")
	}
	return nil
}

// normalizeDiffReviewReply checks a held reply: pr scope, a thread id, no
// line, and the thread's file when it has one.
func normalizeDiffReviewReply(comment *DiffReviewComment) error {
	threadID := strings.TrimSpace(comment.ReplyThreadID)
	switch {
	case comment.Scope != string(DiffReviewScopePR):
		return fmt.Errorf("a reply to a thread is pr scope")
	case threadID == "" || len(threadID) > MaxDiffReviewReplyThreadIDBytes || hasControlCharacter(threadID):
		return fmt.Errorf("a reply names a valid thread id")
	case comment.Side != "" && comment.Side != "reply":
		return fmt.Errorf("a reply to a thread has side reply")
	case comment.OldLine != 0 || comment.NewLine != 0 || comment.StartOldLine != 0 || comment.StartNewLine != 0:
		return fmt.Errorf("a reply to a thread has no line")
	case comment.ReplyRootID < 0:
		return fmt.Errorf("a reply's root comment id must be positive")
	}
	if strings.TrimSpace(comment.FilePath) != "" {
		filePath, err := validateDiffReviewAnchor(comment.FilePath, 0, 0)
		if err != nil {
			return err
		}
		comment.FilePath = filePath
	} else {
		comment.FilePath = ""
	}
	comment.ReplyThreadID, comment.Side = threadID, "reply"
	return nil
}

// normalizeDiffReviewShape checks and normalizes comment's anchor: a held
// reply, or a line, range or file.
func normalizeDiffReviewShape(comment *DiffReviewComment) error {
	if comment.ReplyThreadID != "" || comment.Side == "reply" {
		return normalizeDiffReviewReply(comment)
	}
	filePath, err := validateDiffReviewAnchor(comment.FilePath, comment.OldLine, comment.NewLine)
	if err != nil {
		return err
	}
	side, err := normalizeDiffReviewSide(comment.Side, comment.OldLine, comment.NewLine)
	if err != nil {
		return err
	}
	if comment.ReplyRootID != 0 {
		return fmt.Errorf("only a reply names a root comment")
	}
	if err := validateDiffReviewRange(side, comment.OldLine, comment.NewLine, comment.StartOldLine, comment.StartNewLine); err != nil {
		return err
	}
	comment.FilePath, comment.Side = filePath, side
	return nil
}

// NormalizeDiffReviewLineAnchor checks a line, range or file anchor as a
// draft's is checked, for a comment posted now without a draft. A reply
// is not an anchor.
func NormalizeDiffReviewLineAnchor(comment *DiffReviewComment) error {
	if comment.ReplyThreadID != "" || comment.Side == "reply" {
		return fmt.Errorf("a reply to a thread is not a line comment")
	}
	return normalizeDiffReviewShape(comment)
}

// Bounds of the PR context a send carries for the agent.
const (
	MaxDiffReviewThreadAuthorBytes = 256
	MaxDiffReviewThreadComments    = 500
	MaxDiffReviewPRURLBytes        = 2048
)

// Validate checks the PR context a send carries: it is the client's
// account of other people's text, so every one-line field is one line of
// bounded length, every line number is a line, and each list is bounded.
// The renderer quotes the bodies; this keeps the metadata it puts on a
// header line from carrying a line of its own.
func (c *DiffReviewPRContext) Validate() error {
	if c == nil {
		return nil
	}
	switch {
	case c.Number < 0:
		return fmt.Errorf("the PR number must be positive")
	case len(c.URL) > MaxDiffReviewPRURLBytes || hasControlCharacter(c.URL):
		return fmt.Errorf("the PR url must be one line of at most %d bytes", MaxDiffReviewPRURLBytes)
	case len(c.Comments) > MaxDiffReviewCommentIDs:
		return fmt.Errorf("the PR context names too many comments")
	}
	for i, entry := range c.Comments {
		if len(entry.CommentID) > 128 || hasControlCharacter(entry.CommentID) {
			return fmt.Errorf("comments[%d]: the comment id must be one short line", i)
		}
		if len(entry.HunkExcerpt) > MaxDiffReviewSelectedTextBytes {
			return fmt.Errorf("comments[%d]: the hunk excerpt is too large", i)
		}
		if err := entry.Thread.validate(); err != nil {
			return fmt.Errorf("comments[%d].thread: %w", i, err)
		}
	}
	return nil
}

func (t *DiffReviewThreadContext) validate() error {
	if t == nil {
		return nil
	}
	switch {
	case len(t.Path) > MaxDiffReviewFilePathBytes || hasControlCharacter(t.Path):
		return fmt.Errorf("the path must be one line of at most %d bytes", MaxDiffReviewFilePathBytes)
	case t.Line < 0 || t.StartLine < 0 || (t.StartLine > 0 && t.StartLine > t.Line):
		return fmt.Errorf("the lines must be a line or a range")
	case len(t.Comments) > MaxDiffReviewThreadComments:
		return fmt.Errorf("the thread has more than %d comments", MaxDiffReviewThreadComments)
	}
	for i, comment := range t.Comments {
		switch {
		case len(comment.Author) > MaxDiffReviewThreadAuthorBytes || hasControlCharacter(comment.Author):
			return fmt.Errorf("comments[%d]: the author must be one line of at most %d bytes", i, MaxDiffReviewThreadAuthorBytes)
		case len(comment.Body) > MaxDiffReviewCommentBodyBytes:
			return fmt.Errorf("comments[%d]: the body is too large", i)
		}
	}
	return nil
}
