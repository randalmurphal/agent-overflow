package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/forgeapi"
)

// Forge-agnostic CI shapes for the review pane's pipeline surface.
// A CIPipeline groups jobs by "stage": GitLab's literal pipeline stage,
// or the workflow name on GitHub (which has no stage concept). Statuses
// are normalized to the vocabulary below so the frontend renders one
// set of states.

// Normalized CI status vocabulary. Anything unrecognized passes through
// lowercased so new forge states degrade to visible text, not blanks.
const (
	CIStatusSuccess  = "success"
	CIStatusFailed   = "failed"
	CIStatusRunning  = "running"
	CIStatusPending  = "pending"
	CIStatusCanceled = "canceled"
	CIStatusSkipped  = "skipped"
	CIStatusManual   = "manual"
	CIStatusNeutral  = "neutral"
)

type CIPipeline struct {
	// Status is the normalized aggregate for the whole pipeline.
	Status string `json:"status"`
	// URL links to the forge's pipeline page (GitLab only; empty on GitHub).
	URL    string    `json:"url,omitempty"`
	Stages []CIStage `json:"stages"`
}

type CIStage struct {
	// Name is the GitLab stage or GitHub workflow name.
	Name   string  `json:"name"`
	Status string  `json:"status"`
	Jobs   []CIJob `json:"jobs"`
}

type CIJob struct {
	// ID is the forge's numeric job id, as a string. Empty when the job
	// has no fetchable log (external checks, commit statuses).
	ID              string   `json:"id,omitempty"`
	Name            string   `json:"name"`
	Status          string   `json:"status"`
	DurationSeconds float64  `json:"durationSeconds,omitempty"`
	URL             string   `json:"url,omitempty"`
	AllowFailure    bool     `json:"allowFailure,omitempty"`
	LogsAvailable   bool     `json:"logsAvailable"`
	Steps           []CIStep `json:"steps,omitempty"`
}

