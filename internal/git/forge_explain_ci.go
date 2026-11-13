package git

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"agent-overflow/internal/forgeapi"
)

// ExplainCIError is ExplainForgeError for a CI action or artifacts
// download on repo; subject is what the sentences call the job or
// pipeline ("job lint of grp/tool"). Beside the shared sentences it words
// what only CI actions meet: a pipeline action that some GitHub runs took
// and others refused, a job the forge no longer retries or plays, a run
// GitHub cannot cancel (409, which elsewhere means a moved head), and an
// artifact that is gone.
func ExplainCIError(repo RepoReference, subject string, action ForgeAction, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*ForgeActionError](err); ok {
		return err
	}
	labels := forgeLabels(repo.Forge)
	wrap := func(kind, message string) error {
		return &ForgeActionError{Kind: kind, Message: message, Err: err}
	}
	if partial, ok := errors.AsType[*CIPartialError](err); ok {
		cause, _ := ExplainCIError(repo, subject, action, partial.Err).(*ForgeActionError)
		return wrap(cause.Kind, fmt.Sprintf("%s acted on %d of %d workflow runs of %s. The others failed: %s", labels.name, partial.Done, partial.Total, subject, cause.Message))
	}
	switch {
	case errors.Is(err, forgeapi.ErrNotFound) && !errors.Is(err, ErrNoCIArtifacts):
		return wrap(ForgeErrorNotFound, fmt.Sprintf("%s was not found on %s, or this login cannot see it.", capitalize(subject), labels.host(repo.Host)))
	case errors.Is(err, ErrNoCIArtifacts):
		return wrap(ForgeErrorNotFound, fmt.Sprintf("%s has no artifacts to download on %s.", capitalize(subject), labels.host(repo.Host)))
	case errors.Is(err, ErrCIArtifactExpired):
		return wrap(ForgeErrorNotFound, fmt.Sprintf("The artifacts of %s have expired on %s.", subject, labels.host(repo.Host)))
	}
	if forbidden, ok := errors.AsType[*forgeapi.ForbiddenError](err); ok && forbidden.Scope == "" {
		if sentence := ciNoLongerSentence(action, subject, forbidden.Message); sentence != "" {
			return wrap(ForgeErrorRefused, sentence)
		}
	}
	if status, ok := errors.AsType[*forgeapi.StatusError](err); ok {
		reason := trimSentence(forgeBodyMessage(status.Body))
		if sentence := ciNoLongerSentence(action, subject, reason); sentence != "" {
			return wrap(ForgeErrorRefused, sentence)
		}
		if status.Status == http.StatusConflict {
			if reason == "" {
				return wrap(ForgeErrorRefused, fmt.Sprintf("%s refused to %s %s. Refresh to see its current state.", labels.name, action, subject))
			}
			return wrap(ForgeErrorRefused, fmt.Sprintf("%s refused to %s %s: %s", labels.name, action, subject, reason))
		}
	}
	return explainForgeError(repo, subject, action, err)
}

// ciNoLongerSentence words GitLab's refusal of a job it no longer retries
// (403 "Job is not retryable") or plays (400 "Unplayable Job"), and
// GitHub's of a job or run it cannot re-run, from the forge's message;
// "" for any other message.
func ciNoLongerSentence(action ForgeAction, subject, message string) string {
	lower := strings.ToLower(message)
	switch {
	case (action == ActionRetryJob || action == ActionRetryFailed) && (strings.Contains(lower, "not retryable") || strings.Contains(lower, "cannot be retried") || strings.Contains(lower, "cannot be rerun") || strings.Contains(lower, "cannot be re-run")):
		return fmt.Sprintf("%s can no longer be retried. Refresh to see its current state.", capitalize(subject))
	case action == ActionPlayJob && strings.Contains(lower, "unplayable"):
		return fmt.Sprintf("%s can no longer be run. Refresh to see its current state.", capitalize(subject))
	}
	return ""
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// CIJobSubject names a job in the CI sentences: "job lint of grp/tool".
func CIJobSubject(repo RepoReference, name string) string {
	return fmt.Sprintf("job %s of %s", name, repo.Project())
}

// CIPipelineSubject names a pipeline in the CI sentences: "pipeline 42 of
// grp/tool", or, for a GitHub PR's head checks (which have no single id),
// "the checks of o/r#7".
func CIPipelineSubject(repo RepoReference, id string, pr *PRReference) string {
	if id == "" && pr != nil {
		return fmt.Sprintf("the checks of %s%s%d", pr.Project(), forgeLabels(pr.Forge).numberPrefix, pr.Number)
	}
	return fmt.Sprintf("pipeline %s of %s", id, repo.Project())
}
