package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Suggested changes and reactions on a PR's comments: what the threads
// read carries for each comment (ReviewComment.Suggestions and
// Reactions) and the writes that apply one suggestion or add or remove
// the viewer's reaction. See docs/architecture/forge-transport.md,
// "Suggestions and reactions".

// CommentSuggestion is one suggestion block of a comment, in body order
// (Index). StartLine and EndLine are the new-side lines it replaces,
// zero when the anchor names none on the new side (an old-side line, a
// file, an outdated GitHub thread). From is the replaced text when the
// read proves it (FromKnown: GitLab's from_content), else absent and
// the client shows the additions alone; To is the replacement. ID is
// GitLab's suggestion id, which the apply names; GitHub's apply names
// Index. Appliable is the forge's answer (GitLab's appliable, with the
// developer role its suggestions API requires) or the adapter's own from
// the read (GitHub: the PR is open, the thread current, unresolved and
// on the new side, and viewerCanApplySuggestion). Applied is GitLab's;
// GitHub does not say.
type CommentSuggestion struct {
	Index     int      `json:"index"`
	ID        string   `json:"id,omitempty"`
	StartLine int      `json:"startLine,omitempty"`
	EndLine   int      `json:"endLine,omitempty"`
	From      []string `json:"from,omitempty"`
	FromKnown bool     `json:"fromKnown,omitempty"`
	To        []string `json:"to"`
	Appliable bool     `json:"appliable"`
	Applied   bool     `json:"applied,omitempty"`
	// Digest names what the user is shown and what an apply writes
	// (SuggestionDigest); the apply presents it and is refused when the
	// suggestion no longer has it.
	Digest string `json:"digest"`
}

