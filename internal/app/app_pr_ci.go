package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/cilog"
	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
)

// PRCILogState is one followed job's log as the pump currently holds it:
// the index of the version in the CI log cache, never its text, which
// GetPRCILogLines reads a window at a time. A reader holding lines of
// version Version-1 keeps the first StableLines of them and reads the rest
// again; lines of any other version are read again. Sections are the
// job's rows (GitHub steps, GitLab sections) with their line ranges; a
// GitHub job's steps are listed before its log exists. Available is false
// while the forge cannot serve the log: it has not published it yet
// (GitHub answers 404 until the job's log blob exists, in practice once
// the job completed); the version held from before, if any, stays
// readable. Downloading is a download the forge cut off, resumed on the
// next poll. Error is the caller-safe summary of the last failure, with
// its kind fields (see PRUpdatedEvent).
type PRCILogState struct {
	Version     uint64          `json:"version"`
	TotalLines  int             `json:"totalLines"`
	TotalBytes  int64           `json:"totalBytes"`
	StableLines int             `json:"stableLines"`
	Sections    []cilog.Section `json:"sections"`
	Available   bool            `json:"available"`
	Downloading bool            `json:"downloading"`
	Error       string          `json:"error"`
	ErrorKind   string          `json:"errorKind"`
	Reserve     bool            `json:"reserve"`
	ResumeAt    string          `json:"resumeAt"`
	Seq         uint64          `json:"seq"`
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

// GetPRCILogLines bounds one read of a cached log: enough lines for
// several of the frontend's blocks, and a megabyte of text, so a window
// of very long lines stays a bounded reply. A line is shown up to
// maxCILogLineBytes: a block of the frontend's lines then holds at most
// that many times its line count, and a longer line (a minified bundle
// echoed into a log) is whole in Save to file.
const (
	maxCILogLinesPerRead = 2000
	maxCILogBytesPerRead = 1 << 20
	maxCILogLineBytes    = 64 << 10
)

// GetPRCILogLines reads up to count lines of a job's cached log from line
// fromLine, within maxCILogBytesPerRead of text: a read stops before a
// line that would pass it. A line longer than maxCILogLineBytes is
// clipped to it and listed in Clipped. The reply names the version it read; lines of another version
// than the index the caller holds are stale. The log is in the cache
// while somebody follows the job (SetPRCILogFollows, SetPipelineFollows).
// A log is the repository's, whichever PR or pipeline showed it.
//
//ao:scope git:operate
//ao:route selected
func (a *App) GetPRCILogLines(repo gitops.RepoReference, jobID string, fromLine, count int) (cilog.Lines, error) {
	if a.shuttingDown.Load() {
		return cilog.Lines{}, ErrShuttingDown
	}
	if err := repo.Validate(); err != nil {
		return cilog.Lines{}, err
	}
	if err := gitops.ValidateCIJobID(jobID); err != nil {
		return cilog.Lines{}, err
	}
	if fromLine < 0 {
		return cilog.Lines{}, fmt.Errorf("CI log line %d is before the first line", fromLine)
	}
	if count < 1 || count > maxCILogLinesPerRead {
		return cilog.Lines{}, fmt.Errorf("CI log read of %d lines is outside 1 to %d", count, maxCILogLinesPerRead)
	}
	store, err := a.ciLogs()
	if err != nil {
		return cilog.Lines{}, err
	}
	lines, err := store.Lines(ciLogKey(repo, jobID), fromLine, count, maxCILogBytesPerRead, maxCILogLineBytes)
	if errors.Is(err, cilog.ErrNoLog) {
		return cilog.Lines{}, errCILogNotDownloaded
	}
	return lines, err
}

// SavePRCIJobLog writes a job's whole log, as the log view shows it,
// under the app-managed ci-logs directory and returns the absolute path.
// The file is a copy of the cached log, downloaded first when the cache
// does not hold it. The path is stable per (repository, job), so a re-save
// refreshes the same file.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SavePRCIJobLog(ctx context.Context, repo gitops.RepoReference, jobID, jobName string) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	if err := repo.Validate(); err != nil {
		return "", err
	}
	if err := gitops.ValidateCIJobID(jobID); err != nil {
		return "", err
	}
	store, err := a.ciLogs()
	if err != nil {
		return "", err
	}
	key := ciLogKey(repo, jobID)
	store.Pin(key)
	defer store.Unpin(key)
	name := ciLogFileName(repo, jobID, jobName)
	for attempt := 0; ; attempt++ {
		if !store.Has(key) {
			if err := a.downloadCILog(forgeapi.WithInteractive(ctx), store, repo, jobID); err != nil {
				return "", err
			}
		}
		path, err := a.writeCILogFile(name, func(w io.Writer) error {
			_, err := store.CopyLog(key, w)
			return err
		})
		// A file that went from under the cache was dropped by the read:
		// download it again once.
		if errors.Is(err, cilog.ErrLogGone) && attempt == 0 {
			continue
		}
		return path, err
	}
}

