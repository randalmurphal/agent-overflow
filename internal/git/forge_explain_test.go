package git

import (
	"context"
	"errors"
	"fmt"
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
		{"refused, then the login re-read failed", ActionReview, &forgeapi.RefusalError{Refusal: &forgeapi.StatusError{Method: "POST", Status: http.StatusUnauthorized}, Step: forgeapi.RefusalStepCredential, Err: &forgeapi.TransientError{Err: errors.New("keyring locked")}},
			ForgeErrorSetup, []string{"GitLab refused the `glab` login's token (HTTP 401), so it did not review team/app!42", "keyring locked", "glab auth status --hostname gitlab.example.com"}},
		{"refused, then the re-read found no login", ActionReview, &forgeapi.RefusalError{Refusal: &forgeapi.StatusError{Method: "POST", Status: http.StatusUnauthorized}, Step: forgeapi.RefusalStepCredential, Err: &forgeapi.SetupError{Forge: "gitlab", Binary: "glab", Kind: forgeapi.SetupMissing, Message: "glab is not installed."}},
			ForgeErrorSetup, []string{"glab is not installed."}},
		{"refused, then the login check failed", ActionMerge, &forgeapi.RefusalError{Refusal: &forgeapi.StatusError{Method: "PUT", Status: http.StatusUnauthorized}, Step: forgeapi.RefusalStepProbe, Err: &forgeapi.StatusError{Method: "GET", Status: http.StatusBadGateway}},
			ForgeErrorTransient, []string{"GitLab refused to merge team/app!42 (HTTP 401), so nothing was changed", "checking the `glab` login failed", "Try again in a moment."}},
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

// A repository read names the project, never a PR number of it.
func TestExplainRepoForgeErrorNamesTheProject(t *testing.T) {
	t.Parallel()
	repo := explainRef.Repository()
	cases := []struct {
		err  error
		want string
	}{
		{&forgeapi.StatusError{Method: "GET", Status: http.StatusNotFound}, "team/app was not found on gitlab.example.com, or this login cannot see it."},
		{errors.New("decode body: unexpected EOF"), "GitLab did not read team/app: decode body: unexpected EOF"},
		{&forgeapi.ForbiddenError{Status: &forgeapi.StatusError{Status: http.StatusForbidden}, Scope: "read_api"}, "cannot read team/app."},
	}
	for _, tc := range cases {
		explained, ok := errors.AsType[*ForgeActionError](ExplainRepoForgeError(repo, ActionRead, tc.err))
		if !ok || !strings.Contains(explained.Message, tc.want) || strings.Contains(explained.Message, "!") {
			t.Errorf("ExplainRepoForgeError(%v) = %v, want a sentence containing %q", tc.err, explained, tc.want)
		}
	}
	if err := ExplainRepoForgeError(repo, ActionRead, nil); err != nil {
		t.Fatalf("nil = %v", err)
	}
}

// A forge answer that says the repository does not resolve for this login
// is told from every other, and its sentence names the repository.
func TestRepositoryNotVisible(t *testing.T) {
	t.Parallel()
	repo := RepoReference{Forge: "github", Host: "github.com", Namespace: "example", Repo: "web"}
	search := &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Path: []any{"search"},
		Message: "The listed users and repositories cannot be searched either because the resources do not exist or you do not have permission to view them."}}}
	field := &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Type: "NOT_FOUND", Path: []any{"repository"},
		Message: "Could not resolve to a Repository with the name 'example/web'."}}}
	project := &forgeapi.StatusError{Method: "GET", Status: http.StatusNotFound, Body: `{"message":"404 Project Not Found"}`}
	for _, err := range []error{search, field, project, fmt.Errorf("list: %w", search)} {
		if !RepositoryNotVisible(err) {
			t.Errorf("RepositoryNotVisible(%v) = false", err)
		}
		explained, ok := errors.AsType[*ForgeActionError](ExplainRepoForgeError(repo, ActionRead, err))
		if !ok || explained.Kind != ForgeErrorNotFound || !strings.HasPrefix(explained.Message, "example/web was not found on ") {
			t.Errorf("ExplainRepoForgeError(%v) = %+v, want the not-found sentence naming the repository", err, explained)
		}
	}
	for _, err := range []error{
		&forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Type: "NOT_FOUND", Message: "Could not resolve to a PullRequest with the number of 5."}}},
		&forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Message: "Something went wrong while executing your query."}}},
		&forgeapi.StatusError{Method: "GET", Status: http.StatusNotFound, Body: `{"message":"404 Not Found"}`},
		&forgeapi.StatusError{Method: "GET", Status: http.StatusForbidden, Body: `{"message":"404 Project Not Found"}`},
		errors.New("decode body: unexpected EOF"),
	} {
		if RepositoryNotVisible(err) {
			t.Errorf("RepositoryNotVisible(%v) = true", err)
		}
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

// The token refusals GitHub documents name the token as the cause, on a
// read and on a write alike, as kind forbidden: a token not authorized
// for an organization's single sign-on (with the URL that authorizes it
// when GitHub gave one), a fine-grained token without the permission or
// on a repository it may not write to, an organization's token policy,
// and a GraphQL FORBIDDEN or INSUFFICIENT_SCOPES answer, which used to
// read as a failed refresh retried every tick.
func TestExplainForgeErrorGitHubTokenRefusals(t *testing.T) {
	t.Parallel()
	ref := PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "app", Number: 9}
	const authorize = "https://github.com/orgs/acme/sso?authorization_request=A1"
	samlMessage := "Resource protected by organization SAML enforcement. You must grant your Personal Access token access to this organization."
	status := &forgeapi.StatusError{Status: http.StatusForbidden}
	cases := []struct {
		name     string
		action   ForgeAction
		err      error
		contains []string
	}{
		{"REST SSO with its URL", ActionRead, &forgeapi.ForbiddenError{Status: status, SSO: true, SSOURL: authorize, Message: samlMessage},
			[]string{"not authorized for the single sign-on of the organization that owns acme/app", "Authorize it at " + authorize}},
		{"REST SSO without a URL", ActionApprove, &forgeapi.ForbiddenError{Status: status, SSO: true, Message: samlMessage},
			[]string{"not authorized for the single sign-on", "in your GitHub settings"}},
		{"GraphQL SSO", ActionRead, &forgeapi.GraphQLError{SSO: true, SSOURL: authorize, Problems: []forgeapi.GraphQLProblem{{Type: "FORBIDDEN", Message: samlMessage}}},
			[]string{"single sign-on", authorize}},
		{"GraphQL SAML message without the header", ActionRead, &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Type: "FORBIDDEN", Message: samlMessage}}},
			[]string{"single sign-on"}},
		{"fine-grained write", ActionReview, &forgeapi.ForbiddenError{Status: status, Message: "Resource not accessible by personal access token", Permissions: "pull_requests=write"},
			[]string{"fine-grained", "cannot review acme/app#9", "pull_requests=write", "outside collaborator"}},
		{"fine-grained GraphQL write", ActionMerge, &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Type: "FORBIDDEN", Message: "Resource not accessible by personal access token"}}},
			[]string{"fine-grained", "cannot merge acme/app#9"}},
		{"organization policy", ActionRead, &forgeapi.ForbiddenError{Status: status, Message: "The 'acme' organization forbids access via a personal access tokens (classic)."},
			[]string{"The organization that owns acme/app does not accept this token", "forbids access via", "Log gh in"}},
		{"GraphQL FORBIDDEN read", ActionRead, &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Type: "FORBIDDEN", Message: "Resource not accessible by integration"}}},
			[]string{"No access to acme/app on GitHub with this login."}},
		{"GraphQL scopes", ActionRead, &forgeapi.GraphQLError{Problems: []forgeapi.GraphQLProblem{{Type: "INSUFFICIENT_SCOPES", Message: "Your token has not been granted the required scopes to execute this query. The 'login' field requires one of the following scopes: ['read:org'], but your token has only been granted the: ['repo'] scopes."}}},
			[]string{"lacks the `read:org` scope"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var explained *ForgeActionError
			if !errors.As(ExplainForgeError(ref, tc.action, tc.err), &explained) || explained.Kind != ForgeErrorForbidden {
				t.Fatalf("explained = %+v, want kind %q", explained, ForgeErrorForbidden)
			}
			for _, want := range tc.contains {
				if !strings.Contains(explained.Message, want) {
					t.Errorf("message %q does not contain %q", explained.Message, want)
				}
			}
		})
	}
}