// SuggestionDigest is the lowercase hex SHA-256 of a canonical encoding
// of a suggestion as shown and as written: the forge, the first and last
// replaced line, and the replacement lines, each length-prefixed so no
// two suggestions encode alike. The read and the apply both use it.
func SuggestionDigest(forge string, start, end int, to []string) string {
	h := sha256.New()
	field := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	field("ao-suggestion-v1")
	field(forge)
	field(strconv.Itoa(start))
	field(strconv.Itoa(end))
	field(strconv.Itoa(len(to)))
	for _, line := range to {
		field(line)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// SuggestionChangedError refuses an apply whose suggestion is no longer
// the one the caller's pane showed: its digest differs, or its index or
// GitLab id is gone. Nothing is written; the pane's read is stale.
type SuggestionChangedError struct{}

func (e *SuggestionChangedError) Error() string {
	return "the suggestion changed since it was loaded"
}

// SuggestionApply names one suggestion to apply: the comment it is in
// (Target), its Index in that comment's suggestions, GitLab's suggestion
// ID, the Digest the read gave it (CommentSuggestion.Digest; a
// suggestion that no longer has it is *SuggestionChangedError), and
// HeadSHA, the head the caller's pane loaded. A head that moved
// is refused (*HeadMovedError) before anything is written; GitHub's
// commit also names it as its expectedHeadOid.
type SuggestionApply struct {
	Target       CommentTarget `json:"target"`
	Index        int           `json:"index"`
	SuggestionID string        `json:"suggestionId,omitempty"`
	Digest       string        `json:"digest"`
	HeadSHA      string        `json:"headSha"`
}

var suggestionDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the request's shape for forge.
func (s SuggestionApply) Validate(forge string) error {
	if err := s.Target.Validate(); err != nil {
		return err
	}
	if !mergeHeadSHAPattern.MatchString(strings.TrimSpace(s.HeadSHA)) {
		return errors.New("applying a suggestion requires the head commit sha the caller saw")
	}
	if s.Index < 0 {
		return errors.New("a suggestion index is not negative")
	}
	if !suggestionDigestPattern.MatchString(s.Digest) {
		return errors.New("applying a suggestion requires the digest the read gave it")
	}
	if forge == "gitlab" {
		if id, err := strconv.ParseInt(s.SuggestionID, 10, 64); err != nil || id <= 0 {
			return fmt.Errorf("GitLab suggestion id %q is not a suggestion id", s.SuggestionID)
		}
	}
	return nil
}

// SuggestionCommitHeadline is the commit message GitHub's own "Commit
// suggestion" writes, which the GitHub apply writes too.
const SuggestionCommitHeadline = "Apply suggestion from code review"

// suggestionFileMaxBytes bounds the file a GitHub apply reads and
// rewrites: past it the apply is refused before the file is read whole.
const suggestionFileMaxBytes = 1 << 20

// SuggestionNotAppliedError refuses an apply before anything is written:
// the comment holds no such suggestion, the thread is outdated or not on
// the new side, the PR is not open, or the file cannot be rewritten (past
// suggestionFileMaxBytes, binary, not UTF-8, or shorter than the anchor).
// Reason completes "The suggestion was not applied: ...".
type SuggestionNotAppliedError struct {
	Reason string
}

func (e *SuggestionNotAppliedError) Error() string {
	return "the suggestion was not applied: " + e.Reason
}

// Reaction names: the eight both forges share, in GitLab's award emoji
// names, which are the vocabulary of CommentReaction.Name. Any other
// name is a GitLab award emoji of its own.
const (
	ReactionThumbsUp   = "thumbsup"
	ReactionThumbsDown = "thumbsdown"
	ReactionLaugh      = "laughing"
	ReactionHooray     = "tada"
	ReactionConfused   = "confused"
	ReactionHeart      = "heart"
	ReactionRocket     = "rocket"
	ReactionEyes       = "eyes"
)

// githubReactionContent maps the shared names to GitHub's ReactionContent.
var githubReactionContent = map[string]string{
	ReactionThumbsUp: "THUMBS_UP", ReactionThumbsDown: "THUMBS_DOWN", ReactionLaugh: "LAUGH", ReactionHooray: "HOORAY",
	ReactionConfused: "CONFUSED", ReactionHeart: "HEART", ReactionRocket: "ROCKET", ReactionEyes: "EYES",
}

// githubReactionName is the shared name of a GitHub ReactionContent, ""
// for one outside the eight.
func githubReactionName(content string) string {
	for name, value := range githubReactionContent {
		if value == content {
			return name
		}
	}
	return ""
}

// CommentReaction is one reaction group on a comment: its Name (the
// shared vocabulary, or a GitLab award emoji's own name), how many
// reacted, whether the viewer did, and who, when the read already has
// them (GitLab; GitHub's would cost a connection per comment, so it
// leaves Users empty).
type CommentReaction struct {
	Name          string   `json:"name"`
	Count         int      `json:"count"`
	ViewerReacted bool     `json:"viewerReacted"`
	Users         []string `json:"users,omitempty"`
}

var gitlabEmojiName = regexp.MustCompile(`^[a-z0-9_+-]{1,64}$`)

// ValidateReactionName checks a reaction a write names: one of the
// eight on GitHub, a GitLab award emoji name on GitLab.
func ValidateReactionName(forge, name string) error {
	if forge == "github" {
		if _, ok := githubReactionContent[name]; !ok {
			return fmt.Errorf("GitHub has no reaction %q", name)
		}
		return nil
	}
	if !gitlabEmojiName.MatchString(name) {
		return fmt.Errorf("%q is not an emoji name", name)
	}
	return nil
}

// ForgeSuggester is the suggestion and reaction half of Forge. Every
// write takes an interactive context and checks inside the adapter that
// the comment, thread and suggestion it names are on the PR.
type ForgeSuggester interface {
	// ApplySuggestion applies one suggestion: GitLab's apply endpoint, or
	// on GitHub a commit of the rewritten file on the head branch and the
	// thread resolved after it.
	ApplySuggestion(ctx context.Context, ref PRReference, req SuggestionApply) error
	// SetCommentReaction adds (reacted) or removes the viewer's reaction
	// name on one comment. The reaction already as asked is a success
	// with nothing written.
	SetCommentReaction(ctx context.Context, ref PRReference, target CommentTarget, name string, reacted bool) error
}

func (nullForge) ApplySuggestion(context.Context, PRReference, SuggestionApply) error {
	return ErrUnsupportedForge
}

func (nullForge) SetCommentReaction(context.Context, PRReference, CommentTarget, string, bool) error {
	return ErrUnsupportedForge
}

// ApplyPRSuggestion validates and dispatches one suggestion's apply.
func (c *Core) ApplyPRSuggestion(ctx context.Context, ref PRReference, req SuggestionApply) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	req.HeadSHA = strings.TrimSpace(req.HeadSHA)
	if err := req.Validate(ref.Forge); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).ApplySuggestion(forgeWrite(ctx), ref, req)
}

