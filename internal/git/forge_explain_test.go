package git

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/errorsx"
	"agent-overflow/internal/forgeapi"
)

var explainRef = PRReference{Forge: "gitlab", Host: "gitlab.example.com", Namespace: "team", Repo: "app", Number: 42}

// Every forge failure becomes one sentence with a kind, carried as
// reviewed prose under the code "forge_<kind>" with the forge's own error
// still in the chain.
func TestExplainForgeErrorSentences(t *testing.T) {
	t.Parallel()
	expired := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		action   ForgeAction
		err      error
		kind     string
		contains []string
	}{
		{"expired token", ActionMerge, &forgeapi.SetupError{Forge: "gitlab", Binary: "glab", Kind: forgeapi.SetupUnauthenticated, Host: "gitlab.example.com", ExpiredAt: expired, Message: "GitLab CLI is not logged in."},
			ForgeErrorSetup, []string{"token for gitlab.example.com expired on", "glab auth login --hostname gitlab.example.com"}},
		{"no token for the host", ActionRead, &forgeapi.SetupError{Forge: "gitlab", Binary: "glab", Kind: forgeapi.SetupUnauthenticated, Host: "gitlab.example.com", Message: "GitLab CLI is not logged in."},
			ForgeErrorSetup, []string{"has no token for gitlab.example.com"}},
		{"setup message kept", ActionRead, &forgeapi.SetupError{Forge: "gitlab", Binary: "glab", Kind: forgeapi.SetupMissing, Message: "glab is not installed."},
			ForgeErrorSetup, []string{"glab is not installed."}},
		{"missing scope", ActionMerge, &forgeapi.ForbiddenError{Status: &forgeapi.StatusError{Status: http.StatusForbidden}, Scope: "api"},
			ForgeErrorForbidden, []string{"token lacks the `api` scope", "cannot merge team/app!42", "log glab in"}},
		{"no access", ActionRead, &forgeapi.ForbiddenError{Status: &forgeapi.StatusError{Status: http.StatusForbidden}},
			ForgeErrorForbidden, []string{"No access to team/app on gitlab.example.com with this login."}},
		{"rules block the action", ActionMerge, &forgeapi.ForbiddenError{Status: &forgeapi.StatusError{Status: http.StatusForbidden}, Message: "Required review from a code owner."},
			ForgeErrorRefused, []string{"GitLab blocks this: Required review from a code owner"}},
		{"head moved", ActionMerge, &forgeapi.StatusError{Method: "PUT", Status: http.StatusConflict, Body: `{"message":"SHA does not match HEAD of source branch"}`},
			ForgeErrorStaleHead, []string{"The branch moved after you loaded this. Reload and try again."}},
		{"not mergeable", ActionMerge, &forgeapi.StatusError{Method: "PUT", Status: http.StatusMethodNotAllowed, Body: `{"message":"405 Method Not Allowed"}`},
			ForgeErrorRefused, []string{"GitLab refused to merge team/app!42: 405 Method Not Allowed"}},
		{"gone", ActionApprove, &forgeapi.StatusError{Method: "POST", Status: http.StatusNotFound},
			ForgeErrorNotFound, []string{"team/app!42 was not found on gitlab.example.com"}},
		{"server error", ActionRead, &forgeapi.StatusError{Method: "GET", Status: http.StatusBadGateway},
			ForgeErrorTransient, []string{"gitlab.example.com answered with an error (HTTP 502)"}},
		{"unreachable", ActionRead, &forgeapi.TransientError{Err: errors.New("dial tcp: connection refused")},
			ForgeErrorTransient, []string{"gitlab.example.com could not be reached"}},
		{"deadline", ActionRead, context.DeadlineExceeded,
			ForgeErrorTransient, []string{"could not be reached"}},
		{"own approval", ActionApprove, &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Message: "Can not approve your own pull request"}}},
			ForgeErrorRefused, []string{"You cannot approve your own pull request."}},
		{"other", ActionReply, errors.New("decode body: unexpected EOF"),
			ForgeErrorForge, []string{"GitLab did not reply on team/app!42: decode body: unexpected EOF"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ExplainForgeError(explainRef, tc.action, tc.err)
			var explained *ForgeActionError
			if !errors.As(err, &explained) {
				t.Fatalf("ExplainForgeError = %T %v, want *ForgeActionError", err, err)
			}
			if explained.Kind != tc.kind {
				t.Errorf("kind = %q, want %q (%s)", explained.Kind, tc.kind, explained.Message)
			}
			for _, want := range tc.contains {
				if !strings.Contains(explained.Message, want) {
					t.Errorf("message %q does not contain %q", explained.Message, want)
				}
			}
			if err.Error() != explained.Message {
				t.Errorf("Error() = %q, want the sentence %q", err.Error(), explained.Message)
			}
			if strings.Contains(explained.Message, "\n") {
				t.Errorf("message spans lines: %q", explained.Message)
			}
			code, message, ok := errorsx.PublicDetails(err)
			if !ok || code != "forge_"+tc.kind || message != explained.Message {
				t.Errorf("public details = (%q, %q, %v), want (forge_%s, the sentence, true)", code, message, ok, tc.kind)
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("the forge's error is not in the chain")
			}
		})
	}
}