// SavePRCIJobLogSection writes one row of a job's cached log (a GitHub
// step or a GitLab section, by the key its index names) under the ci-logs
// directory, as plain text without escape sequences, and returns the
// absolute path. The rows are the ones the
// follow's last index computed. The path is stable per (pr, job,
// section), so a re-save refreshes the same file.
//
//ao:scope git:operate
//ao:route selected
func (a *App) SavePRCIJobLogSection(repo gitops.RepoReference, jobID, jobName, sectionKey string) (string, error) {
	if a.shuttingDown.Load() {
		return "", ErrShuttingDown
	}
	if err := repo.Validate(); err != nil {
		return "", err
	}
	if err := gitops.ValidateCIJobID(jobID); err != nil {
		return "", err
	}
	if sectionKey == "" {
		return "", errors.New("CI log section key is empty")
	}
	store, err := a.ciLogs()
	if err != nil {
		return "", err
	}
	key := ciLogKey(repo, jobID)
	var section cilog.Section
	stem := strings.TrimSuffix(ciLogFileName(repo, jobID, jobName), ".log")
	// The name comes from the row the copy finds, so the copy goes to a
	// temp file first and is renamed once the row is known.
	path, err := a.writeCILogFileNamed(func(w io.Writer) (string, error) {
		plain := cilog.NewPlainWriter(w)
		var err error
		section, err = store.CopySection(key, sectionKey, plain)
		if err == nil {
			err = plain.Close()
		}
		if err != nil {
			return "", err
		}
		name := stem
		if segment := sanitizeCIFileSegment(section.Name); segment != "" {
			name += "-" + segment
		}
		return name + ".log", nil
	})
	if errors.Is(err, cilog.ErrNoLog) {
		return "", errCILogNotDownloaded
	}
	return path, err
}

// writeCILogFile writes a saved log under the ci-logs directory through a
// temp file renamed into place, so a failed write never leaves half a log
// under the name.
func (a *App) writeCILogFile(name string, write func(io.Writer) error) (string, error) {
	return a.writeCILogFileNamed(func(w io.Writer) (string, error) {
		return name, write(w)
	})
}

// writeCILogFileNamed is writeCILogFile for a writer that learns the
// file's name as it writes.
func (a *App) writeCILogFileNamed(write func(io.Writer) (string, error)) (string, error) {
	if a.configDir == "" {
		return "", errors.New("app data directory is not initialised")
	}
	dir := filepath.Join(a.configDir, "ci-logs")
	if err := ensureAppPrivateDir(dir); err != nil {
		return "", fmt.Errorf("create ci-logs directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".save-*")
	if err != nil {
		return "", fmt.Errorf("write CI log: %w", err)
	}
	name, err := write(temp)
	if closeErr := temp.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("write CI log: %w", closeErr)
	}
	path := filepath.Join(dir, name)
	if err == nil {
		if err = os.Rename(temp.Name(), path); err != nil {
			err = fmt.Errorf("write CI log: %w", err)
		}
	}
	if err != nil {
		if removeErr := os.Remove(temp.Name()); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			log.Printf("ci logs: remove unfinished saved log: %v", removeErr)
		}
		return "", err
	}
	return path, nil
}

// ciLogFileName builds a filesystem-safe, per-(repository, job) stable
// name: <forge>-<namespace>-<repo>-<jobID>-<job name>.log. A job id is
// unique in its repository, so no PR or pipeline number is needed.
func ciLogFileName(repo gitops.RepoReference, jobID, jobName string) string {
	parts := []string{
		repo.Forge,
		sanitizeCIFileSegment(repo.Namespace),
		sanitizeCIFileSegment(repo.Repo),
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
