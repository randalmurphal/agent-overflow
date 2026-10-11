package app

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"time"

	"agent-overflow/internal/cilog"
	gitops "agent-overflow/internal/git"
)

// ciLogAttemptTimeout bounds one download of a job's log. The pump
// downloads on its own goroutine, so this is also how long one log holds
// up the PR's other polls. A GitHub download cut off by it resumes with a
// Range on the next poll; a running GitLab trace ignores Range and has to
// arrive whole in one attempt, so the bound covers GitLab's default job
// log limit of 100 MB at under 1 MB/s.
const ciLogAttemptTimeout = 2 * time.Minute

// errCIJobLogUnpublished answers a person's request for a log the forge
// answered 404 for: the job has not started, or the forge has not
// published its log yet.
var errCIJobLogUnpublished = errors.New("the forge has not published this job's log yet")

// errCILogNotDownloaded answers a read of a log the cache does not hold:
// nobody followed the job, or the cache dropped it.
var errCILogNotDownloaded = errors.New("this job's log is not downloaded; refresh to download it")

// ciLogs is the CI job log cache under <data dir>/ci-logs/cache, opened on
// first use. Saved logs are written beside it, in ci-logs.
func (a *App) ciLogs() (*cilog.Store, error) {
	if a.configDir == "" {
		return nil, errors.New("app data directory is not initialised")
	}
	a.ciLogOnce.Do(func() {
		a.ciLogStore = cilog.Open(filepath.Join(a.configDir, "ci-logs", "cache"), cilog.CacheBytes, log.Printf)
	})
	return a.ciLogStore, nil
}

func ciLogKey(pr gitops.PRReference, jobID string) cilog.Key {
	return cilog.Key{Forge: pr.Forge, Host: pr.Host, Project: pr.Project(), JobID: jobID}
}

// ciLogJob is what a job's log rows are computed from: its name, status
// and steps as the pipeline the pump holds reports them. A job the
// pipeline does not list has none.
func ciLogJob(job *gitops.CIJob) cilog.Job {
	if job == nil {
		return cilog.Job{}
	}
	out := cilog.Job{Name: job.Name, Status: job.Status}
	if len(job.Steps) > 0 {
		out.Steps = make([]cilog.Step, len(job.Steps))
		for i, step := range job.Steps {
			out.Steps[i] = cilog.Step{Number: step.Number, Name: step.Name, Status: step.Status, StartedAt: step.StartedAt, CompletedAt: step.CompletedAt}
		}
	}
	return out
}

// ciLogSource downloads one job's log from its forge for the cache.
func (a *App) ciLogSource(pr gitops.PRReference, jobID string) cilog.Source {
	return func(ctx context.Context, req cilog.Request, begin func(cilog.Start) (io.Writer, error)) (bool, error) {
		forgeReq := gitops.CIJobLogRequest{JobID: jobID, ETag: req.ETag, From: req.From, Timeout: ciLogAttemptTimeout}
		start := func(st gitops.CIJobLogStart) (io.Writer, error) {
			return begin(cilog.Start{Status: st.Status, ETag: st.ETag, Offset: st.Offset})
		}
		if a.prUpdates.ciLogStreamFn != nil {
			return a.prUpdates.ciLogStreamFn(ctx, pr, forgeReq, start)
		}
		return a.gitCore().StreamCIJobLog(ctx, pr, forgeReq, start)
	}
}

// downloadCILog brings a job's log into the cache for a person's request,
// resuming each download the forge cuts off. Every resumed attempt carried
// the download further (a cut that made no progress is Fetch's error), so
// the loop ends with the log, a failure or the caller's cancellation.
func (a *App) downloadCILog(ctx context.Context, store *cilog.Store, pr gitops.PRReference, jobID string) error {
	key := ciLogKey(pr, jobID)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		result, err := store.Fetch(ctx, key, a.ciLogSource(pr, jobID))
		switch {
		case errors.Is(err, gitops.ErrCIJobLogNotFound):
			return errCIJobLogUnpublished
		case err != nil:
			return err
		case !result.Partial:
			return nil
		}
	}
}
