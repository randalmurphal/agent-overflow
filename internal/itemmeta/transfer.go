package itemmeta

import (
	"encoding/json"
	"fmt"
)

// AttachmentDestination binds an original attachment reference to its installed
// owner. SourceThreadID prevents a corrupt reference from borrowing another
// thread's attachment just because the ID exists.
type AttachmentDestination struct{ SourceThreadID, ThreadID, ID string }

// TransferAttachments rewrites only the structured attachments field. Provider
// correlation, unknown metadata, and prose retain their exact JSON values.
func TransferAttachments(raw, sourceThreadID string, destinations map[string]AttachmentDestination) (string, error) {
	if raw == "" {
		return raw, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return "", err
	}
	attachments, ok := object["attachments"]
	if !ok {
		return raw, nil
	}
	rewritten, err := TransferAttachmentArray(string(attachments), sourceThreadID, destinations)
	if err != nil {
		return "", err
	}
	object["attachments"] = json.RawMessage(rewritten)
	encoded, err := json.Marshal(object)
	return string(encoded), err
}

// TransferAttachmentArray also handles composer drafts and legacy ID arrays.
func TransferAttachmentArray(raw, sourceThreadID string, destinations map[string]AttachmentDestination) (string, error) {
	var values []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return "", err
	}
	for i, value := range values {
		var id string
		if json.Unmarshal(value, &id) == nil {
			destination, ok := destinations[id]
			if !ok {
				return "", fmt.Errorf("transfer: attachment %q is no longer available", id)
			}
			encoded, err := json.Marshal(destination.ID)
			if err != nil {
				return "", err
			}
			values[i] = encoded
			continue
		}
		var attachment map[string]json.RawMessage
		if err := json.Unmarshal(value, &attachment); err != nil {
			return "", err
		}
		if err := json.Unmarshal(attachment["id"], &id); err != nil {
			return "", err
		}
		threadID := sourceThreadID
		if field := attachment["threadId"]; len(field) != 0 {
			if err := json.Unmarshal(field, &threadID); err != nil {
				return "", err
			}
			if threadID == "" {
				threadID = sourceThreadID
			}
		}
		destination, ok := destinations[id]
		if !ok || destination.SourceThreadID != threadID {
			return "", fmt.Errorf("transfer: attachment %q is no longer available on its recorded computer", id)
		}
		attachment["id"], _ = json.Marshal(destination.ID)
		attachment["threadId"], _ = json.Marshal(destination.ThreadID)
		encoded, err := json.Marshal(attachment)
		if err != nil {
			return "", err
		}
		values[i] = encoded
	}
	encoded, err := json.Marshal(values)
	return string(encoded), err
}

// TransferThreadReferences updates explicit AO plan/review links and their
// comment IDs. External-thread links keep their owners. The remapper is supplied
// by storage, so this package remains independent of its ID and database types.
func TransferThreadReferences(raw, sourceID, targetID string, remap func(string, string) string) (string, error) {
	if raw == "" || sourceID == targetID {
		return raw, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return "", err
	}
	changed := false
	for _, field := range []struct{ ref, comments, kind string }{
		{"sourceProposedPlan", "", ""},
		{"revisionSourceProposedPlan", "revisionSourceCommentIds", "plan_comment"},
		{"revisionSourceDiffReview", "revisionSourceDiffCommentIds", "diff_comment"},
	} {
		value, ok := object[field.ref]
		if !ok || string(value) == "null" {
			continue
		}
		var ref map[string]json.RawMessage
		if err := json.Unmarshal(value, &ref); err != nil {
			return "", err
		}
		owner := sourceID
		if current := ref["threadId"]; len(current) != 0 {
			if err := json.Unmarshal(current, &owner); err != nil {
				return "", err
			}
			if owner == "" {
				owner = sourceID
			}
		}
		if owner != sourceID {
			continue
		}
		ref["threadId"], _ = json.Marshal(targetID)
		encoded, err := json.Marshal(ref)
		if err != nil {
			return "", err
		}
		object[field.ref] = encoded
		changed = true
		if comments, ok := object[field.comments]; ok {
			var ids []string
			if err := json.Unmarshal(comments, &ids); err != nil {
				return "", err
			}
			for i, id := range ids {
				ids[i] = remap(field.kind, id)
			}
			object[field.comments], err = json.Marshal(ids)
			if err != nil {
				return "", err
			}
		}
	}
	if !changed {
		return raw, nil
	}
	encoded, err := json.Marshal(object)
	return string(encoded), err
}
