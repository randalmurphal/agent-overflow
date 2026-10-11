package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// ciLogDisplayTailBytes caps the log text shipped to the frontend. CI
// traces are read tail-first (the failure is at the bottom), so the cap
// keeps the head off the wire; SavePRCIJobLog writes the full fetch.
const ciLogDisplayTailBytes = 2 * 1024 * 1024

// PRCILogState is one followed job's log as the pump currently holds it.
// Text is the cleaned, tail-capped trace. Available is false while the
// forge cannot serve the log: it has not published the log yet (GitHub
// answers 404 until the job's log blob exists, in practice once the job
// completed). Text is then unchanged and the frontend says why. Error is the caller-safe summary of the last fetch
// failure, with its kind fields (see PRUpdatedEvent).
type PRCILogState struct {
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	TotalBytes int    `json:"totalBytes"`
	Available  bool   `json:"available"`
	Error      string `json:"error"`
	ErrorKind  string `json:"errorKind"`
	Reserve    bool   `json:"reserve"`
	ResumeAt   string `json:"resumeAt"`
	Seq        uint64 `json:"seq"`
}

// PRCILogFollowResult answers SetPRCILogFollows with the current state of
// every job the call asked to follow, keyed by job id.
type PRCILogFollowResult struct {
	Logs map[string]PRCILogState `json:"logs"`
}

// SetPRCILogFollows replaces the set of CI job logs one subscription
// follows. The pump polls every requested job now, so the reply carries
// each job's current log; from then on the pump keeps a live job's log
// moving through "pr:ci_log" frames until the job completes and its final
// log is in hand. An empty list stops following. The call is the manual
// "refresh log" too: re-sending a job id fetches it again.
//
//ao:scope git:operate
//ao:route home
func (a *App) SetPRCILogFollows(ctx context.Context, subscriptionID string, jobIDs []string) (PRCILogFollowResult, error) {
	if a.shuttingDown.Load() {
		return PRCILogFollowResult{}, ErrShuttingDown
	}
	for _, jobID := range jobIDs {
		if err := gitops.ValidateCIJobID(jobID); err != nil {
			return PRCILogFollowResult{}, err
		}
	}
	return a.setPRCILogFollows(ctx, subscriptionID, jobIDs)
}

// RefreshPRCI polls a subscribed pull request's pipeline now. The result
// reaches every subscriber through "pr:ci_updated" when it changed; the
// error is the fetch failure, so the caller can show it on the button
// that asked.
//
//ao:scope git:operate
//ao:route home
func (a *App) RefreshPRCI(ctx context.Context, subscriptionID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}
	return a.refreshPRCI(ctx, subscriptionID)
}

// errCIJobLogUnpublished is SavePRCIJobLog's answer to a forge 404: the
// job has not started, or the forge has not published its log yet.
var errCIJobLogUnpublished = errors.New("the forge has not published this job's log yet")

// SavePRCIJobLog fetches the full job log and writes it under the
// app-managed ci-logs directory, returning the absolute path. The path
// is stable per (pr, job), so a re-save refreshes the same file.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SavePRCIJobLog(ctx context.Context, pr gitops.PRReference, jobID, jobName string) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	if err := pr.Validate(); err != nil {
		return "", err
	}
	if err := gitops.ValidateCIJobID(jobID); err != nil {
		return "", err
	}
	if a.configDir == "" {
		return "", errors.New("app data directory is not initialised")
	}
	log, err := a.gitCore().GetCIJobLog(forgeapi.WithInteractive(ctx), pr, gitops.CIJobLogRequest{JobID: jobID})
	if errors.Is(err, gitops.ErrCIJobLogNotFound) {
		return "", errCIJobLogUnpublished
	}
	if err != nil {
		return "", err
	}
	dir := filepath.Join(a.configDir, "ci-logs")
	if err := ensureAppPrivateDir(dir); err != nil {
		return "", fmt.Errorf("create ci-logs directory: %w", err)
	}
	path := filepath.Join(dir, ciLogFileName(pr, jobID, jobName))
	if err := os.WriteFile(path, []byte(log.Text), 0o600); err != nil {
		return "", fmt.Errorf("write CI log: %w", err)
	}
	return path, nil
}

// ciLogFileName builds a filesystem-safe, per-(pr, job) stable name:
// <forge>-<namespace>-<repo>-pr<N>-<jobID>-<job name>.log
func ciLogFileName(pr gitops.PRReference, jobID, jobName string) string {
	parts := []string{
		pr.Forge,
		sanitizeCIFileSegment(pr.Namespace),
		sanitizeCIFileSegment(pr.Repo),
		fmt.Sprintf("pr%d", pr.Number),
		jobID,
	}
	if segment := sanitizeCIFileSegment(jobName); segment != "" {
		parts = append(parts, segment)
	}
	return strings.Join(parts, "-") + ".log"
}

const ciFileSegmentMaxLen = 60

func sanitizeCIFileSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= ciFileSegmentMaxLen {
			break
		}
	}
	return strings.Trim(b.String(), "-.")
}

// tailCapLog returns the last maxBytes of log, advanced to the next
// line boundary so the tail never starts mid-line.
func tailCapLog(log string, maxBytes int) (string, bool) {
	if len(log) <= maxBytes {
		return log, false
	}
	tail := log[len(log)-maxBytes:]
	if idx := strings.IndexByte(tail, '\n'); idx >= 0 && idx+1 < len(tail) {
		tail = tail[idx+1:]
	}
	return tail, true
}
