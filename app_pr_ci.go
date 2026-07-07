package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitops "agent-overflow/internal/git"
)

// ciLogDisplayTailBytes caps the log text shipped to the frontend. CI
// traces are read tail-first (the failure is at the bottom), so the cap
// keeps the head off the wire; SavePRCIJobLog writes the full fetch.
const ciLogDisplayTailBytes = 2 * 1024 * 1024

type PRCIJobLogResult struct {
	Text string `json:"text"`
	// Truncated reports that Text is the tail of a longer log. The full
	// content is available via SavePRCIJobLog.
	Truncated  bool `json:"truncated"`
	TotalBytes int  `json:"totalBytes"`
}

func (a *App) GetPRCIJobs(pr gitops.PRReference) (gitops.CIPipeline, error) {
	if a.shuttingDown.Load() {
		return gitops.CIPipeline{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return gitops.CIPipeline{}, err
	}
	return a.gitCore().ListPRCIJobs("", pr)
}

func (a *App) GetPRCIJobLog(pr gitops.PRReference, jobID string) (PRCIJobLogResult, error) {
	if a.shuttingDown.Load() {
		return PRCIJobLogResult{}, ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return PRCIJobLogResult{}, err
	}
	log, err := a.gitCore().GetCIJobLog("", pr, jobID)
	if err != nil {
		return PRCIJobLogResult{}, err
	}
	tail, truncated := tailCapLog(log, ciLogDisplayTailBytes)
	return PRCIJobLogResult{
		Text:       tail,
		Truncated:  truncated,
		TotalBytes: len(log),
	}, nil
}

// SavePRCIJobLog fetches the full job log and writes it under the
// app-managed ci-logs directory, returning the absolute path. The path
// is stable per (pr, job), so a re-save refreshes the same file.
func (a *App) SavePRCIJobLog(pr gitops.PRReference, jobID, jobName string) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	if err := validatePRReference(pr); err != nil {
		return "", err
	}
	if err := gitops.ValidateCIJobID(jobID); err != nil {
		return "", err
	}
	if a.configDir == "" {
		return "", errors.New("app data directory is not initialised")
	}
	log, err := a.gitCore().GetCIJobLog("", pr, jobID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(a.configDir, "ci-logs")
	if err := ensureAppPrivateDir(dir); err != nil {
		return "", fmt.Errorf("create ci-logs directory: %w", err)
	}
	path := filepath.Join(dir, ciLogFileName(pr, jobID, jobName))
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
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
