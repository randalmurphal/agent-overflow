package git

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// GitLab's comment actions (https://docs.gitlab.com/api/discussions/,
// https://docs.gitlab.com/api/notes/): a comment now on the diff is a new
// discussion positioned on the merge request's current diff, one on the
// merge request itself a note, and an edit or delete addresses a note in
// its discussion.

// gitlabDiscussionIn is the REST body of a new positioned discussion.
type gitlabDiscussionIn struct {
	Body     string             `json:"body"`
	Position *gitlabPositionRaw `json:"position"`
}

// PostLineComment reads the merge request for the diff refs a position
// names and refuses with *HeadMovedError when its head is not headSHA:
// the caller's line numbers are of that diff.
func (f *gitlabForge) PostLineComment(ctx context.Context, ref PRReference, headSHA string, comment ReviewLineComment) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	mr, err := readGitLabMR(ctx, client, ref)
	if err != nil {
		return err
	}
	if have := mr.headSHA(); !strings.EqualFold(have, headSHA) {
		return &HeadMovedError{Want: headSHA, Have: have}
	}
	position, err := gitlabPositionForComment(comment, mr.diffRefs())
	if err != nil {
		return err
	}
	request := forgeapi.Request{Method: http.MethodPost, Path: gitlabMRPath(ref) + "/discussions", Body: gitlabDiscussionIn{Body: comment.Body, Position: position}}
	_, err = client.JSON(ctx, request, nil)
	return err
}

func (f *gitlabForge) PostPRComment(ctx context.Context, ref PRReference, body string) error {
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodPost, Path: gitlabMRPath(ref) + "/notes", Body: map[string]string{"body": body}}, nil)
	return err
}

// gitlabNotePath is the REST path of one note in its discussion.
func gitlabNotePath(ref PRReference, target CommentTarget) (string, error) {
	id, err := strconv.ParseInt(target.CommentID, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("GitLab note id %q is not a note id", target.CommentID)
	}
	return gitlabDiscussionPath(ref, target.ThreadID) + "/notes/" + url.PathEscape(target.CommentID), nil
}

// EditComment updates the note's body and checks that the answer is that
// note: a 200 that changed another must not read as the edit.
func (f *gitlabForge) EditComment(ctx context.Context, ref PRReference, target CommentTarget, body string) error {
	path, err := gitlabNotePath(ref, target)
	if err != nil {
		return err
	}
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	var note struct {
		ID int64 `json:"id"`
	}
	if _, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodPut, Path: path, Body: map[string]string{"body": body}}, &note); err != nil {
		return err
	}
	if strconv.FormatInt(note.ID, 10) != target.CommentID {
		return fmt.Errorf("GitLab answered note %d for an edit of note %s", note.ID, target.CommentID)
	}
	return nil
}

func (f *gitlabForge) DeleteComment(ctx context.Context, ref PRReference, target CommentTarget) error {
	path, err := gitlabNotePath(ref, target)
	if err != nil {
		return err
	}
	client, err := f.core.gitlabAPI(ref.Host)
	if err != nil {
		return err
	}
	_, err = client.JSON(ctx, forgeapi.Request{Method: http.MethodDelete, Path: path}, nil)
	return err
}
