package codex

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// -- buildElicitationMeta tests --

func TestBuildElicitationMeta_WithServerName(t *testing.T) {
	params := json.RawMessage(`{"serverName":"my-mcp-server","message":"","requestedSchema":{"type":"object"}}`)
	meta := buildElicitationMeta("t1", "turn-1", 55, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.RequestID != "55" {
		t.Errorf("requestID: got %q, want %q", approval.RequestID, "55")
	}
	if approval.ThreadID != "t1" {
		t.Errorf("threadID: got %q, want %q", approval.ThreadID, "t1")
	}
	if approval.TurnID != "turn-1" {
		t.Errorf("turnID: got %q, want %q", approval.TurnID, "turn-1")
	}
	if approval.ToolName != "mcp_elicitation" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "mcp_elicitation")
	}
	if approval.Kind != "mcp-elicitation" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "mcp-elicitation")
	}
	if approval.Title != "MCP Server Consent" {
		t.Errorf("title: got %q, want %q", approval.Title, "MCP Server Consent")
	}
	// When message is empty but serverName is present, description uses the serverName format.
	want := `MCP server "my-mcp-server" requests user consent`
	if approval.Description != want {
		t.Errorf("description: got %q, want %q", approval.Description, want)
	}
}

func TestBuildElicitationMeta_WithMessage(t *testing.T) {
	params := json.RawMessage(`{"serverName":"srv","message":"Please confirm access to the database"}`)
	meta := buildElicitationMeta("t1", "turn-2", 10, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// When message is present, it takes precedence over serverName-derived description.
	if approval.Description != "Please confirm access to the database" {
		t.Errorf("description: got %q, want %q", approval.Description, "Please confirm access to the database")
	}
}

func TestBuildElicitationMeta_NoServerNameNoMessage(t *testing.T) {
	params := json.RawMessage(`{}`)
	meta := buildElicitationMeta("t1", "", 1, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Description != "MCP server elicitation" {
		t.Errorf("description: got %q, want %q", approval.Description, "MCP server elicitation")
	}
}

func TestBuildElicitationMeta_InputPreserved(t *testing.T) {
	params := json.RawMessage(`{"serverName":"x","message":"confirm","requestedSchema":{"type":"string"}}`)
	meta := buildElicitationMeta("t1", "turn-1", 99, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(approval.Input) != string(params) {
		t.Errorf("input: got %s, want %s", approval.Input, params)
	}
}

// -- approvalKindForMethod tests --

func TestApprovalKindForMethod(t *testing.T) {
	tests := []struct {
		method string
		want   string
	}{
		{"item/commandExecution/requestApproval", "command"},
		{"execCommandApproval", "command"},
		{"item/fileRead/requestApproval", "file-read"},
		{"item/fileChange/requestApproval", "file-change"},
		{"applyPatchApproval", "file-change"},
		{"unknown/method", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			got := approvalKindForMethod(tt.method)
			if got != tt.want {
				t.Errorf("approvalKindForMethod(%q) = %q, want %q", tt.method, got, tt.want)
			}
		})
	}
}

// -- buildApprovalMeta file-read path --

func TestBuildApprovalMetaFileRead(t *testing.T) {
	params := json.RawMessage(`{"filePath":"/etc/hosts"}`)
	meta := buildApprovalMeta("t1", "turn-1", "item/fileRead/requestApproval", 50, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.ToolName != "file_read" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "file_read")
	}
	if approval.Title != "File read" {
		t.Errorf("title: got %q, want %q", approval.Title, "File read")
	}
	if approval.Description != "/etc/hosts" {
		t.Errorf("description: got %q, want %q", approval.Description, "/etc/hosts")
	}
	if approval.Kind != "file-read" {
		t.Errorf("kind: got %q, want %q", approval.Kind, "file-read")
	}
}

// -- buildApprovalMeta with neither command nor filePath (default kind) --

func TestBuildApprovalMetaUnknownMethod(t *testing.T) {
	params := json.RawMessage(`{"someField":"value"}`)
	meta := buildApprovalMeta("t1", "", "custom/approval", 77, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// When there's no command or filePath, toolName and description default to the method.
	if approval.ToolName != "custom/approval" {
		t.Errorf("toolName: got %q, want %q", approval.ToolName, "custom/approval")
	}
	if approval.Description != "custom/approval" {
		t.Errorf("description: got %q, want %q", approval.Description, "custom/approval")
	}
	if approval.Kind != "" {
		t.Errorf("kind: got %q, want empty", approval.Kind)
	}
}

// -- parseUserInputQuestions tests --

func TestParseUserInputQuestions_Valid(t *testing.T) {
	params := json.RawMessage(`{"questions":[{"id":"q1","header":"H","question":"Q","options":[{"label":"A"}],"multiSelect":false}]}`)
	questions := parseUserInputQuestions(params)
	if len(questions) != 1 {
		t.Fatalf("len = %d, want 1", len(questions))
	}
	if questions[0].ID != "q1" {
		t.Errorf("id: got %q, want %q", questions[0].ID, "q1")
	}
}

func TestParseUserInputQuestions_InvalidJSON(t *testing.T) {
	questions := parseUserInputQuestions(json.RawMessage(`not json`))
	if questions != nil {
		t.Errorf("expected nil for invalid JSON, got %v", questions)
	}
}

func TestParseUserInputQuestions_EmptyQuestions(t *testing.T) {
	questions := parseUserInputQuestions(json.RawMessage(`{"questions":[]}`))
	if len(questions) != 0 {
		t.Errorf("len = %d, want 0", len(questions))
	}
}

func TestParseUserInputQuestions_NoQuestionsField(t *testing.T) {
	questions := parseUserInputQuestions(json.RawMessage(`{"other":"data"}`))
	if questions != nil {
		t.Errorf("expected nil for missing questions field, got %v", questions)
	}
}

// -- parsePermissionRequest tests --

func TestParsePermissionRequest_Valid(t *testing.T) {
	enabled := true
	params := json.RawMessage(`{"reason":"Need network access","permissions":{"network":{"enabled":true},"fileSystem":{"read":["/tmp"],"write":["/tmp/out"]}}}`)
	reason, perms := parsePermissionRequest(params)

	if reason != "Need network access" {
		t.Errorf("reason: got %q, want %q", reason, "Need network access")
	}
	if perms == nil {
		t.Fatal("expected non-nil permissions")
	}
	if perms.Network == nil || perms.Network.Enabled == nil || *perms.Network.Enabled != enabled {
		t.Error("expected network enabled=true")
	}
	if perms.FileSystem == nil || len(perms.FileSystem.Read) != 1 || perms.FileSystem.Read[0] != "/tmp" {
		t.Errorf("fileSystem.read: got %v", perms.FileSystem)
	}
}

func TestParsePermissionRequest_InvalidJSON(t *testing.T) {
	reason, perms := parsePermissionRequest(json.RawMessage(`not json`))
	if reason != "" || perms != nil {
		t.Errorf("expected empty/nil for invalid JSON, got %q, %v", reason, perms)
	}
}

func TestParsePermissionRequest_EmptyObject(t *testing.T) {
	reason, perms := parsePermissionRequest(json.RawMessage(`{}`))
	if reason != "" {
		t.Errorf("reason: got %q, want empty", reason)
	}
	if perms != nil {
		t.Errorf("expected nil permissions for empty object, got %v", perms)
	}
}

// -- buildThreadParams edge cases --

func TestBuildThreadParamsReadOnly(t *testing.T) {
	params := buildThreadParams(Config{
		Sandbox:        "read-only",
		ApprovalPolicy: "on-request",
	})
	if params["sandboxPolicy"] != "read-only" {
		t.Errorf("sandboxPolicy: got %v, want %q", params["sandboxPolicy"], "read-only")
	}
	if params["approvalPolicy"] != "on-request" {
		t.Errorf("approvalPolicy: got %v, want %q", params["approvalPolicy"], "on-request")
	}
}

func TestBuildThreadParamsUnknownSandbox(t *testing.T) {
	// Unknown sandbox value falls through to default case.
	params := buildThreadParams(Config{
		Sandbox:        "custom-sandbox",
		ApprovalPolicy: "untrusted",
	})
	if params["sandboxPolicy"] != "read-only" {
		t.Errorf("sandboxPolicy: got %v, want %q", params["sandboxPolicy"], "read-only")
	}
	if params["approvalPolicy"] != "untrusted" {
		t.Errorf("approvalPolicy: got %v, want %q", params["approvalPolicy"], "untrusted")
	}
}

func TestBuildThreadParamsMinimal(t *testing.T) {
	params := buildThreadParams(Config{})
	if len(params) != 0 {
		t.Errorf("expected empty params for zero config, got %v", params)
	}
}

func TestBuildThreadParamsWorkDir(t *testing.T) {
	params := buildThreadParams(Config{WorkDir: "/home/user/project"})
	if params["cwd"] != "/home/user/project" {
		t.Errorf("cwd: got %v, want %q", params["cwd"], "/home/user/project")
	}
}

// -- readRouteFields edge cases --

func TestReadRouteFieldsEmpty(t *testing.T) {
	turnID, itemID := readRouteFields(json.RawMessage(`{}`))
	if turnID != "" || itemID != "" {
		t.Errorf("expected empty strings, got turnID=%q, itemID=%q", turnID, itemID)
	}
}

func TestReadRouteFieldsMixedSources(t *testing.T) {
	// turnId at top level, item.id nested.
	params := json.RawMessage(`{"turnId":"turn-top","item":{"id":"item-nested"}}`)
	turnID, itemID := readRouteFields(params)
	if turnID != "turn-top" {
		t.Errorf("turnID: got %q, want %q", turnID, "turn-top")
	}
	if itemID != "item-nested" {
		t.Errorf("itemID: got %q, want %q", itemID, "item-nested")
	}
}
