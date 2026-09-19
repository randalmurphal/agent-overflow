package threadtools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
)

// thread_update and thread_group: organizing through the same bindings the
// sidebar calls, so every change shows there live.
//
// The whole patch is validated before any thread is touched, and a
// refusal is per id: one thread that cannot take the patch does not stop
// the other forty-nine, and one unreachable computer fails only its own
// ids.

type updateArgs struct {
	ThreadIDs []string        `json:"thread_ids"`
	Title     *string         `json:"title"`
	Archived  *bool           `json:"archived"`
	Pin       *string         `json:"pin"`
	Group     json.RawMessage `json:"group"`
}

type updateResult struct {
	Results []ThreadUpdateResult `json:"results"`
	Note    string               `json:"note,omitempty"`
}

func (c *session) update(ctx context.Context, raw json.RawMessage) (any, error) {
	var args updateArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	patch, err := updatePatch(args)
	if err != nil {
		return nil, err
	}
	if len(args.ThreadIDs) == 0 || len(args.ThreadIDs) > MaxUpdateThreads {
		return nil, invalidf("thread_ids must name 1 to %d threads.", MaxUpdateThreads)
	}

	results := make([]ThreadUpdateResult, len(args.ThreadIDs))
	byComputer := map[string][]int{}
	computers := map[string]Computer{}
	for index, ref := range args.ThreadIDs {
		target, err := c.resolve(ctx, ref, "")
		if err != nil {
			code, message := publicMessage(err)
			results[index] = ThreadUpdateResult{ThreadID: trim(ref), Error: message, ErrorCode: code}
			continue
		}
		id, name := c.stamp(Computer{ID: target.ComputerID, Name: target.Computer})
		results[index] = ThreadUpdateResult{ThreadID: target.ThreadID, ComputerID: id, Computer: name, Title: target.Title}
		if target.ThreadID == c.caller.ThreadID && patch.Archived != nil && *patch.Archived {
			results[index].Error = "A thread cannot archive itself."
			results[index].ErrorCode = CodeIsCaller
			continue
		}
		key := target.ComputerID
		if target.Local {
			key = ""
		}
		byComputer[key] = append(byComputer[key], index)
		computers[key] = Computer{ID: target.ComputerID, Name: target.Computer}
	}

	for key, indexes := range byComputer {
		ids := make([]string, 0, len(indexes))
		for _, index := range indexes {
			ids = append(ids, results[index].ThreadID)
		}
		call := patch
		call.ThreadIDs = ids
		applied, err := c.applyUpdate(ctx, key, computers[key], call)
		if err != nil {
			code, message := publicMessage(err)
			for _, index := range indexes {
				results[index].Error, results[index].ErrorCode = message, code
			}
			continue
		}
		byID := make(map[string]ThreadUpdateResult, len(applied))
		for _, row := range applied {
			byID[row.ThreadID] = row
		}
		for _, index := range indexes {
			row, ok := byID[results[index].ThreadID]
			if !ok {
				results[index].Error = "That computer did not report a result for this thread."
				results[index].ErrorCode = CodeUnreachable
				continue
			}
			results[index].Updated, results[index].Error, results[index].ErrorCode = row.Updated, row.Error, row.ErrorCode
			if row.Title != "" {
				results[index].Title = row.Title
			}
		}
	}

	result := updateResult{Results: results}
	for _, row := range results {
		if row.Error != "" {
			result.Note = "One or more threads were refused and left untouched; each says why. The others were updated."
			break
		}
	}
	return result, nil
}

// updatePatch validates the whole patch once. group and pin together are
// refused here rather than per thread: a group carries the pin, so the two
// are contradictory whatever thread they are aimed at.
func updatePatch(args updateArgs) (UpdateCall, error) {
	patch := UpdateCall{Archived: args.Archived}
	if args.Title != nil {
		title := trim(*args.Title)
		if title == "" {
			return UpdateCall{}, invalidf("title is trimmed, and an empty title is refused. Omit title to leave it alone.")
		}
		if utf8.RuneCountInString(title) > MaxTitleRunes {
			return UpdateCall{}, invalidf("title must be at most %d characters.", MaxTitleRunes)
		}
		patch.Title = &title
	}
	if args.Pin != nil {
		pin := trim(*args.Pin)
		if !slices.Contains([]string{PinFront, PinBack, PinNone}, pin) {
			return UpdateCall{}, invalidf("pin must be front, back or none.")
		}
		patch.Pin = &pin
	}
	if len(args.Group) > 0 {
		group, err := decodeNullableString(args.Group, "group")
		if err != nil {
			return UpdateCall{}, err
		}
		patch.Group = group
	}
	if patch.Group != nil && patch.Pin != nil && *patch.Pin != PinNone {
		return UpdateCall{}, publicf(CodeGrouped, "A grouped thread cannot carry its own pin, because its group carries it. Set group or pin in one call, not both, and pin the group itself with thread_group.")
	}
	if !patch.Patched() {
		return UpdateCall{}, invalidf("Set at least one of title, archived, pin or group.")
	}
	return patch, nil
}

