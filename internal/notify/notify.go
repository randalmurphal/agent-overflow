// Package notify holds the OS-notification wire contract shared by the
// backend (package main) and the Windows launcher. Target and Send cross a
// process boundary — backend → launcher over the transport event ring, and
// launcher → backend through the NotificationActivated RPC — so both sides
// must agree on shapes, channels, limits, and validation. A single copy here
// is what keeps the two ends from drifting apart: a mismatch would make the
// launcher silently discard notifications the backend considers valid.
package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-overflow/internal/eventchan"
)

// Transport event channels carrying the notification pipe. Defined AS
// their internal/eventchan constants so the emit side and this
// cross-process contract cannot drift, but typed `string`: the launcher
// carries these in subscribe frames and replay cursors, which are wire
// input on the other end. Emit sites use the eventchan constants
// directly.
const (
	ActivatedChannel = string(eventchan.NotificationActivated)
	SendChannel      = string(eventchan.NotificationSend)
)

// Content limits enforced by the backend before publication and re-checked
// by the launcher before presentation.
const (
	MaxThreadIDBytes   = 256
	MaxWorkItemIDBytes = 256
	MaxBackendIDBytes  = 256
	MaxIDBytes         = 256
	MaxTitleBytes      = 4 * 1024
	MaxBodyBytes       = 64 * 1024
)

// Target is the typed deep link carried by every OS notification. Thread and
// workflow routes share this contract across the backend and launcher.
type Target struct {
	Kind       string `json:"kind"`
	ThreadID   string `json:"threadId,omitempty"`
	WorkItemID string `json:"workItemId,omitempty"`
	// BackendID names WHICH backend the route above resolves against — the
	// deep-link scheme docs/specs/remote-access.md §9 describes ("backend
	// UUID + thread id"). It is orthogonal to Kind, not a fourth route: a
	// client attached to several backends holds several threads called
	// "the one in that notification", and the id is how it tells them apart.
	//
	// Optional, and a single-backend client ignores it. Producing one costs
	// nothing (the backend knows its own id) and it is what keeps a future
	// multi-backend client from having to guess; that is the whole of its
	// job today. Additive, per §9's wire discipline — an older launcher
	// decoding this shape drops the field and routes exactly as before.
	BackendID string `json:"backendId,omitempty"`
}

// The target kinds. These are the WIRE spellings: the frontend's tap route
// (frontend/src/lib/stores/events.ts) and the phone's parser read them
// verbatim, so a rename here is a wire change, not a refactor.
const (
	TargetThread       = "thread"
	TargetWorkflowItem = "workflow-item"
	TargetNone         = "none"
)