// Nil stays nil and an explained error is not explained twice.
func TestExplainForgeErrorIdempotent(t *testing.T) {
	t.Parallel()
	if err := ExplainForgeError(explainRef, ActionRead, nil); err != nil {
		t.Fatalf("nil = %v", err)
	}
	once := ExplainForgeError(explainRef, ActionMerge, &forgeapi.StatusError{Status: http.StatusConflict})
	if twice := ExplainForgeError(explainRef, ActionRead, once); twice != once {
		t.Fatalf("explained twice: %v", twice)
	}
}

// A GitHub GraphQL NOT_FOUND is the same not-found answer a REST 404 is,
// sentence for sentence, not a refusal quoting the forge.
func TestExplainForgeErrorGraphQLNotFound(t *testing.T) {
	t.Parallel()
	ref := PRReference{Forge: "github", Host: "github.com", Namespace: "o", Repo: "r", Number: 5}
	missing := &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Type: "NOT_FOUND", Message: "Could not resolve to a PullRequest with the number of 5."}}}
	err := ExplainForgeError(ref, ActionMerge, missing)
	explained, ok := errors.AsType[*ForgeActionError](err)
	if !ok || explained.Kind != ForgeErrorNotFound {
		t.Fatalf("GraphQL NOT_FOUND = %T %v, want kind %q", err, err, ForgeErrorNotFound)
	}
	want := "o/r#5 was not found on GitHub, or this login cannot see it."
	if explained.Message != want {
		t.Fatalf("sentence = %q\nwant       %q", explained.Message, want)
	}
	if rest := ExplainForgeError(ref, ActionMerge, &forgeapi.StatusError{Method: "PUT", Status: http.StatusNotFound}); rest.Error() != explained.Message {
		t.Fatalf("REST 404 reads %q, GraphQL NOT_FOUND %q", rest.Error(), explained.Message)
	}
	if code, message, ok := errorsx.PublicDetails(err); !ok || code != "forge_not_found" || message != want {
		t.Fatalf("public details = (%q, %q, %v)", code, message, ok)
	}
	if !errors.Is(err, missing) {
		t.Fatal("the forge's error is not in the chain")
	}
	// A NOT_FOUND after another problem still names the missing object.
	second := &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Message: "Something else"}, {Type: "NOT_FOUND", Message: "Could not resolve"}}}
	if explained, _ := errors.AsType[*ForgeActionError](ExplainForgeError(ref, ActionRead, second)); explained == nil || explained.Kind != ForgeErrorNotFound {
		t.Fatalf("NOT_FOUND second = %+v", explained)
	}
}

// GitHub's labels: the public host reads as the forge's name, the number
// as #N, and a scope refusal names gh.
func TestExplainForgeErrorGitHubLabels(t *testing.T) {
	t.Parallel()
	ref := PRReference{Forge: "github", Host: "github.com", Namespace: "o", Repo: "r", Number: 9}
	err := ExplainForgeError(ref, ActionMerge, &forgeapi.ForbiddenError{Status: &forgeapi.StatusError{Status: http.StatusForbidden}, Scope: "repo"})
	want := "Your GitHub token lacks the `repo` scope, so the app cannot merge o/r#9. Create a token with that scope and log gh in with it."
	if err.Error() != want {
		t.Fatalf("sentence = %q\nwant       %q", err.Error(), want)
	}
}
