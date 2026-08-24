package rollout

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-overflow/internal/importir"
	"agent-overflow/internal/provider"
)

type reviewChildCandidate struct {
	id    string
	path  string
	model string
}

type projectedReviewChild struct {
	id     string
	turnID string
	model  string
	events []importir.Event
}

// ProjectReviewChildren joins reviewer detail from Codex's separate review
// rollout into the root review launch. The root file remains the only cursor
// authority. Child source offsets are therefore cleared after projection.
func ProjectReviewChildren(
	ctx context.Context,
	codexHome string,
	parentThreadID string,
	root ParseResult,
) ParseResult {
	launches := reviewLaunches(root.Events)
	if len(launches) == 0 {
		return root
	}
	candidates, err := listReviewChildCandidates(ctx, codexHome)
	if err != nil {
		root.Warnings = append(root.Warnings, importir.Warning{
			Code:    WarnReviewChild,
			Message: "The Code review result was imported, but its internal activity could not be read from Codex's child-thread index.",
		})
		return root
	}

	children := make(map[string]projectedReviewChild, len(candidates))
	childReadFailed := false
	for _, candidate := range candidates {
		child, ok, err := readReviewChild(ctx, codexHome, parentThreadID, candidate)
		if err != nil {
			childReadFailed = true
			continue
		}
		if !ok {
			continue
		}
		children[child.turnID] = child
	}
	if childReadFailed {
		root.Warnings = append(root.Warnings, importir.Warning{
			Code:    WarnReviewChild,
			Message: "The Code review result was imported, but some internal reviewer activity could not be read from its child rollout.",
		})
	}

	for i := len(launches) - 1; i >= 0; i-- {
		launch := launches[i]
		child, ok := children[launch.controlTurnID]
		if !ok {
			continue
		}
		projected := projectReviewChildEvents(child, launch)
		if len(projected) == 0 {
			continue
		}
		root.Events[launch.eventIndex].Meta = setReviewModel(root.Events[launch.eventIndex].Meta, child.model)
		for j := launch.eventIndex + 1; j < len(root.Events); j++ {
			if root.Events[j].ItemID == launch.launchID && root.Events[j].Kind == provider.EventToolComplete {
				root.Events[j].Meta = setReviewModel(root.Events[j].Meta, child.model)
				break
			}
		}
		root.Events = append(root.Events, make([]importir.Event, len(projected))...)
		copy(root.Events[launch.eventIndex+1+len(projected):], root.Events[launch.eventIndex+1:])
		copy(root.Events[launch.eventIndex+1:], projected)
	}
	return root
}

type reviewLaunch struct {
	eventIndex    int
	launchID      string
	outerTurnID   string
	turnIndex     int
	controlTurnID string
}

func reviewLaunches(events []importir.Event) []reviewLaunch {
	var out []reviewLaunch
	for i := range events {
		event := events[i]
		if event.Kind != provider.EventToolStart || event.ItemType != importedCodexReviewToolName {
			continue
		}
		var meta struct {
			Input struct {
				ControlTurnID string `json:"reviewControlTurnId"`
			} `json:"input"`
		}
		if json.Unmarshal(event.Meta, &meta) != nil || strings.TrimSpace(meta.Input.ControlTurnID) == "" {
			continue
		}
		out = append(out, reviewLaunch{
			eventIndex:    i,
			launchID:      event.ItemID,
			outerTurnID:   event.TurnID,
			turnIndex:     event.TurnIndex,
			controlTurnID: strings.TrimSpace(meta.Input.ControlTurnID),
		})
	}
	return out
}

