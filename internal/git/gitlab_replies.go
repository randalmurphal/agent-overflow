package git

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// A GitLab review's held replies are draft notes in their discussions
// (in_reply_to_discussion_id), published with the review. GitLab answers
// a draft reply to a discussion that is gone with a 400, and a publish
// after the discussion went with success, dropping the reply: so a submit
// reads the discussions before it creates drafts (each reply's discussion
// exists, and whether it is resolved, which the draft keeps) and after it
// published (each reply is a note of its discussion), and reports a reply
// it cannot find as not posted.

// gitlabReplyThreads are the merge request's discussions by id, as a
// submit with held replies reads them before creating drafts.
type gitlabReplyThreads map[string]ReviewThread

func readGitLabReplyThreads(ctx context.Context, client *forgeapi.Client, ref PRReference, mr gitlabMRRaw) (gitlabReplyThreads, error) {
	threads, _, err := readGitLabDiscussions(ctx, client, ref, gitlabThreadContext{headSHA: mr.headSHA(), webURL: mr.WebURL})
	if err != nil {
		return nil, err
	}
	out := make(gitlabReplyThreads, len(threads))
	for _, thread := range threads {
		out[thread.ID] = thread
	}
	return out, nil
}

// resolveFor is the resolve_discussion a reply into thread sends: the
// discussion's state now, so publishing the reply neither resolves nor
// reopens it. A discussion that cannot be resolved takes none.
func (threads gitlabReplyThreads) resolveFor(threadID string) *bool {
	thread := threads[threadID]
	if !thread.IsResolvable {
		return nil
	}
	resolved := thread.IsResolved
	return &resolved
}

// gitlabDiscussionGone reports whether the merge request has no discussion
// id: the cause of a 400 on a draft reply GitLab gives no code for.
func gitlabDiscussionGone(ctx context.Context, client *forgeapi.Client, ref PRReference, id string) bool {
	_, err := client.JSON(ctx, forgeapi.Request{Method: http.MethodGet, Path: gitlabDiscussionPath(ref, id)}, nil)
	return errors.Is(err, forgeapi.ErrNotFound)
}

// gitlabHeldReply is one held reply of a submit: its ordinal among the
// request's replies, and the comment.
type gitlabHeldReply struct {
	ordinal int
	comment ReviewLineComment
}

// lostGitLabReplies reads the discussions after a publish and returns the
// ordinals of the replies that are not a new note of their discussion: a
// note by viewer (any author when the login is unknown) with the reply's
// body that before did not have.
func lostGitLabReplies(ctx context.Context, client *forgeapi.Client, ref PRReference, mr gitlabMRRaw, before gitlabReplyThreads, replies []gitlabHeldReply, viewer string) ([]int, error) {
	after, err := readGitLabReplyThreads(ctx, client, ref, mr)
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	var lost []int
	for _, reply := range replies {
		threadID := reply.comment.InReplyTo.ThreadID
		existed := map[string]bool{}
		for _, comment := range before[threadID].Comments {
			existed[comment.ID] = true
		}
		found := false
		for _, comment := range after[threadID].Comments {
			if existed[comment.ID] || used[comment.ID] || strings.TrimSpace(comment.Body) != strings.TrimSpace(reply.comment.Body) {
				continue
			}
			if viewer != "" && !strings.EqualFold(comment.AuthorLogin, viewer) {
				continue
			}
			used[comment.ID], found = true, true
			break
		}
		if !found {
			lost = append(lost, reply.ordinal)
		}
	}
	return lost, nil
}