// CIStep is a per-step status inside a job (GitHub Actions only;
// GitLab has no step concept; its trace marks sections instead, see
// internal/cilog). Number is GitHub's step number, which has gaps: post
// steps are numbered after the steps that did not run. StartedAt and
// CompletedAt are the jobs API's RFC 3339 times, to the second, empty
// until the step starts or completes. Steps run in order, and every line
// of the job log starts with its own RFC 3339 time at 100 ns, so a step's
// lines run from its start to the next step's start. A skipped step
// carries times but writes no lines.
type CIStep struct {
	Number      int    `json:"number"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
}

// ErrCIJobLogNotFound is a job log request the forge answered with HTTP
// 404: the job has not started, or the forge has not published its log.
// GitHub answers it until the job's log blob exists, which is a running
// job's whole run and can be a while after it completed.
var ErrCIJobLogNotFound = errors.New("ci job log not found")

// ErrCIJobLogRangeUnsatisfiable is a resumed job log request (From > 0)
// the forge answered with HTTP 416: it holds no byte at that offset.
var ErrCIJobLogRangeUnsatisfiable = errors.New("ci job log range not satisfiable")

// CIJobLogRequest names one job's log for StreamCIJobLog. From > 0 asks
// for the bytes from there on (Range: bytes=From-), to resume a cut-off
// download; ETag, sent only from the start, is the validator of the log
// the caller holds (If-None-Match). Timeout bounds the whole read, the
// body included; zero is the transport's default.
type CIJobLogRequest struct {
	JobID   string
	ETag    string
	From    int64
	Timeout time.Duration
}

// CIJobLogStart describes a log answer before its body: the status (200,
// or 206 for a range), the ETag and the byte its body starts at.
type CIJobLogStart struct {
	Status int
	ETag   string
	Offset int64
}

// ciLogRequest is the Stream request for a job log at path.
func ciLogRequest(path string, req CIJobLogRequest) forgeapi.Request {
	header := http.Header{}
	switch {
	case req.From > 0:
		header.Set("Range", "bytes="+strconv.FormatInt(req.From, 10)+"-")
	case req.ETag != "":
		header.Set("If-None-Match", req.ETag)
	}
	return forgeapi.Request{Path: path, Header: header, Timeout: req.Timeout}
}

// ciLogSink is a job log Stream destination: told the answer, it asks the
// caller for the writer the body goes to.
type ciLogSink struct {
	begin func(CIJobLogStart) (io.Writer, error)
	dst   io.Writer
}

func (s *ciLogSink) StartStream(status int, header http.Header) error {
	start := CIJobLogStart{Status: status, ETag: header.Get("ETag")}
	if status == http.StatusPartialContent {
		offset, ok := contentRangeStart(header.Get("Content-Range"))
		if !ok {
			return fmt.Errorf("CI log: a 206 without a usable Content-Range %q", header.Get("Content-Range"))
		}
		start.Offset = offset
	}
	dst, err := s.begin(start)
	if err != nil {
		return err
	}
	s.dst = dst
	return nil
}

func (s *ciLogSink) Write(p []byte) (int, error) {
	if s.dst == nil {
		return 0, errors.New("CI log: body before its answer")
	}
	return s.dst.Write(p)
}

// contentRangeStart reads the first byte of "bytes a-b/total".
func contentRangeStart(value string) (int64, bool) {
	rest, ok := strings.CutPrefix(value, "bytes ")
	if !ok {
		return 0, false
	}
	first, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(first, 10, 64)
	return n, err == nil && n >= 0
}

// streamCILog runs a job log request into the caller's writer. A 404 is
// ErrCIJobLogNotFound, a 416 ErrCIJobLogRangeUnsatisfiable; a 304 is
// notModified, the writer never asked for.
func streamCILog(ctx context.Context, client *forgeapi.Client, request forgeapi.Request, begin func(CIJobLogStart) (io.Writer, error)) (bool, error) {
	resp, err := client.Stream(ctx, request, &ciLogSink{begin: begin}, forgeapi.NoLimit)
	if err != nil {
		var statusErr *forgeapi.StatusError
		switch {
		case errors.Is(err, forgeapi.ErrNotFound):
			return false, fmt.Errorf("%w: %w", ErrCIJobLogNotFound, err)
		case errors.As(err, &statusErr) && statusErr.Status == http.StatusRequestedRangeNotSatisfiable:
			return false, fmt.Errorf("%w: %w", ErrCIJobLogRangeUnsatisfiable, err)
		}
		return false, err
	}
	return resp.NotModified, nil
}

var ciJobIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

// ValidateCIJobID enforces that a job id is a plain decimal number
// before it is spliced into an API path.
func ValidateCIJobID(jobID string) error {
	if !ciJobIDPattern.MatchString(jobID) {
		return fmt.Errorf("CI job id must be a decimal number, got %q", jobID)
	}
	return nil
}

// NormalizeCIStatus maps forge-native status/conclusion pairs onto the
// normalized vocabulary. GitHub reports (status, conclusion); GitLab
// reports a single status with an empty conclusion.
func NormalizeCIStatus(status, conclusion string) string {
	switch lowerTrim(conclusion) {
	case "success":
		return CIStatusSuccess
	case "failure", "error", "timed_out", "action_required", "startup_failure":
		return CIStatusFailed
	case "cancelled", "canceled":
		return CIStatusCanceled
	case "skipped", "stale":
		return CIStatusSkipped
	case "neutral":
		return CIStatusNeutral
	}
	switch s := lowerTrim(status); s {
	case "success":
		return CIStatusSuccess
	case "failed", "failure", "error":
		return CIStatusFailed
	case "running", "in_progress":
		return CIStatusRunning
	case "queued", "pending", "created", "waiting_for_resource", "preparing", "scheduled", "requested", "waiting":
		return CIStatusPending
	case "canceled", "cancelled", "canceling":
		return CIStatusCanceled
	case "skipped":
		return CIStatusSkipped
	case "manual":
		return CIStatusManual
	case "completed":
		// GitHub "completed" with no recognizable conclusion.
		return CIStatusNeutral
	default:
		return s
	}
}

// ciStatusSeverity ranks normalized statuses for worst-of aggregation.
func ciStatusSeverity(status string) int {
	switch status {
	case CIStatusFailed:
		return 6
	case CIStatusRunning:
		return 5
	case CIStatusPending:
		return 4
	case CIStatusManual:
		return 3
	case CIStatusCanceled:
		return 2
	case CIStatusSuccess:
		return 1
	case CIStatusSkipped, CIStatusNeutral:
		return 0
	default:
		// Unknown pass-through states rank above success so they stay
		// visible in aggregates.
		return 2
	}
}

// AggregateCIStatus reduces job/stage statuses to the worst one. An
// empty slice aggregates to skipped (a stage of nothing ran nothing).
func AggregateCIStatus(statuses []string) string {
	out := CIStatusSkipped
	best := -1
	for _, status := range statuses {
		if sev := ciStatusSeverity(status); sev > best {
			best = sev
			out = status
		}
	}
	return out
}

func lowerTrim(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ciDurationSeconds computes a job duration from RFC3339 start/end
// timestamps, returning 0 when either is missing or malformed.
func ciDurationSeconds(startedAt, completedAt string) float64 {
	start, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return 0
	}
	end, err := time.Parse(time.RFC3339, completedAt)
	if err != nil {
		return 0
	}
	d := end.Sub(start).Seconds()
	if d < 0 {
		return 0
	}
	return d
}

// CIJobLive reports whether a normalized job status can still change on
// its own: queued or running. Every other status is terminal until the
// pipeline is re-run, which shows up as a new job id.
func CIJobLive(status string) bool {
	return status == CIStatusRunning || status == CIStatusPending
}

// CIPipelineLive reports whether any job of the pipeline is live.
func CIPipelineLive(pipeline CIPipeline) bool {
	for _, stage := range pipeline.Stages {
		for _, job := range stage.Jobs {
			if CIJobLive(job.Status) {
				return true
			}
		}
	}
	return false
}

// FindCIJob returns the job with the given id, or nil.
func FindCIJob(pipeline CIPipeline, jobID string) *CIJob {
	for i := range pipeline.Stages {
		for j := range pipeline.Stages[i].Jobs {
			if pipeline.Stages[i].Jobs[j].ID == jobID {
				return &pipeline.Stages[i].Jobs[j]
			}
		}
	}
	return nil
}