func listReviewChildCandidates(ctx context.Context, codexHome string) ([]reviewChildCandidate, error) {
	home := strings.TrimSpace(codexHome)
	if home == "" {
		return nil, fmt.Errorf("rollout: CodexHome is required")
	}
	dbPath := filepath.Join(home, StateDBName)
	db, err := sql.Open("sqlite", readOnlyDSN(dbPath))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, `
SELECT id, rollout_path, model
  FROM threads
 WHERE json_valid(source)
   AND json_extract(source, '$.subagent') = 'review'
 ORDER BY COALESCE(created_at_ms, created_at * 1000), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reviewChildCandidate
	for rows.Next() {
		var candidate reviewChildCandidate
		var path, model sql.NullString
		if err := rows.Scan(&candidate.id, &path, &model); err != nil {
			return nil, err
		}
		candidate.path = strings.TrimSpace(path.String)
		candidate.model = strings.TrimSpace(model.String)
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func readReviewChild(
	ctx context.Context,
	codexHome string,
	parentThreadID string,
	candidate reviewChildCandidate,
) (projectedReviewChild, bool, error) {
	path, err := PathInHome(codexHome, candidate.path)
	if err != nil {
		return projectedReviewChild{}, false, nil
	}
	meta, err := ReadSessionMeta(path, candidate.id)
	if err != nil || meta.ParentThreadID != parentThreadID || meta.SubagentKind != "review" {
		return projectedReviewChild{}, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return projectedReviewChild{}, false, fmt.Errorf("open review child %s: %w", candidate.id, err)
	}
	defer file.Close()
	parsed, err := Parse(ctx, ParseOptions{File: file, Path: path, SessionID: candidate.id})
	if err != nil {
		return projectedReviewChild{}, false, fmt.Errorf("parse review child %s: %w", candidate.id, err)
	}
	turnID := ""
	for _, event := range parsed.Events {
		if event.Kind == provider.EventTurnStart {
			turnID = strings.TrimSpace(event.TurnID)
			break
		}
	}
	if turnID == "" {
		return projectedReviewChild{}, false, fmt.Errorf("review child %s has no turn", candidate.id)
	}
	model := strings.TrimSpace(parsed.Profile.Model)
	if model == "" {
		model = candidate.model
	}
	return projectedReviewChild{id: candidate.id, turnID: turnID, model: model, events: parsed.Events}, true, nil
}

func projectReviewChildEvents(child projectedReviewChild, launch reviewLaunch) []importir.Event {
	lastAssistant := -1
	for i := range child.events {
		if child.events[i].Kind == provider.EventContentBlockStop && child.events[i].ItemType == "agentMessage" {
			lastAssistant = i
		}
	}
	out := make([]importir.Event, 0, len(child.events))
	for i := range child.events {
		event := child.events[i]
		switch event.Kind {
		case provider.EventTurnStart, provider.EventTurnComplete, provider.EventUserText:
			continue
		}
		if i == lastAssistant {
			// The review child's last answer is raw structured JSON. The root
			// rollout owns the formatted user-facing result.
			continue
		}
		if event.ParentToolUseID == "" {
			event.ParentToolUseID = launch.launchID
		}
		event.TurnID = launch.outerTurnID
		event.TurnIndex = launch.turnIndex
		event.SourceUUID = reviewChildSourceUUID(child.id, event.SourceUUID)
		event.SourceOffset = 0
		if event.Kind == provider.EventError {
			event.Meta = mergeReviewMeta(event.Meta, map[string]any{"fatal": false})
		}
		out = append(out, event)
	}
	return out
}

func setReviewModel(raw json.RawMessage, model string) json.RawMessage {
	model = strings.TrimSpace(model)
	if model == "" {
		return raw
	}
	var meta map[string]any
	if json.Unmarshal(raw, &meta) != nil {
		return raw
	}
	input, _ := meta["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
		meta["input"] = input
	}
	input["model"] = model
	encoded, err := json.Marshal(meta)
	if err != nil {
		return raw
	}
	return encoded
}

func mergeReviewMeta(raw json.RawMessage, values map[string]any) json.RawMessage {
	var meta map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &meta) != nil {
		meta = map[string]any{}
	}
	for key, value := range values {
		meta[key] = value
	}
	encoded, _ := json.Marshal(meta)
	return encoded
}
