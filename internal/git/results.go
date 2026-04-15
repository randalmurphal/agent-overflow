package git

// GitActionResult summarizes the outcome of a git binding action.
type GitActionResult struct {
	Action  string `json:"action"`
	Branch  string `json:"branch,omitempty"`
	Commit  string `json:"commitSha,omitempty"`
	PRURL   string `json:"prUrl,omitempty"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// GitActionProgress represents a phase update for multi-step git actions.
type GitActionProgress struct {
	Phase   string `json:"phase"`
	Message string `json:"message"`
}