// SetPRCommentReaction validates and dispatches a reaction change.
func (c *Core) SetPRCommentReaction(ctx context.Context, ref PRReference, target CommentTarget, name string, reacted bool) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if err := ValidateReactionName(ref.Forge, name); err != nil {
		return err
	}
	return c.ForgeByID(ref.Forge).SetCommentReaction(forgeWrite(ctx), ref, target, name, reacted)
}

// applySuggestionToFile is the file content after replacing its lines
// start to end (1-based, inclusive) with lines, as a byte splice: the
// bytes before line start and after line end's ending are the input's.
// A line ends at "\n", its ending "\r\n" when a CR precedes it. The
// replacement lines are joined and ended with the last replaced line's
// ending; when that line has none (the file's last, with no final
// newline), they are joined with the nearest preceding line ending in
// the file (LF when there is none) and the last gets none. A deletion
// removes the lines with their endings. A UTF-8 byte order mark stays in
// front when line 1 is replaced. A file that is not UTF-8 text, holds a
// NUL byte, or has fewer lines than end, is refused.
func applySuggestionToFile(content []byte, start, end int, lines []string) ([]byte, error) {
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, &SuggestionNotAppliedError{Reason: "the file is not text"}
	}
	// starts[i] is line i+1's first byte; a last line without an ending
	// counts when it holds anything.
	starts := []int{}
	for at := 0; at < len(content); {
		starts = append(starts, at)
		i := bytes.IndexByte(content[at:], '\n')
		if i < 0 {
			break
		}
		at += i + 1
	}
	if start < 1 || end < start || end > len(starts) {
		return nil, &SuggestionNotAppliedError{Reason: fmt.Sprintf("the file has %d lines, not the lines %d to %d the comment is on", len(starts), start, end)}
	}
	lineEnding := func(n int) string { // line n's ending, 1-based
		stop := len(content)
		if n < len(starts) {
			stop = starts[n]
		}
		switch {
		case stop == 0 || content[stop-1] != '\n':
			return ""
		case stop >= 2 && content[stop-2] == '\r':
			return "\r\n"
		}
		return "\n"
	}
	from, to := starts[start-1], len(content)
	if end < len(starts) {
		to = starts[end]
	}
	if start == 1 && bytes.HasPrefix(content, utf8BOM) {
		from = len(utf8BOM)
	}
	var replacement strings.Builder
	if len(lines) > 0 {
		eol, last := lineEnding(end), lineEnding(end)
		for n := end - 1; eol == "" && n >= 1; n-- {
			eol = lineEnding(n)
		}
		if eol == "" {
			eol = "\n"
		}
		replacement.WriteString(strings.Join(lines, eol))
		replacement.WriteString(last)
	}
	out := make([]byte, 0, from+replacement.Len()+len(content)-to)
	out = append(out, content[:from]...)
	out = append(out, replacement.String()...)
	return append(out, content[to:]...), nil
}

var utf8BOM = []byte{0xef, 0xbb, 0xbf}
