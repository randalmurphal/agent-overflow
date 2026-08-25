package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const codexReviewToolName = "codex_review"

type reviewAgentSnapshot struct {
	itemID string
	text   string
}

// reviewRun correlates the two Codex turn identities and the item snapshots
// that review mode does not expose through the normal delta/completion pair.
// One Session can run at most one review because Codex permits one active turn.
type reviewRun struct {
	turnIndex int
	target    ReviewTarget
	model     string

	outerTurnID   string
	controlTurnID string
	launchID      string
	entered       bool
	exited        bool

	pendingAgent    *reviewAgentSnapshot
	formattedResult string
	fallbackResult  string
	errorMessage    string
	responseBound   bool
	completed       bool
}

func (s *Session) reserveReview(turnIndex int, target ReviewTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Closed-session check MUST precede the busy reads AND run under s.mu:
	// Close zeroes s.turn and s.review, so a post-Close call would read
	// "idle", write a fresh reviewRun onto the closed session, and proceed
	// into a request on a dead pipe (see ErrSessionClosed). Under mu it is
	// ordered against the zeroing; before Lock it leaves a preemption
	// window.
	if s.closing.Load() {
		return fmt.Errorf("codex: %s: %w", reviewStartMethod, ErrSessionClosed)
	}
	if s.review != nil {
		return fmt.Errorf("codex: %s: a review is already running", reviewStartMethod)
	}
	if s.turn.activeTurnID != "" {
		return fmt.Errorf("codex: %s: thread already has an active turn", reviewStartMethod)
	}
	s.review = &reviewRun{turnIndex: turnIndex, target: target}
	return nil
}

func (s *Session) releaseReservedReview() {
	s.mu.Lock()
	s.review = nil
	s.mu.Unlock()
}

func (s *Session) setReservedReviewModel(model string) {
	s.mu.Lock()
	if s.review != nil {
		s.review.model = strings.TrimSpace(model)
	}
	s.mu.Unlock()
}

func (s *Session) bindReviewResponse(turnID string) error {
	turnID = strings.TrimSpace(turnID)
	if turnID == "" {
		return fmt.Errorf("codex: %s: response carried no turn id", reviewStartMethod)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.review == nil {
		return fmt.Errorf("codex: %s: review reservation disappeared", reviewStartMethod)
	}
	if s.review.outerTurnID != "" && s.review.outerTurnID != turnID {
		return fmt.Errorf(
			"codex: %s: response turn %s disagrees with entered review turn %s",
			reviewStartMethod, turnID, s.review.outerTurnID,
		)
	}
	s.review.outerTurnID = turnID
	s.review.responseBound = true
	if s.review.completed {
		s.review = nil
	}
	return nil
}

func (s *Session) effectiveReviewModel(ctx context.Context) (string, error) {
	raw, err := s.sendRequest(ctx, "config/read", map[string]any{
		"cwd":           s.workDir,
		"includeLayers": false,
	})
	if err != nil {
		return "", fmt.Errorf("codex: config/read for review model: %w", err)
	}
	var response struct {
		Config struct {
			ReviewModel string `json:"review_model"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("codex: decode config/read for review model: %w", err)
	}
	if model := strings.TrimSpace(response.Config.ReviewModel); model != "" {
		return model, nil
	}
	s.mu.Lock()
	model := strings.TrimSpace(s.turnConfig.model)
	s.mu.Unlock()
	return model, nil
}
