package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
)

// ForgeActionError is a forge failure as the user reads it: one sentence
// naming the cause and the fix (Message), its kind for the surface that
// shows it, and the forge's own error underneath for "Details". Every
// forge call the user makes (and the PR pump's reads) passes its error
// through ExplainForgeError before it reaches the wire.
type ForgeActionError struct {
	Kind    string
	Message string
	Err     error
}

func (e *ForgeActionError) Error() string { return e.Message }
func (e *ForgeActionError) Unwrap() error { return e.Err }

// ForgeActionError.Kind values. The first four are the PR pump's failure
// kinds (internal/app/app_forge_failure.go); the rest are refusals of
// one action.
const (
	ForgeErrorTransient   = "transient"
	ForgeErrorRateLimited = "rate_limited"
	ForgeErrorSetup       = "setup"
	ForgeErrorForbidden   = "forbidden"
	ForgeErrorNotFound    = "not_found"
	ForgeErrorStaleHead   = "stale_head"
	ForgeErrorRefused     = "refused"
	ForgeErrorForge       = "forge"
)

// ForgeAction names what the caller was doing, for the sentence.
type ForgeAction string

const (
	ActionRead      ForgeAction = "read"
	ActionApprove   ForgeAction = "approve"
	ActionUnapprove ForgeAction = "unapprove"
	ActionMerge     ForgeAction = "merge"
	ActionAutoMerge ForgeAction = "auto-merge"
	ActionUpdate    ForgeAction = "update the branch of"
	ActionReady     ForgeAction = "mark ready"
	ActionReview    ForgeAction = "review"
	ActionReply     ForgeAction = "reply on"
	ActionResolve   ForgeAction = "resolve a thread on"
)

// ExplainForgeError turns err, from a forge call about ref, into a
// *ForgeActionError. An error that is already one, or nil, is returned
// as is. The sentences are the table in docs/specs/pull-requests.md.
func ExplainForgeError(ref PRReference, action ForgeAction, err error) error {
	if err == nil {
		return nil
	}
	var explained *ForgeActionError
	if errors.As(err, &explained) {
		return err
	}
	labels := forgeLabels(ref.Forge)
	name := fmt.Sprintf("%s%s%d", ref.Project(), labels.numberPrefix, ref.Number)
	wrap := func(kind, message string) error {
		return &ForgeActionError{Kind: kind, Message: message, Err: err}
	}
	var setup *forgeapi.SetupError
	var limited *forgeapi.RateLimitedError
	var transient *forgeapi.TransientError
	var forbidden *forgeapi.ForbiddenError
	var graphQL *forgeapi.GraphQLError
	var status *forgeapi.StatusError
	switch {
	case errors.As(err, &setup):
		return wrap(ForgeErrorSetup, setupSentence(setup, labels))
	case errors.As(err, &limited):
		return wrap(ForgeErrorRateLimited, limited.Error())
	case errors.As(err, &transient), errors.Is(err, context.DeadlineExceeded):
		return wrap(ForgeErrorTransient, fmt.Sprintf("%s could not be reached. Check the connection and try again.", labels.host(ref.Host)))
	case errors.As(err, &forbidden):
		if forbidden.Scope != "" {
			return wrap(ForgeErrorForbidden, fmt.Sprintf("Your %s token lacks the `%s` scope, so the app cannot %s %s. Create a token with that scope and log %s in with it.", labels.host(ref.Host), forbidden.Scope, action, name, labels.binary))
		}
		if forbidden.Message != "" && action != ActionRead {
			return wrap(ForgeErrorRefused, fmt.Sprintf("%s blocks this: %s", labels.name, trimSentence(forbidden.Message)))
		}
		return wrap(ForgeErrorForbidden, fmt.Sprintf("No access to %s on %s with this login.", ref.Project(), labels.host(ref.Host)))
	case errors.As(err, &graphQL):
		return wrap(ForgeErrorRefused, graphQLSentence(graphQL, labels, action, name))
	case errors.As(err, &status):
		return wrap(statusKind(status), statusSentence(status, labels, ref, action, name))
	}
	return wrap(ForgeErrorForge, fmt.Sprintf("%s did not %s %s: %s", labels.name, action, name, trimSentence(err.Error())))
}

type forgeLabelSet struct {
	name         string
	binary       string
	numberPrefix string
	publicHost   string
}

func forgeLabels(forge string) forgeLabelSet {
	if forge == "gitlab" {
		return forgeLabelSet{name: "GitLab", binary: "glab", numberPrefix: "!", publicHost: "gitlab.com"}
	}
	return forgeLabelSet{name: "GitHub", binary: "gh", numberPrefix: "#", publicHost: "github.com"}
}

// host names the forge for the sentence: the forge's name on its public
// host, the host itself elsewhere.
func (l forgeLabelSet) host(host string) string {
	if host == "" || host == l.publicHost {
		return l.name
	}
	return host
}

