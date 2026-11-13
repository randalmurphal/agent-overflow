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
