package git

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// An edit or delete of a comment whose answer cannot say whether the
// forge applied it (a GraphQL answer with data beside errors, a 5xx) may
// have applied: unconfirmed, never refused; a refusal stays one.
func TestCommentEditAndDeleteOutcomes(t *testing.T) {
	t.Parallel()
	githubCore := func(answer string) *Core {
		core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			if call.Op == "CommentOwner" {
				return githubData(`{"node":{"__typename":"PullRequestReviewComment","pullRequest":{"number":5,"repository":{"nameWithOwner":"o/r"}}}}`)
			}
			return forgeAPIAnswer{Body: answer}
		})
		return core
	}
	review := CommentTarget{ThreadID: "PRRT_1", CommentID: "PRRC_2"}
	ghRef := githubTestRef(5)
	for _, tc := range []struct {
		name, answer string
		write        func(*Core) error
		action       ForgeAction
		unconfirmed  bool
	}{
		{"edit with data and an error", `{"data":{"updatePullRequestReviewComment":{"pullRequestReviewComment":{"id":"PRRC_2"}}},"errors":[{"message":"Something went wrong"}]}`,
			func(c *Core) error { return c.EditPRComment(t.Context(), ghRef, review, "new") }, ActionEdit, true},
		{"delete with data and an error", `{"data":{"deletePullRequestReviewComment":{"clientMutationId":null}},"errors":[{"message":"timeout"}]}`,
			func(c *Core) error { return c.DeletePRComment(t.Context(), ghRef, review) }, ActionDelete, true},
		{"edit refused", `{"data":{"updatePullRequestReviewComment":null},"errors":[{"type":"FORBIDDEN","message":"no"}]}`,
			func(c *Core) error { return c.EditPRComment(t.Context(), ghRef, review, "new") }, ActionEdit, false},
	} {
		t.Run("github "+tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.write(githubCore(tc.answer))
			_, unsure := errors.AsType[*UnconfirmedWriteError](err)
			explained, _ := errors.AsType[*ForgeActionError](ExplainForgeError(ghRef, tc.action, err))
			if unsure != tc.unconfirmed || explained == nil || (explained.Kind == ForgeErrorUnconfirmed) != tc.unconfirmed {
				t.Fatalf("err = %v, explained %+v; want unconfirmed %v", err, explained, tc.unconfirmed)
			}
		})
	}
	note := gitlabTestProject + "/merge_requests/7/discussions/d1/notes/42"
	for _, status := range []int{http.StatusBadGateway, http.StatusForbidden} {
		t.Run(fmt.Sprintf("gitlab %d", status), func(t *testing.T) {
			t.Parallel()
			core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
				if call.Path == note {
					return forgeAPIAnswer{Status: status, Body: `{"message":"x"}`}
				}
				return forgeUnexpected(t, call)
			})
			target := CommentTarget{ThreadID: "d1", CommentID: "42"}
			for _, err := range []error{core.EditPRComment(t.Context(), gitlabTestRef(7), target, "new"), core.DeletePRComment(t.Context(), gitlabTestRef(7), target)} {
				_, unsure := errors.AsType[*UnconfirmedWriteError](err)
				if unsure != (status >= 500) {
					t.Fatalf("%d: err = %v", status, err)
				}
				if unsure && !strings.Contains(ExplainForgeError(gitlabTestRef(7), ActionEdit, err).Error(), "may have been applied") {
					t.Fatalf("explained = %v", ExplainForgeError(gitlabTestRef(7), ActionEdit, err))
				}
			}
		})
	}
}
