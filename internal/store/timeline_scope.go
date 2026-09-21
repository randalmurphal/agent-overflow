package store

import (
	"errors"
	"fmt"
)

// TimelineSelection identifies history independently of its wire projection.
// The zero value selects the main transcript. Tools selects only tool activity
// within an agent transcript, for the background tray.
type TimelineSelection struct {
	ScopeRootID string `json:"scopeRootId,omitempty"`
	Tools       bool   `json:"tools,omitempty"`
}

var ErrTimelineScopeGone = errors.New("timeline scope no longer exists")

// TimelineScopeContext supplies agent identity independently of the loaded range.
// Lifecycle and Completion refer to the latest persisted execution; live provider
// state remains authoritative while that execution runs.
type TimelineScopeContext struct {
	Root       Item  `json:"root"`
	Lifecycle  Item  `json:"lifecycle"`
	Completion *Item `json:"completion,omitempty"`
}

type timelineScope struct {
	selection TimelineSelection
	context   *TimelineScopeContext
}

func (s *Store) resolveTimelineScope(q sqlQueryer, threadID string, selection TimelineSelection) (timelineScope, error) {
	scope := timelineScope{selection: selection}
	if selection.ScopeRootID == "" {
		if selection.Tools {
			return scope, fmt.Errorf("tool activity requires an agent scope")
		}
		return scope, nil
	}
	if len(selection.ScopeRootID) > maxHeldWindowIDBytes {
		return scope, fmt.Errorf("timeline scope id is too long")
	}
	root, found, err := s.getThreadItem(q, threadID, selection.ScopeRootID)
	if err != nil {
		return scope, err
	}
	if !found {
		return scope, ErrTimelineScopeGone
	}
	if canonical := transcriptRootFromMeta(root.Meta); canonical != "" && canonical != root.ID {
		root, found, err = s.getThreadItem(q, threadID, canonical)
		if err != nil {
			return scope, err
		}
		if !found {
			return scope, ErrTimelineScopeGone
		}
	}
	// A transcript root may be empty while the provider is starting it.
	// Validate ownership structurally without encoding provider tool names.
	if root.Kind != "tool_call" {
		return scope, fmt.Errorf("timeline scope %q is not a transcript root", root.ID)
	}
	scope.selection.ScopeRootID = root.ID
	context := &TimelineScopeContext{Root: root, Lifecycle: root}
	carriers, args := timelineIDSelection(threadID, timelineSelection{
		Where: `items.kind = 'tool_call' AND ` + jsonFieldExpr("items.meta", "$.transcript_root_id") + ` = ?`, WhereArgs: []any{root.ID},
		OrderBy: "turn_index DESC, item_index DESC", Limit: 1,
	})
	rows, err := queryHydratedTimelineItems(q, threadID, carriers, args...)
	if err != nil {
		return scope, fmt.Errorf("read scope lifecycle: %w", err)
	}
	if len(rows) > 0 {
		context.Lifecycle = rows[0]
	}
	completions, args := timelineIDSelection(threadID, timelineSelection{
		Where: `items.completion_of <> '' AND items.completion_of = ?`, WhereArgs: []any{context.Lifecycle.ID},
		OrderBy: "turn_index DESC, item_index DESC", Limit: 1,
	})
	rows, err = queryHydratedTimelineItems(q, threadID, completions, args...)
	if err != nil {
		return scope, fmt.Errorf("read scope completion: %w", err)
	}
	if len(rows) > 0 {
		context.Completion = &rows[0]
	}
	scope.context = context
	return scope, nil
}

func (scope timelineScope) filter(alias string) (string, []any) {
	filter := visibleItemsFilterFor(alias)
	if scope.selection.ScopeRootID == "" {
		return filter + " AND " + topLevelItemsFilterFor(alias), nil
	}
	filter += " AND " + alias + "parent_id <> '' AND " + alias + "parent_id = ?"
	if scope.selection.Tools {
		filter += " AND " + alias + "kind IN ('tool_call','tool_completion','terminal_interaction')"
	}
	return filter, []any{scope.selection.ScopeRootID}
}
