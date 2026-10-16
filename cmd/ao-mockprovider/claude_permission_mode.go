package main

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// claudePermissionMode follows the permission mode and model this process
// runs, for the two CLI behaviors AO reads back:
//
//   - Every mode change is reported on `system/status`
//     (`{"status":null,"permissionMode":…}`), including the ones the client
//     asked for: a set_permission_mode answers its ack, then the report.
//   - Since 2.1.299, a set_model to a model without auto mode, while the
//     session runs auto, leaves auto for "default" and reports it before
//     the set_model ack, and a set_permission_mode to auto on such a model
//     is refused.
//
// Wire shapes from the 2.1.300 bundle (claude-wire.md §system/status).
type claudePermissionMode struct {
	mu    sync.Mutex
	mode  string
	model string
}

func newClaudePermissionMode(args []string) *claudePermissionMode {
	mode := flagValue(args, "--permission-mode")
	if mode == "" {
		mode = "default"
	}
	return &claudePermissionMode{mode: mode, model: flagValue(args, "--model")}
}

// claudeModelSupportsAutoMode is the CLI's own check (2.1.300 `iD`): the 4.6
// generation and older, and every Haiku but 5.5, have no auto mode.
func claudeModelSupportsAutoMode(model string) bool {
	model = strings.TrimSuffix(strings.TrimSpace(model), "[1m]")
	switch model {
	case "claude-opus-4-6", "claude-sonnet-4-6", "claude-opus-4-5", "claude-sonnet-4-5",
		"claude-opus-4-1", "claude-opus-4-0", "claude-sonnet-4-0":
		return false
	}
	return !strings.Contains(model, "haiku") || model == "claude-haiku-5-5"
}

// claudeAutoModeRefusal is the CLI's answer to set_permission_mode auto on a
// model without auto mode (2.1.300: the refusal reason "model").
const claudeAutoModeRefusal = "Cannot set permission mode to auto: auto mode unavailable for this model"

// frames is what the CLI writes ahead of a control request's ack and after
// it, or the error it answers instead of the ack. Called only for a request
// whose keys the ack accepts.
func (p *claudePermissionMode) frames(subtype string, request json.RawMessage) (beforeAck, afterAck []string, refusal string) {
	var fields struct {
		Model *string `json:"model"`
		Mode  string  `json:"mode"`
	}
	if json.Unmarshal(request, &fields) != nil {
		return nil, nil, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch subtype {
	case "set_permission_mode":
		if fields.Mode == "auto" && !claudeModelSupportsAutoMode(p.model) {
			return nil, nil, claudeAutoModeRefusal
		}
		if fields.Mode == "" || fields.Mode == p.mode {
			return nil, nil, ""
		}
		p.mode = fields.Mode
		return nil, []string{claudePermissionModeStatus(p.mode)}, ""
	case "set_model":
		if fields.Model == nil || strings.TrimSpace(*fields.Model) == "" {
			return nil, nil, ""
		}
		p.model = *fields.Model
		if p.mode == "auto" && !claudeModelSupportsAutoMode(p.model) {
			p.mode = "default"
			return []string{claudePermissionModeStatus(p.mode)}, nil, ""
		}
	}
	return nil, nil, ""
}

// current is the mode `system/init` restates.
func (p *claudePermissionMode) current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode
}

func claudePermissionModeStatus(mode string) string {
	return mustJSON(map[string]any{
		"type":           "system",
		"subtype":        "status",
		"status":         nil,
		"permissionMode": mode,
		"uuid":           uuid.NewString(),
	})
}
