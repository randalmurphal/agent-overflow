package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const threadTurnsListMethod = "thread/turns/list"

type threadTurnIDPage struct {
	IDs        []string
	NextCursor string
}

// threadTurnIDsPage reads turn shells only. Callers use it to validate provider
// history boundaries without serializing any transcript item payloads.
func (s *Session) threadTurnIDsPage(
	ctx context.Context,
	threadID string,
	cursor string,
	limit int,
) (threadTurnIDPage, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return threadTurnIDPage{}, fmt.Errorf("codex: %s: thread id is required", threadTurnsListMethod)
	}
	if limit <= 0 {
		return threadTurnIDPage{}, fmt.Errorf("codex: %s: limit must be positive", threadTurnsListMethod)
	}
	params := map[string]any{
		"threadId":      threadID,
		"limit":         limit,
		"sortDirection": "desc",
		"itemsView":     "notLoaded",
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	resp, err := s.sendRequest(ctx, threadTurnsListMethod, params)
	if err != nil {
		return threadTurnIDPage{}, fmt.Errorf("codex: %s: %w", threadTurnsListMethod, err)
	}
	var decoded struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(resp, &decoded); err != nil {
		return threadTurnIDPage{}, fmt.Errorf("codex: %s: decode response: %w", threadTurnsListMethod, err)
	}
	page := threadTurnIDPage{
		IDs:        make([]string, 0, len(decoded.Data)),
		NextCursor: strings.TrimSpace(decoded.NextCursor),
	}
	for i, turn := range decoded.Data {
		id := strings.TrimSpace(turn.ID)
		if id == "" {
			return threadTurnIDPage{}, fmt.Errorf(
				"codex: %s: response data[%d] is missing id",
				threadTurnsListMethod,
				i,
			)
		}
		page.IDs = append(page.IDs, id)
	}
	return page, nil
}

func (s *Session) newestThreadTurnID(ctx context.Context, threadID string) (string, error) {
	page, err := s.threadTurnIDsPage(ctx, threadID, "", 1)
	if err != nil {
		return "", err
	}
	if len(page.IDs) > 0 {
		return page.IDs[0], nil
	}
	if page.NextCursor != "" {
		return "", fmt.Errorf(
			"codex: %s: thread %s returned an empty first page with a continuation cursor",
			threadTurnsListMethod,
			threadID,
		)
	}
	return "", nil
}