func setupSentence(setup *forgeapi.SetupError, labels forgeLabelSet) string {
	if !setup.ExpiredAt.IsZero() {
		host := setup.Host
		if host == "" {
			host = labels.publicHost
		}
		return fmt.Sprintf("Your %s token for %s expired on %s. Run `%s auth login --hostname %s` to continue.", labels.name, host, formatExpiry(setup.ExpiredAt), labels.binary, host)
	}
	if setup.Kind == forgeapi.SetupUnauthenticated && setup.Host != "" && setup.Host != labels.publicHost {
		return fmt.Sprintf("Your `%s` login has no token for %s. Run `%s auth login --hostname %s` to continue.", labels.binary, setup.Host, labels.binary, setup.Host)
	}
	return setup.Message
}

func graphQLSentence(graphQL *forgeapi.GraphQLError, labels forgeLabelSet, action ForgeAction, name string) string {
	message := ""
	if len(graphQL.Problems) > 0 {
		message = graphQL.Problems[0].Message
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "approve your own"):
		return "You cannot approve your own pull request."
	case strings.Contains(lower, "head branch was modified") || strings.Contains(lower, "expected head"):
		return "The branch moved after you loaded this pull request. Reload and try again."
	case strings.Contains(lower, "not mergeable") || strings.Contains(lower, "protected branch") || strings.Contains(lower, "required status") || strings.Contains(lower, "review is required"):
		return fmt.Sprintf("%s blocks this merge: %s", labels.name, trimSentence(message))
	case message == "":
		return fmt.Sprintf("%s did not %s %s.", labels.name, action, name)
	}
	return fmt.Sprintf("%s did not %s %s: %s", labels.name, action, name, trimSentence(message))
}

func statusKind(status *forgeapi.StatusError) string {
	switch status.Status {
	case http.StatusNotFound:
		return ForgeErrorNotFound
	case http.StatusConflict:
		return ForgeErrorStaleHead
	case http.StatusMethodNotAllowed, http.StatusUnprocessableEntity, http.StatusNotAcceptable, http.StatusBadRequest, http.StatusUnauthorized:
		return ForgeErrorRefused
	}
	if status.Status >= 500 {
		return ForgeErrorTransient
	}
	return ForgeErrorForge
}

func statusSentence(status *forgeapi.StatusError, labels forgeLabelSet, ref PRReference, action ForgeAction, name string) string {
	reason := trimSentence(forgeBodyMessage(status.Body))
	switch status.Status {
	case http.StatusNotFound:
		return fmt.Sprintf("%s was not found on %s, or this login cannot see it.", name, labels.host(ref.Host))
	case http.StatusConflict:
		return "The branch moved after you loaded this. Reload and try again."
	case http.StatusUnauthorized:
		// A 401 past the token refresh is a refusal of the action (GitLab
		// answers a merge the user may not perform with 401).
		return fmt.Sprintf("%s did not let you %s %s.", labels.name, action, name)
	case http.StatusMethodNotAllowed, http.StatusUnprocessableEntity, http.StatusNotAcceptable:
		if reason == "" {
			return fmt.Sprintf("%s refused to %s %s.", labels.name, action, name)
		}
		return fmt.Sprintf("%s refused to %s %s: %s", labels.name, action, name, reason)
	}
	if status.Status >= 500 {
		return fmt.Sprintf("%s answered with an error (HTTP %d). Try again in a moment.", labels.host(ref.Host), status.Status)
	}
	if reason == "" {
		return fmt.Sprintf("%s did not %s %s (HTTP %d).", labels.name, action, name, status.Status)
	}
	return fmt.Sprintf("%s did not %s %s: %s", labels.name, action, name, reason)
}

// forgeBodyMessage pulls the human text out of a forge error body:
// GitLab's {"message": ...} (a string, or a map of field errors), GitHub's
// {"message": ...}. Anything else answers empty.
func forgeBodyMessage(body string) string {
	body = strings.TrimSpace(body)
	if body == "" || body[0] != '{' {
		return ""
	}
	var raw struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return ""
	}
	switch message := raw.Message.(type) {
	case string:
		return message
	case map[string]any:
		parts := make([]string, 0, len(message))
		for field, value := range message {
			parts = append(parts, fmt.Sprintf("%s %v", field, value))
		}
		return strings.Join(parts, "; ")
	case []any:
		parts := make([]string, 0, len(message))
		for _, value := range message {
			parts = append(parts, fmt.Sprint(value))
		}
		return strings.Join(parts, "; ")
	}
	return raw.Error
}

// trimSentence bounds a forge's text to one line of reasonable length and
// drops a trailing period so the sentence it joins reads once.
func trimSentence(text string) string {
	text = strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if len(text) > 200 {
		text = text[:197] + "..."
	}
	return strings.TrimSuffix(text, ".")
}

// formatExpiry is the date the sentences show.
func formatExpiry(at time.Time) string { return at.Local().Format("Jan 2, 2006") }
