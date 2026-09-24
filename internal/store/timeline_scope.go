package store

import (
	"errors"
	"fmt"
)

// TimelineSelection identifies history independently of its wire projection.
// The zero value selects the main transcript. Tools selects only tool activity
// within an agent transcript, for the background tray. DigestItemID selects
// the launch or completion whose execution an inline card summarizes.
type TimelineSelection struct {
	ScopeRootID  string `json:"scopeRootId,omitempty"`
	Tools        bool   `json:"tools,omitempty"`
	DigestItemID string `json:"digestItemId,omitempty"`
}

var ErrTimelineScopeGone = errors.New("timeline scope no longer exists")

// TimelineScopeContext supplies agent identity independently of the loaded range.
// Lifecycle and Completion refer to the selected digest execution, or the latest
// persisted execution for a continuous scope. Live provider state owns execution.
type TimelineScopeContext struct {
	Root       Item                   `json:"root"`
	Lifecycle  Item                   `json:"lifecycle"`
	Completion *Item                  `json:"completion,omitempty"`
	Digest     *TimelineDigestContext `json:"digest,omitempty"`
}

type timelineScope struct {
	selection TimelineSelection
	context   *TimelineScopeContext
	digest    *TimelineDigestContext
}

func (s *Store) resolveTimelineScope(q sqlQueryer, threadID string, selection TimelineSelection) (timelineScope, error) {
	scope := timelineScope{selection: selection}
	if selection.ScopeRootID == "" {
		if selection.Tools || selection.DigestItemID != "" {
			return scope, fmt.Errorf("timeline selection requires an agent scope")
		}
		return scope, nil
	}
	if len(selection.ScopeRootID) > maxHeldWindowIDBytes || len(selection.DigestItemID) > maxHeldWindowIDBytes {
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
	scope.context = context
	if selection.DigestItemID != "" {
		if selection.Tools {
			return scope, fmt.Errorf("digest and tools selections cannot be combined")
		}
		err := s.resolveTimelineDigest(q, threadID, &scope)
		return scope, err
	}
	carriers, args, err := timelineKeyedIDSelection(q, threadID,
		"items.turn_index AS turn_index, items.item_index AS item_index",
		`items.kind = 'tool_call' AND `+jsonFieldExpr("items.meta", "$.transcript_root_id")+` = ?`, []any{root.ID},
		"turn_index DESC, item_index DESC", 1)
	if err != nil {
		return scope, err
	}
	rows, err := queryHydratedTimelineItems(q, threadID, carriers, args...)
	if err != nil {
		return scope, fmt.Errorf("read scope lifecycle: %w", err)
	}
	if len(rows) > 0 {
		context.Lifecycle = rows[0]
	}
	completions, args, err := timelineKeyedIDSelection(q, threadID,
		"items.turn_index AS turn_index, items.item_index AS item_index",
		`items.completion_of <> '' AND items.completion_of = ?`, []any{context.Lifecycle.ID},
		"turn_index DESC, item_index DESC", 1)
	if err != nil {
		return scope, err
	}
	rows, err = queryHydratedTimelineItems(q, threadID, completions, args...)
	if err != nil {
		return scope, fmt.Errorf("read scope completion: %w", err)
	}
	if len(rows) > 0 {
		context.Completion = &rows[0]
	}
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
	args := []any{scope.selection.ScopeRootID}
	if scope.digest != nil {
		bounds, values := scope.digest.filter(alias)
		filter += " AND " + bounds
		args = append(args, values...)
		filter += " AND (" + digestActivityFilter(alias) + " OR " + alias + "id IN (?,?))"
		args = append(args, scope.digest.PromptID, scope.digest.AnswerID)
	}
	return filter, args
}
