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
)

// Transport event channels carrying the notification pipe.
const (
	ActivatedChannel = "notification:activated"
	SendChannel      = "notification:send"
)

// Content limits enforced by the backend before publication and re-checked
// by the launcher before presentation.
const (
	MaxThreadIDBytes   = 256
	MaxWorkItemIDBytes = 256
	MaxTitleBytes      = 4 * 1024
	MaxBodyBytes       = 64 * 1024
)

// Target is the typed deep link carried by every OS notification. Thread and
// workflow routes share this contract across the backend and launcher.
type Target struct {
	Kind       string `json:"kind"`
	ThreadID   string `json:"threadId,omitempty"`
	WorkItemID string `json:"workItemId,omitempty"`
}

// Send is the backend-to-launcher wire payload. ID is allocated before
// transport publication so the launcher can pass a stable identifier to the
// host notification service.
type Send struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Target Target `json:"target"`
}

// NewID allocates a presentation identifier for one notification. The
// caller supplies a monotonic sequence to disambiguate notifications
// created within the same nanosecond.
func NewID(sequence uint64) string {
	return fmt.Sprintf("agent-overflow-%d-%d", time.Now().UnixNano(), sequence)
}

// ValidateTarget rejects targets that are not exactly one supported wire
// shape. Each kind owns one identifier so stale or ambiguous deep links never
// cross the launcher boundary.
func ValidateTarget(target Target) error {
	switch target.Kind {
	case "thread":
		if target.ThreadID == "" {
			return errors.New("notification thread target requires threadId")
		}
		if len(target.ThreadID) > MaxThreadIDBytes {
			return fmt.Errorf("notification thread target threadId exceeds %d bytes", MaxThreadIDBytes)
		}
		if target.WorkItemID != "" {
			return errors.New("notification thread target must not include workflow identifiers")
		}
	case "workflow-item":
		if target.WorkItemID == "" {
			return errors.New("notification workflow-item target requires workItemId")
		}
		if len(target.WorkItemID) > MaxWorkItemIDBytes {
			return fmt.Errorf("notification workflow-item target workItemId exceeds %d bytes", MaxWorkItemIDBytes)
		}
		if target.ThreadID != "" {
			return errors.New("notification workflow-item target must include only workItemId")
		}
	case "none":
		if target.ThreadID != "" || target.WorkItemID != "" {
			return errors.New("notification none target must not include identifiers")
		}
	default:
		return fmt.Errorf("notification target kind %q is unsupported", target.Kind)
	}
	return nil
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
