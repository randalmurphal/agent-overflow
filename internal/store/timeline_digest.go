package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// TimelineDigestContext fixes an inline card to its execution while the agent
// pane remains a continuous transcript. Bounds are exclusive at After and
// Before; completion timestamps are inclusive at CompletedAt.
type TimelineDigestContext struct {
	After        *TimelineCursor `json:"after,omitempty"`
	Before       *TimelineCursor `json:"before,omitempty"`
	StartedAfter *int64          `json:"startedAfter,omitempty"`
	CompletedAt  *int64          `json:"completedAt,omitempty"`
	PromptID     string          `json:"promptId"`
	AnswerID     string          `json:"answerId"`
}

func (d *TimelineDigestContext) filter(alias string) (string, []any) {
	terms := []string{"1=1"}
	var args []any
	if d.After != nil {
		terms = append(terms, "("+alias+"turn_index,"+alias+"item_index) > (?,?)")
		args = append(args, d.After.TurnIndex, d.After.ItemIndex)
	}
	if d.Before != nil {
		terms = append(terms, "("+alias+"turn_index,"+alias+"item_index) < (?,?)")
		args = append(args, d.Before.TurnIndex, d.Before.ItemIndex)
	}
	if d.StartedAfter != nil {
		terms = append(terms, alias+"created_at > ?")
		args = append(args, *d.StartedAfter)
	}
	if d.CompletedAt != nil {
		terms = append(terms, alias+"created_at <= ?")
		args = append(args, *d.CompletedAt)
	}
	return strings.Join(terms, " AND "), args
}
func digestActivityFilter(a string) string {
	return `(` + a + `kind IN ('tool_call','tool_completion','error','api_error') OR (` + a + `kind='notification' AND COALESCE(` + jsonFieldExpr(a+"meta", "$.kind") + `,` + a + `tool_name) IN ('permission_denied','transcript_mirror_degraded')))`
}
func (s *Store) resolveTimelineDigest(q sqlQueryer, threadID string, scope *timelineScope) error {
	anchor, found, err := s.getThreadItem(q, threadID, scope.selection.DigestItemID)
	if err != nil {
		return err
	}
	if !found {
		return ErrTimelineScopeGone
	}
	launch := anchor
	if anchor.Kind == "tool_completion" {
		launch, found, err = s.getThreadItem(q, threadID, anchor.CompletionOf)
		if err != nil {
			return err
		}
		if !found {
			return ErrTimelineScopeGone
		}
	}
	root := launch.ID
	if canonical := transcriptRootFromMeta(launch.Meta); canonical != "" {
		root = canonical
	}
	if launch.Kind != "tool_call" || root != scope.selection.ScopeRootID {
		return fmt.Errorf("digest item does not belong to the selected agent")
	}
	d := &TimelineDigestContext{}
	rounds, err := subagentResumeRounds(q, threadID, []string{root})
	if err != nil {
		return err
	}
	for _, b := range subagentRoundBoundsFor([]string{root}, rounds, map[string]string{launch.ID: root}) {
		if b.anchorID != launch.ID {
			continue
		}
		if b.lo != nil {
			d.After = &TimelineCursor{TurnIndex: b.lo.TurnIndex, ItemIndex: b.lo.ItemIndex - 1}
		}
		if b.hi != nil {
			d.Before = &TimelineCursor{TurnIndex: b.hi.TurnIndex, ItemIndex: b.hi.ItemIndex}
		}
	}
	if anchor.Kind == "tool_completion" {
		var meta struct {
			Start *int `json:"codex_execution_child_start_index"`
			End   *int `json:"codex_execution_child_end_index"`
		}
		if anchor.Meta != "" {
			if err := json.Unmarshal([]byte(anchor.Meta), &meta); err != nil {
				return fmt.Errorf("decode digest execution: %w", err)
			}
		}
		if meta.Start != nil && meta.End != nil {
			d.After = &TimelineCursor{TurnIndex: launch.TurnIndex, ItemIndex: *meta.Start}
			d.Before = &TimelineCursor{TurnIndex: launch.TurnIndex, ItemIndex: *meta.End + 1}
		} else {
			d.CompletedAt = &anchor.CreatedAt
			previous, args := timelineArms(threadID, timelineSelection{Columns: func(string, string) string { return "items.created_at" }, Where: "items.completion_of <> '' AND items.completion_of=? AND (items.turn_index,items.item_index)<(?,?)", WhereArgs: []any{launch.ID, anchor.TurnIndex, anchor.ItemIndex}})
			var started sql.NullInt64
			if err := q.QueryRow("SELECT MAX(created_at) FROM ("+previous+")", args...).Scan(&started); err != nil {
				return err
			}
			if started.Valid {
				d.StartedAfter = &started.Int64
			}
		}
	}
	scope.context.Lifecycle = launch
	scope.context.Completion = nil
	if anchor.Kind == "tool_completion" {
		scope.context.Completion = &anchor
	}
	bounds, values := d.filter("items.")
	pick := func(kind, order string) (string, error) {
		where := "items.parent_id <> '' AND items.parent_id=? AND items.kind=? AND " + bounds
		args := append([]any{root, kind}, values...)
		query, args := timelineArms(threadID, timelineSelection{Columns: timelineIDColumns, Where: where, WhereArgs: args, OrderBy: order, Limit: 1})
		rows, err := q.Query(query, args...)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		var id string
		var turn, index int
		if rows.Next() {
			if err := rows.Scan(&id, &turn, &index); err != nil {
				return "", err
			}
		}
		return id, rows.Err()
	}
	d.PromptID, err = pick("user_text", "turn_index ASC,item_index ASC")
	if err != nil {
		return err
	}
	d.AnswerID, err = pick("assistant_text", "turn_index DESC,item_index DESC")
	if err != nil {
		return err
	}
	scope.digest = d
	scope.context.Digest = d
	return nil
}
