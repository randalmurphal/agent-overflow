package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
	"agent-overflow/internal/mdsuggest"
)

// GitLab's suggestions: the discussions read carries each note's
// suggestions as GitLab parsed them (https://docs.gitlab.com/api/discussions/,
// notes[].suggestions), with the replaced text, appliable and applied;
// the apply is PUT suggestions/:id/apply
// (https://docs.gitlab.com/api/suggestions/), which needs the Developer
// role or above.

// gitlabSuggestionRaw is one notes[].suggestions entry.
type gitlabSuggestionRaw struct {
	ID          int64  `json:"id"`
	FromLine    int    `json:"from_line"`
	ToLine      int    `json:"to_line"`
	Appliable   bool   `json:"appliable"`
	Applied     bool   `json:"applied"`
	FromContent string `json:"from_content"`
	ToContent   string `json:"to_content"`
}

// gitlabSuggestions maps a note's suggestions, in GitLab's order (the
// body's), placing suggestion i on the body's block i when the body holds
// as many blocks as GitLab reports suggestions.
func gitlabSuggestions(raw []gitlabSuggestionRaw, body string) []CommentSuggestion {
	var out []CommentSuggestion
	var blocks []mdsuggest.Block
	if len(raw) > 0 {
		blocks = mdsuggest.Parse(body)
	}
	for i, s := range raw {
		out = append(out, CommentSuggestion{
			Index: i, ID: strconv.FormatInt(s.ID, 10), StartLine: s.FromLine, EndLine: s.ToLine,
			From: splitContentLines(s.FromContent), FromKnown: true, To: splitContentLines(s.ToContent),
			Appliable: s.Appliable && !s.Applied, Applied: s.Applied,
			Digest: SuggestionDigest("gitlab", s.FromLine, s.ToLine, splitContentLines(s.ToContent)),
		})
		if len(blocks) == len(raw) {
			last := &out[len(out)-1]
			last.BodyStartLine, last.BodyEndLine, last.BodyPlaced = blocks[i].StartLine, blocks[i].EndLine, true
		}
	}
	return out
}

// gateGitLabSuggestions withholds Apply below the Developer role, which
// GitLab's suggestions API requires (gitlabDeveloperAccess); appliable
// says nothing of the viewer.
func gateGitLabSuggestions(threads []ReviewThread, access int) {
	if access >= gitlabDeveloperAccess {
		return
	}
	for i := range threads {
		for j := range threads[i].Comments {
			for k := range threads[i].Comments[j].Suggestions {
				threads[i].Comments[j].Suggestions[k].Appliable = false
			}
		}
	}
}

// ApplySuggestion checks, before the apply, that the merge request is
// open at the head the caller saw and that the suggestion is one of the
// named note's in the named discussion of this merge request (the
// suggestion id is global on the host), then applies it. An answer that
// does not report it applied is unconfirmed.
func (f *gitlabForge) ApplySuggestion(ctx context.Context, ref PRReference, req SuggestionApply) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	mr, err := readGitLabMR(ctx, client, ref)
	if err != nil {
		return err
	}
	if !strings.EqualFold(mr.State, "opened") {
		return &SuggestionNotAppliedError{Reason: "the merge request is not open"}
	}
	if have := mr.headSHA(); !strings.EqualFold(have, req.HeadSHA) {
		return &HeadMovedError{Want: req.HeadSHA, Have: have}
	}
	var discussion gitlabDiscussionRaw
	if _, err := client.JSON(ctx, forgeapi.Request{Path: gitlabDiscussionPath(ref, req.Target.ThreadID)}, &discussion); err != nil {
		if errors.Is(err, forgeapi.ErrNotFound) {
			return &CommentNotOnPRError{CommentID: req.Target.CommentID}
		}
		return err
	}
	// GitLab's apply takes no expected content, so the suggestion is
	// re-read here and compared with what the caller was shown. An edit
	// between this read and the apply is not caught here; GitLab refuses
	// an apply whose lines changed.
	onNote, suggestion := gitlabDiscussionSuggestion(discussion, req)
	if !onNote {
		return &CommentNotOnPRError{CommentID: req.Target.CommentID}
	}
	if suggestion == nil || suggestion.Digest != req.Digest {
		return &SuggestionChangedError{}
	}
	var applied gitlabSuggestionRaw
	path := "suggestions/" + req.SuggestionID + "/apply"
	if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPut, Path: path}, &applied); err != nil {
		return writeOutcome(err)
	}
	if strconv.FormatInt(applied.ID, 10) != req.SuggestionID || !applied.Applied {
		return &UnconfirmedWriteError{Err: fmt.Errorf("GitLab answered the apply of suggestion %s with suggestion %d, applied %t", req.SuggestionID, applied.ID, applied.Applied)}
	}
	return nil
}

// gitlabDiscussionSuggestion finds the named note in discussion (onNote)
// and, read as the threads read reads it, its suggestion with the named
// id; nil when the note no longer has it.
func gitlabDiscussionSuggestion(discussion gitlabDiscussionRaw, req SuggestionApply) (onNote bool, suggestion *CommentSuggestion) {
	for _, note := range discussion.Notes {
		if strconv.FormatInt(note.ID, 10) != req.Target.CommentID {
			continue
		}
		for _, s := range gitlabSuggestions(note.Suggestions, note.Body) {
			if s.ID == req.SuggestionID {
				return true, &s
			}
		}
		return true, nil
	}
	return false, nil
}