// decodeNullableString tells an explicit null apart from an absent field.
// For group they mean different things: null ungroups, absent leaves the
// group alone.
func decodeNullableString(raw json.RawMessage, field string) (*string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "null" {
		empty := ""
		return &empty, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, invalidf("%s must be a string or null.", field)
	}
	value = trim(value)
	if value == "" {
		return nil, invalidf("%s cannot be blank. Pass null to ungroup.", field)
	}
	if utf8.RuneCountInString(value) > MaxTitleRunes {
		return nil, invalidf("%s must be at most %d characters.", field, MaxTitleRunes)
	}
	return &value, nil
}

func (c *session) applyUpdate(ctx context.Context, key string, computer Computer, call UpdateCall) ([]ThreadUpdateResult, error) {
	if key == "" {
		report, err := c.app.UpdateThreads(ctx, c.caller, call)
		if err != nil {
			return nil, err
		}
		return report.Results, nil
	}
	peer, err := c.app.Peer(ctx, computer.ID)
	if err != nil {
		return nil, err
	}
	raw, err := marshalUpdate(call)
	if err != nil {
		return nil, err
	}
	answer, err := peer.Invoke(ctx, "thread_update", raw)
	if err != nil {
		return nil, err
	}
	var result updateResult
	if err := peerResult(answer, &result); err != nil {
		return nil, err
	}
	return result.Results, nil
}

func marshalUpdate(call UpdateCall) (json.RawMessage, error) {
	fields := map[string]any{"thread_ids": call.ThreadIDs}
	if call.Title != nil {
		fields["title"] = *call.Title
	}
	if call.Archived != nil {
		fields["archived"] = *call.Archived
	}
	if call.Pin != nil {
		fields["pin"] = *call.Pin
	}
	if call.Group != nil {
		if *call.Group == "" {
			fields["group"] = nil
		} else {
			fields["group"] = *call.Group
		}
	}
	return json.Marshal(fields)
}

type groupArgs struct {
	Group      string `json:"group"`
	GroupID    string `json:"group_id"`
	ProjectID  string `json:"project_id"`
	ComputerID string `json:"computer_id"`
	Rename     string `json:"rename"`
	Pin        string `json:"pin"`
	Delete     bool   `json:"delete"`
}

type groupResult struct {
	GroupReport
	Note string `json:"note,omitempty"`
}

func (c *session) group(ctx context.Context, raw json.RawMessage) (any, error) {
	var args groupArgs
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := c.checkComputerArg(args.ComputerID); err != nil {
		return nil, err
	}
	call := GroupCall{
		Group:     trim(args.Group),
		GroupID:   trim(args.GroupID),
		ProjectID: trim(args.ProjectID),
		Rename:    trim(args.Rename),
		Pin:       trim(args.Pin),
		Delete:    args.Delete,
	}
	if count := exactlyOne(call.Group != "", call.GroupID != ""); count != 1 {
		return nil, invalidf("Name the group with group plus project_id, or with group_id. This call passed %d of the two.", count)
	}
	if count := exactlyOne(call.Rename != "", call.Pin != "", call.Delete); count != 1 {
		return nil, invalidf("Pass exactly one of rename, pin or delete. This call passed %d.", count)
	}
	if call.Rename != "" && utf8.RuneCountInString(call.Rename) > MaxTitleRunes {
		return nil, invalidf("rename must be at most %d characters.", MaxTitleRunes)
	}
	if call.Pin != "" && !slices.Contains([]string{PinFront, PinBack, PinNone}, call.Pin) {
		return nil, invalidf("pin must be front, back or none.")
	}

	computer, local, ok := c.computerByID(trim(args.ComputerID))
	if !ok {
		return nil, publicf(CodeInvalidRequest, "There is no computer %q. thread_options lists the ones you can reach.", args.ComputerID)
	}
	report, err := c.applyGroup(ctx, computer, local, call)
	if err != nil {
		return nil, err
	}
	report.ComputerID, report.Computer = c.stamp(computer)
	result := groupResult{GroupReport: report}
	if report.Action == "deleted" {
		result.Note = "Its threads were ungrouped, not deleted, exactly as the sidebar does it."
	}
	return result, nil
}

func (c *session) applyGroup(ctx context.Context, computer Computer, local bool, call GroupCall) (GroupReport, error) {
	if local {
		return c.app.UpdateGroup(ctx, c.caller, call)
	}
	peer, err := c.app.Peer(ctx, computer.ID)
	if err != nil {
		return GroupReport{}, err
	}
	raw, err := json.Marshal(call)
	if err != nil {
		return GroupReport{}, err
	}
	answer, err := peer.Invoke(ctx, "thread_group", raw)
	if err != nil {
		return GroupReport{}, err
	}
	var result groupResult
	if err := peerResult(answer, &result); err != nil {
		return GroupReport{}, err
	}
	return result.GroupReport, nil
}
