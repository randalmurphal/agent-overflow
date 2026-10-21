package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// The kinds of forge failure the PR pump reports (ErrorKind on the wire),
// so a pane chooses its surface without reading the text. See
// docs/architecture/forge-transport.md#failure-presentation.
const (
	// forgeFailureTransient is a forge the next attempt may reach: dial,
	// DNS, TLS, a timeout.
	forgeFailureTransient = "transient"
	// forgeFailureRateLimited is a pool the forge (or the transport's
	// reserve) refuses until ResumeAt.
	forgeFailureRateLimited = "rate_limited"
	// forgeFailureSetup is a login the user fixes outside the app: the CLI
	// is missing or has no token for the host.
	forgeFailureSetup = "setup"
	// forgeFailureForbidden is a forge that refused the login: a token
	// without the scope, or no access to the project. The wire carries the
	// sentence naming which (git.ExplainForgeError).
	forgeFailureForbidden = "forbidden"
	// forgeFailureForge is every other forge answer: a status, a GraphQL
	// error, a body the app could not read.
	forgeFailureForge = "forge"
)

// prUpdateStaggerMax bounds the random delay a pump adds to a rate limit's
// resume time, so the panes of one host do not all fire the moment the
// pool reopens.
const prUpdateStaggerMax = 2 * time.Second

// classifyForgeFailure sorts a forge error by kind. Until is when a
// rate-limited pool may be asked again; reserve marks a background request
// the transport refused to keep the pool's last tenth for the user's own
// actions.
func classifyForgeFailure(err error) (kind string, reserve bool, until time.Time) {
	var limited *forgeapi.RateLimitedError
	var transient *forgeapi.TransientError
	var setup *forgeapi.SetupError
	var forbidden *forgeapi.ForbiddenError
	switch {
	case errors.As(err, &limited):
		return forgeFailureRateLimited, limited.Reserve, limited.Until
	case errors.As(err, &setup):
		return forgeFailureSetup, false, time.Time{}
	case errors.As(err, &forbidden):
		return forgeFailureForbidden, false, time.Time{}
	case errors.As(err, &transient), errors.Is(err, context.DeadlineExceeded):
		return forgeFailureTransient, false, time.Time{}
	}
	return forgeFailureForge, false, time.Time{}
}

// prForgeFailure is one failed forge read as the PR pump holds it. The
// zero value is no failure.
type prForgeFailure struct {
	// key decides whether a failure repeats the one before it: the raw
	// error text, or for a rate limit its kind, reserve and resume time,
	// so a frame goes out when the resume time moves and not every tick.
	// It never reaches the wire.
	key string
	// wire is the caller-safe Error: a summary and the correlation id the
	// server log carries the raw text under, or a setup error's own
	// message, which names the login to fix and nothing else.
	wire     string
	kind     string
	reserve  bool
	resumeAt string // RFC 3339; set only for a rate limit
	// release is when a poller resumes after a rate limit: the pool's
	// resume time plus the stagger. Zero for every other kind.
	release time.Time
	// correlationID ties wire to the server log line.
	correlationID string
}

func (f prForgeFailure) failing() bool { return f.key != "" }

// rateLimited reports whether the failure holds its poller until release.
func (f prForgeFailure) rateLimited() bool { return f.kind == forgeFailureRateLimited }

// newPRForgeFailure classifies err, from a read of pr, and mints its wire
// summary: the correlation summary, or for a setup or forbidden failure
// the sentence naming the login to fix (git.ExplainForgeError), which
// carries no forge text.
func (a *App) newPRForgeFailure(pr gitops.PRReference, err error) prForgeFailure {
	return a.newForgeFailure(err, prUpdateErrorMessage, func(err error) error {
		return gitops.ExplainForgeError(pr, gitops.ActionRead, err)
	})
}

// newRepoForgeFailure is newPRForgeFailure for a read of a whole
// repository (its PR list, a pipeline of it): the setup and forbidden
// sentences name the project, and every other kind reads as a failed
// refresh of the repository.
func (a *App) newRepoForgeFailure(repo gitops.RepoReference, err error) prForgeFailure {
	return a.newForgeFailure(err, repoRefreshErrorMessage, func(err error) error {
		return gitops.ExplainRepoForgeError(repo, gitops.ActionRead, err)
	})
}

// repoRefreshErrorMessage is the repository read's counterpart of
// prUpdateErrorMessage.
func repoRefreshErrorMessage(correlationID string) string {
	return fmt.Sprintf("failed to refresh from the forge (id: %s)", correlationID)
}

// newForgeFailure classifies err with summary as the correlation sentence
// and explain as the sentence of a setup or forbidden failure.
func (a *App) newForgeFailure(err error, summary func(correlationID string) string, explain func(error) error) prForgeFailure {
	kind, reserve, until := classifyForgeFailure(err)
	f := prForgeFailure{key: err.Error(), kind: kind, correlationID: uuid.NewString()}
	f.wire = summary(f.correlationID)
	switch kind {
	case forgeFailureSetup, forgeFailureForbidden:
		f.wire = explain(err).Error()
	case forgeFailureRateLimited:
		f.reserve = reserve
		f.resumeAt = until.UTC().Format(time.RFC3339)
		f.key = fmt.Sprintf("%s\x00%t\x00%s", kind, reserve, f.resumeAt)
		f.release = until.Add(a.prUpdateStagger())
	}
	return f
}

// prUpdateStagger is the random 0 to prUpdateStaggerMax a poller adds to a
// rate limit's resume time.
func (a *App) prUpdateStagger() time.Duration {
	if a.prUpdates.staggerFn != nil {
		return a.prUpdates.staggerFn()
	}
	return rand.N(prUpdateStaggerMax)
}

// prUpdateErrorMessage is the PR-poll failure text that reaches the wire
// for every kind but setup: what went wrong, and the id to grep the
// server log for.
func prUpdateErrorMessage(correlationID string) string {
	return fmt.Sprintf("failed to refresh pull request (id: %s)", correlationID)
}

// heldUntil is how long from now a poller waits for a rate limit's
// release, at least a millisecond so a timer armed with it fires.
func heldUntil(release time.Time) time.Duration {
	return max(time.Until(release), time.Millisecond)
}
