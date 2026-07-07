package git

import (
	"fmt"
	"regexp"
	"strings"
	"time"
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

// maxCILogBytes caps a fetched CI job log/trace. Traces beyond this are
// rare; the runner errors past the cap (same discipline as
// maxPRDiffBytes) and the UI falls back to "open in browser".
const maxCILogBytes = 16 * 1024 * 1024

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

// CIStep is a per-step status inside a job (GitHub Actions only —
// GitLab has no step concept).
type CIStep struct {
	Number int    `json:"number"`
	Name   string `json:"name"`
	Status string `json:"status"`
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
