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
		return wrap(ciPartialSentence(repo, subject, action, partial))
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

// ciPartialSentence words a CI action whose writes were not all
// confirmed: one write that may have been applied, or the runs of a
// GitHub pipeline action that took it, may have, and refused it.
func ciPartialSentence(repo RepoReference, subject string, action ForgeAction, partial *CIPartialError) (string, string) {
	labels := forgeLabels(repo.Forge)
	refused := partial.Total - partial.Done - partial.Unconfirmed
	if partial.Total == 1 {
		return ForgeErrorTransient, fmt.Sprintf("%s did not confirm the request to %s %s, so it may have been applied. The CI view shows what %s did.", labels.name, action, subject, labels.name)
	}
	parts := []string{}
	if partial.Done > 0 {
		parts = append(parts, fmt.Sprintf("took it on %d", partial.Done))
	}
	if partial.Unconfirmed > 0 {
		parts = append(parts, fmt.Sprintf("did not confirm it on %d, which may have taken it", partial.Unconfirmed))
	}
	kind := ForgeErrorTransient
	tail := "."
	if refused > 0 {
		parts = append(parts, fmt.Sprintf("refused it on %d", refused))
		cause, _ := ExplainCIError(repo, subject, action, partial.Err).(*ForgeActionError)
		kind, tail = cause.Kind, ": "+cause.Message
		if !strings.HasSuffix(tail, ".") {
			tail += "."
		}
	}
	return kind, fmt.Sprintf("Of the %d workflow runs of %s asked to %s, %s %s%s", partial.Total, subject, strings.TrimSuffix(string(action), " of"), labels.name, joinClauses(parts), tail)
}

// joinClauses joins clauses as a list: "a", "a and b", "a, b and c".
func joinClauses(parts []string) string {
	if len(parts) <= 1 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// ciActionsUnavailableSentence words a failed read behind the CI action
// flags (CIPipeline.ActionsUnavailable), naming only what it took away:
// retry and cancel for the permission read (permErr), artifact downloads
// for an artifacts read (artErr), all three when both failed. someArtifacts
// says a download is still offered, from the runs whose artifacts were
// read: the sentence then covers the others only.
func ciActionsUnavailableSentence(repo RepoReference, permErr, artErr error, someArtifacts bool) string {
	labels := forgeLabels(repo.Forge)
	host := labels.host(repo.Host)
	downloads := "artifact downloads"
	if someArtifacts {
		downloads = "some artifact downloads"
	}
	what, read, err := "Retry, cancel and "+downloads+" are", "the repository permissions or Actions artifacts", permErr
	switch {
	case artErr == nil:
		what, read = "Retry and cancel are", "the repository permissions"
	case permErr == nil:
		what, read, err = capitalize(downloads)+" are", "the Actions artifacts", artErr
	}
	if forbidden, ok := errors.AsType[*forgeapi.ForbiddenError](err); ok {
		if forbidden.Scope != "" {
			return fmt.Sprintf("%s not shown: your %s token lacks the `%s` scope. Create a token with that scope and log %s in with it.", what, host, forbidden.Scope, labels.binary)
		}
		return fmt.Sprintf("%s not shown: this login cannot read %s of %s on %s.", what, read, repo.Project(), host)
	}
	if errors.Is(err, forgeapi.ErrNotFound) {
		return fmt.Sprintf("%s not shown: this login cannot read %s of %s on %s.", what, read, repo.Project(), host)
	}
	explained, _ := explainForgeError(repo, repo.Project(), ActionRead, err).(*ForgeActionError)
	return what + " not shown for now. " + explained.Message
}

// ciNoLongerSentence words GitLab's refusal of a job it no longer retries
// (403 "Job is not retryable") or plays (400 "Unplayable Job"), and
// GitHub's of a job or run it cannot re-run, from the forge's message;
// "" for any other message.
func ciNoLongerSentence(action ForgeAction, subject, message string) string {
	lower := strings.ToLower(message)
	switch {
	case (action == ActionRetryJob || action == ActionRetryFailed) && ciTooOldMessage(lower):
		return fmt.Sprintf("%s is more than 30 days old, so it can no longer be retried. Start a new run on the forge.", capitalize(subject))
	case (action == ActionRetryJob || action == ActionRetryFailed) && (strings.Contains(lower, "not retryable") || strings.Contains(lower, "cannot be retried") || strings.Contains(lower, "cannot be rerun") || strings.Contains(lower, "cannot be re-run")):
		return fmt.Sprintf("%s can no longer be retried. Refresh to see its current state.", capitalize(subject))
	case action == ActionPlayJob && strings.Contains(lower, "unplayable"):
		return fmt.Sprintf("%s can no longer be run. Refresh to see its current state.", capitalize(subject))
	}
	return ""
}

// ciTooOldMessage reports GitHub's refusal to re-run a workflow run past
// its 30-day window (githubRerunWindow), as it words it: the run "was
// created over a month ago". The wording is matched loosely, since
// GitHub's documentation states the limit but not the message.
func ciTooOldMessage(lower string) bool {
	return strings.Contains(lower, "over a month ago") || strings.Contains(lower, "30 days") || (strings.Contains(lower, "created") && strings.Contains(lower, "month"))
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