// Send is the backend-to-presenter wire payload: the Windows launcher over
// the transport event ring, and (§9) an attached remote client raising the
// same notification natively.
//
// ID is allocated by the MAPPING, before transport publication, and it is
// stable per moment rather than per emission — that is what makes Retract
// and replace-in-place possible at all. Two Sends carrying one ID are one
// notification, not two: the second updates the first where the platform
// supports it, and the presenter never accumulates a stack of stale alerts
// for the same thread.
type Send struct {
	ID string `json:"id"`
	// Kind names the moment (see mapping.go). It rides the wire because
	// preferences are per event type (§9), and the presenter that applies
	// them is not always this process: a remote client raising its own OS
	// notification reads its own per-kind toggles off this field.
	Kind Kind `json:"kind"`
	// Retract withdraws the notification previously published under ID
	// because the moment stopped being true — an approval was answered, a
	// rested thread went back to work, a signed-out provider signed in.
	// A retract carries ID and Kind only; Title, Body and Target are empty
	// and a presenter must not read them. Retracting an ID that was never
	// presented is a no-op on every platform, so a presenter never has to
	// remember what it showed.
	Retract bool   `json:"retract,omitempty"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Target  Target `json:"target"`
}

// NewID allocates a throwaway identifier for a notification that names no
// moment. The caller supplies a monotonic sequence to disambiguate ids
// created within the same nanosecond.
//
// It is NOT how a real notification gets its id. Every mapped moment derives
// a STABLE one from what it is about (mapping.go), because that is what lets
// a later send replace it in place and a retraction find it; an allocated id
// would turn "the turn after the one that finished also finished" into two
// banners nobody can withdraw. The one remaining caller is the harness RPC,
// whose sends exist to exercise the pipe and describe no state at all.
func NewID(sequence uint64) string {
	return fmt.Sprintf("agent-overflow-%d-%d", time.Now().UnixNano(), sequence)
}

// ValidateTarget rejects targets that are not exactly one supported wire
// shape.
//
// THE ONE-ID RULE, restated because the shape grew a field. Each kind owns
// exactly one ROUTE identifier: a thread target carries threadId and nothing
// else, a workflow-item target carries workItemId and nothing else, and
// "none" carries neither. The rule is not tidiness. A target carrying both
// ids is a deep link with two destinations, and every consumer resolves that
// by picking one — differently. The launcher, the SPA's activation queue and
// the phone shell would each pick their own, so a click would open different
// things on different devices from the same payload. Refusing the shape is
// what makes the destination a property of the payload rather than of
// whoever decoded it. Stale is the second half: an id left over from a
// kind the target no longer is routes somewhere the moment never named.
//
// BackendID does NOT weaken that rule, and the switch below is where you can
// see why: it is not one of the mutually exclusive branches. It answers a
// different question — not "where does this go" but "whose". Two ids that
// answer one question are ambiguous; two ids that answer different questions
// compose. So BackendID is legal on every kind, "none" included (a
// notification with no route still came from a backend, and a multi-backend
// client attributes it), and it is bounded here rather than branched on.
// A future field that answered "where does this go" would have to join the
// switch instead.
func ValidateTarget(target Target) error {
	if len(target.BackendID) > MaxBackendIDBytes {
		return fmt.Errorf("notification target backendId exceeds %d bytes", MaxBackendIDBytes)
	}
	switch target.Kind {
	case TargetThread:
		if target.ThreadID == "" {
			return errors.New("notification thread target requires threadId")
		}
		if len(target.ThreadID) > MaxThreadIDBytes {
			return fmt.Errorf("notification thread target threadId exceeds %d bytes", MaxThreadIDBytes)
		}
		if target.WorkItemID != "" {
			return errors.New("notification thread target must not include workflow identifiers")
		}
	case TargetWorkflowItem:
		if target.WorkItemID == "" {
			return errors.New("notification workflow-item target requires workItemId")
		}
		if len(target.WorkItemID) > MaxWorkItemIDBytes {
			return fmt.Errorf("notification workflow-item target workItemId exceeds %d bytes", MaxWorkItemIDBytes)
		}
		if target.ThreadID != "" {
			return errors.New("notification workflow-item target must include only workItemId")
		}
	case TargetNone:
		if target.ThreadID != "" || target.WorkItemID != "" {
			return errors.New("notification none target must not include identifiers")
		}
	default:
		return fmt.Errorf("notification target kind %q is unsupported", target.Kind)
	}
	return nil
}

// ValidateSend rejects a payload no presenter should act on. It is the one
// admission check in front of every send pipe — the in-process desktop
// service, the launcher bridge, and the launcher's own re-check after the
// payload crossed a process boundary — so the three cannot drift into
// disagreeing about what a valid notification is.
//
// A retraction is held to a different, narrower contract than a
// presentation: it needs an ID and a kind and must carry no content, because
// content on a retract is a payload nobody will ever render and the shape
// says so.
func ValidateSend(send Send) error {
	if send.ID == "" {
		return errors.New("notification id must be non-empty")
	}
	if len(send.ID) > MaxIDBytes {
		return fmt.Errorf("notification id exceeds %d bytes", MaxIDBytes)
	}
	if !KnownKind(send.Kind) {
		return fmt.Errorf("notification kind %q is unsupported", send.Kind)
	}
	if send.Retract {
		if send.Title != "" || send.Body != "" || send.Target != (Target{}) {
			return errors.New("notification retraction must carry only an id and a kind")
		}
		return nil
	}
	if send.Title == "" {
		return errors.New("notification title must be non-empty")
	}
	if len(send.Title) > MaxTitleBytes {
		return fmt.Errorf("notification title exceeds %d bytes", MaxTitleBytes)
	}
	if len(send.Body) > MaxBodyBytes {
		return fmt.Errorf("notification body exceeds %d bytes", MaxBodyBytes)
	}
	return ValidateTarget(send.Target)
}

// TargetToMap encodes a validated target as the user-info payload a platform
// notification service attaches to the presented notification.
func TargetToMap(target Target) (map[string]any, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return nil, fmt.Errorf("encode notification target: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("encode notification target data: %w", err)
	}
	return data, nil
}

// TargetJSON encodes a validated target as the ONE document a presenter
// that cannot carry a nested map has to pass along — today the phone push
// payload, whose data map is flat strings (`internal/push`, KeyTarget).
//
// Beside TargetToMap rather than inside it because the two answer different
// questions and share the one thing that matters: both validate first, and
// both produce exactly the field spellings `TargetFromMap` and the SPA's
// `parseNotificationTarget` read back. A caller that marshalled a Target
// itself would skip the validation and, sooner or later, spell a field
// differently.
func TargetJSON(target Target) (string, error) {
	if err := ValidateTarget(target); err != nil {
		return "", err
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return "", fmt.Errorf("encode notification target: %w", err)
	}
	return string(raw), nil
}

// TargetFromMap decodes a platform user-info payload back into a validated
// Target. Click callbacks route through this before any navigation happens.
func TargetFromMap(data map[string]any) (Target, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Target{}, fmt.Errorf("decode notification target data: %w", err)
	}
	var target Target
	if err := json.Unmarshal(raw, &target); err != nil {
		return Target{}, fmt.Errorf("decode notification target: %w", err)
	}
	if err := ValidateTarget(target); err != nil {
		return Target{}, err
	}
	return target, nil
}
