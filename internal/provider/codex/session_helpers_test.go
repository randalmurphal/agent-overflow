package codex

import (
	"encoding/json"
	"fmt"
	"reflect"
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

// -- buildElicitationMeta: Elicitation field coverage --

// Form mode with an explicit `mode: "form"` on the wire. The RequestedSchema
// passes through as raw JSON and mode is preserved verbatim.
func TestBuildElicitationMeta_FormMode_Explicit(t *testing.T) {
	params := json.RawMessage(`{
		"mode":"form",
		"message":"Provide connection details",
		"serverName":"db-mcp",
		"requestedSchema":{"type":"object","properties":{"host":{"type":"string"}}}
	}`)
	meta := buildElicitationMeta("t1", "turn-A", 12, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Elicitation == nil {
		t.Fatal("expected non-nil Elicitation")
	}
	e := approval.Elicitation
	if e.Mode != "form" {
		t.Errorf("mode: got %q, want form", e.Mode)
	}
	if e.Message != "Provide connection details" {
		t.Errorf("message: got %q", e.Message)
	}
	if e.ServerName != "db-mcp" {
		t.Errorf("serverName: got %q", e.ServerName)
	}
	if e.URL != "" || e.ElicitationID != "" {
		t.Errorf("url/elicitationId should be empty in form mode, got %q / %q", e.URL, e.ElicitationID)
	}
	// RequestedSchema should round-trip. Canonicalize via re-marshal to avoid
	// whitespace differences.
	var got, want any
	if err := json.Unmarshal(e.RequestedSchema, &got); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"host":{"type":"string"}}}`), &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if !jsonEqual(got, want) {
		t.Errorf("requestedSchema: got %s, want %s", e.RequestedSchema, `{"type":"object","properties":{"host":{"type":"string"}}}`)
	}
}

// URL mode with explicit discriminator and both URL-mode fields populated.
func TestBuildElicitationMeta_URLMode_Explicit(t *testing.T) {
	params := json.RawMessage(`{
		"mode":"url",
		"message":"Authorize this app",
		"serverName":"oauth-mcp",
		"url":"https://auth.example.com/approve?token=abc",
		"elicitationId":"el-42"
	}`)
	meta := buildElicitationMeta("t1", "turn-B", 7, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	e := approval.Elicitation
	if e == nil {
		t.Fatal("expected non-nil Elicitation")
	}
	if e.Mode != "url" {
		t.Errorf("mode: got %q, want url", e.Mode)
	}
	if e.URL != "https://auth.example.com/approve?token=abc" {
		t.Errorf("url: got %q", e.URL)
	}
	if e.ElicitationID != "el-42" {
		t.Errorf("elicitationId: got %q", e.ElicitationID)
	}
	// URL mode must NOT leak the schema field even if one was present.
	if len(e.RequestedSchema) != 0 {
		t.Errorf("URL mode must not set RequestedSchema; got %s", e.RequestedSchema)
	}
}

// Adversarial: `mode` omitted but URL present. Parser must infer URL mode.
func TestBuildElicitationMeta_InferURLFromPayload(t *testing.T) {
	params := json.RawMessage(`{"message":"Open link","url":"https://x.test","elicitationId":"e1"}`)
	meta := buildElicitationMeta("t1", "", 1, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Elicitation == nil || approval.Elicitation.Mode != "url" {
		t.Errorf("expected inferred url mode, got %+v", approval.Elicitation)
	}
	if approval.Elicitation.URL != "https://x.test" {
		t.Errorf("url not preserved: %q", approval.Elicitation.URL)
	}
}

// Adversarial: `mode` omitted but schema present. Parser must infer form mode.
func TestBuildElicitationMeta_InferFormFromPayload(t *testing.T) {
	params := json.RawMessage(`{"message":"m","requestedSchema":{"type":"object"}}`)
	meta := buildElicitationMeta("t1", "", 1, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Elicitation == nil || approval.Elicitation.Mode != "form" {
		t.Errorf("expected inferred form mode, got %+v", approval.Elicitation)
	}
}

// Adversarial: `mode` is present but not one of the known values. Falls back
// to shape-based inference, then to "form" as last resort.
func TestBuildElicitationMeta_UnknownModeFallsBack(t *testing.T) {
	cases := []struct {
		name     string
		params   string
		wantMode string
	}{
		{"garbage mode, url present", `{"mode":"??","url":"https://x.test"}`, "url"},
		{"garbage mode, schema present", `{"mode":"??","requestedSchema":{"type":"object"}}`, "form"},
		{"garbage mode, nothing else", `{"mode":"??"}`, "form"},
		{"empty mode string", `{"mode":""}`, "form"},
		{"numeric mode (type mismatch)", `{"mode":7}`, "form"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := buildElicitationMeta("t1", "", 1, json.RawMessage(tc.params))
			var approval provider.ApprovalRequest
			if err := json.Unmarshal(meta, &approval); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if approval.Elicitation == nil || approval.Elicitation.Mode != tc.wantMode {
				t.Errorf("got %+v, want mode=%q", approval.Elicitation, tc.wantMode)
			}
		})
	}
}

// Adversarial: entirely invalid JSON. Must not panic; must still produce a
// renderable approval with form mode + empty schema.
func TestBuildElicitationMeta_MalformedJSONIsSafe(t *testing.T) {
	params := json.RawMessage(`not even close to json`)
	meta := buildElicitationMeta("t1", "", 1, params)

	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Kind != "mcp-elicitation" {
		t.Errorf("kind must still be mcp-elicitation on malformed input, got %q", approval.Kind)
	}
	if approval.Elicitation == nil {
		t.Fatal("Elicitation must be non-nil on malformed input")
	}
	if approval.Elicitation.Mode != "form" {
		t.Errorf("expected fallback form mode, got %q", approval.Elicitation.Mode)
	}
	if len(approval.Elicitation.RequestedSchema) != 0 {
		t.Errorf("expected no schema on malformed input, got %s", approval.Elicitation.RequestedSchema)
	}
}

// Adversarial: null JSON literal as the whole payload.
func TestBuildElicitationMeta_NullParams(t *testing.T) {
	meta := buildElicitationMeta("t1", "", 1, json.RawMessage(`null`))
	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Elicitation == nil {
		t.Fatal("Elicitation must be non-nil even on null params")
	}
	if approval.Elicitation.Mode != "form" {
		t.Errorf("expected fallback form mode, got %q", approval.Elicitation.Mode)
	}
}

// Adversarial: `requestedSchema` is explicitly the JSON null. The parser must
// treat null-valued-schema the same as absent-schema; not pass "null" through.
func TestBuildElicitationMeta_NullSchemaIsTreatedAsAbsent(t *testing.T) {
	for _, raw := range []string{
		`{"mode":"form","requestedSchema":null}`,
		`{"mode":"form","requestedSchema":  null }`,
	} {
		t.Run(raw, func(t *testing.T) {
			meta := buildElicitationMeta("t1", "", 1, json.RawMessage(raw))
			var approval provider.ApprovalRequest
			if err := json.Unmarshal(meta, &approval); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(approval.Elicitation.RequestedSchema) != 0 {
				t.Errorf("null schema must not pass through; got %s", approval.Elicitation.RequestedSchema)
			}
		})
	}
}

// URL mode where only the URL is provided and elicitationId is missing. The
// parser still produces a usable Elicitation — the UI can show the URL even
// without an elicitationId (response just omits the id).
func TestBuildElicitationMeta_URLModeMissingElicitationID(t *testing.T) {
	meta := buildElicitationMeta("t1", "", 1, json.RawMessage(`{"mode":"url","url":"https://x.test"}`))
	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Elicitation.URL != "https://x.test" {
		t.Errorf("url not preserved: %q", approval.Elicitation.URL)
	}
	if approval.Elicitation.ElicitationID != "" {
		t.Errorf("elicitationId should be empty, got %q", approval.Elicitation.ElicitationID)
	}
}

// Adversarial: unicode + control characters in message and serverName survive
// the round-trip without corruption.
func TestBuildElicitationMeta_UnicodePreserved(t *testing.T) {
	params := json.RawMessage(`{"mode":"form","message":"日本語 \u00e9 \\n embedded","serverName":"服务器","requestedSchema":{"type":"object"}}`)
	meta := buildElicitationMeta("t1", "", 1, params)
	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Elicitation.Message != "日本語 é \\n embedded" {
		t.Errorf("message corrupted: %q", approval.Elicitation.Message)
	}
	if approval.Elicitation.ServerName != "服务器" {
		t.Errorf("serverName corrupted: %q", approval.Elicitation.ServerName)
	}
}

// Adversarial: large schema payload (>1 MB equivalent). Parser should not
// choke; RequestedSchema preserves the full payload byte-for-byte by content.
func TestBuildElicitationMeta_LargeSchemaRoundTrip(t *testing.T) {
	// Build a schema with many properties to simulate a chatty server.
	var b []byte
	b = append(b, `{"mode":"form","message":"big","requestedSchema":{"type":"object","properties":{`...)
	for i := 0; i < 2000; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		// Each property: "p0000": {"type":"string","description":"..."}
		b = append(b, []byte(fmt.Sprintf(`"p%04d":{"type":"string","description":"field %d"}`, i, i))...)
	}
	b = append(b, "}}}"...)

	meta := buildElicitationMeta("t1", "", 1, json.RawMessage(b))
	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Ensure the schema made it through and can be re-parsed as JSON.
	var decoded map[string]any
	if err := json.Unmarshal(approval.Elicitation.RequestedSchema, &decoded); err != nil {
		t.Fatalf("schema not valid JSON after round-trip: %v", err)
	}
	props, ok := decoded["properties"].(map[string]any)
	if !ok || len(props) != 2000 {
		t.Errorf("expected 2000 properties in round-tripped schema, got %d", len(props))
	}
}

// Both URL and schema present with explicit form mode → schema wins, URL is
// dropped. Defensive choice so a confused server can't slip a phishing URL
// past a form-mode UI.
func TestBuildElicitationMeta_FormModeIgnoresURLField(t *testing.T) {
	params := json.RawMessage(`{"mode":"form","url":"https://evil.test","requestedSchema":{"type":"object"}}`)
	meta := buildElicitationMeta("t1", "", 1, params)
	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if approval.Elicitation.URL != "" {
		t.Errorf("form mode must not expose URL field, got %q", approval.Elicitation.URL)
	}
	if len(approval.Elicitation.RequestedSchema) == 0 {
		t.Errorf("expected schema to be preserved in form mode")
	}
}

// URL mode with a schema field present → URL wins, schema is dropped. Parallel
// of the form-mode-ignores-url test.
func TestBuildElicitationMeta_URLModeIgnoresSchemaField(t *testing.T) {
	params := json.RawMessage(`{"mode":"url","url":"https://x.test","elicitationId":"e1","requestedSchema":{"type":"object"}}`)
	meta := buildElicitationMeta("t1", "", 1, params)
	var approval provider.ApprovalRequest
	if err := json.Unmarshal(meta, &approval); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(approval.Elicitation.RequestedSchema) != 0 {
		t.Errorf("url mode must not expose RequestedSchema, got %s", approval.Elicitation.RequestedSchema)
	}
	if approval.Elicitation.URL != "https://x.test" {
		t.Errorf("url not preserved: %q", approval.Elicitation.URL)
	}
}

// isJSONNull coverage: whitespace-padded nulls, non-null values, truncated
// "nul" must report false. Pinned down because the check drives the
// null-schema-equals-absent behavior.
func TestIsJSONNull(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"null", true},
		{" null", true},
		{"null ", true}, // trailing whitespace is ignored for our purposes
		{"  \t\nnull", true},
		{"nullx", true},   // starts with null — isJSONNull only checks prefix after trim
		{"nul", false},    // too short
		{"n", false},      // truncated
		{"", false},       // empty
		{"true", false},   // other literal
		{`"null"`, false}, // quoted string
		{"0", false},      // number
		{"{}", false},     // object
		{`   `, false},    // all whitespace, no content
		{"Null", false},   // JSON is case-sensitive — only lowercase null is the literal
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isJSONNull(json.RawMessage(tc.in)); got != tc.want {
				t.Errorf("isJSONNull(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// jsonEqual deep-compares two JSON values after parsing so whitespace /
// ordering don't affect equality. Test-local helper.
func jsonEqual(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
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

func TestParseUserInputQuestions_NormalizesMissingAndReservedIDs(t *testing.T) {
	params := json.RawMessage(`{"questions":[{"header":"Framework","question":"Q","options":[{"label":"A"}]},{"question":"Mode","options":[{"label":"B"}]},{"id":"__proto__","header":"constructor","question":"Safe","options":[{"label":"C"}]}]}`)
	questions := parseUserInputQuestions(params)
	if len(questions) != 3 {
		t.Fatalf("len = %d, want 3", len(questions))
	}
	got := []string{questions[0].ID, questions[1].ID, questions[2].ID}
	want := []string{"Framework", "Mode", "Safe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %#v, want %#v", got, want)
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
	if params["sandbox"] != "read-only" {
		t.Errorf("sandbox: got %v, want %q", params["sandbox"], "read-only")
	}
	if params["approvalPolicy"] != "on-request" {
		t.Errorf("approvalPolicy: got %v, want %q", params["approvalPolicy"], "on-request")
	}
}

func TestBuildThreadParamsUnknownSandbox(t *testing.T) {
	// Unknown sandbox value falls through to the safest thread-start value.
	params := buildThreadParams(Config{
		Sandbox:        "custom-sandbox",
		ApprovalPolicy: "untrusted",
	})
	if params["sandbox"] != "read-only" {
		t.Errorf("sandbox: got %v, want %q", params["sandbox"], "read-only")
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

func TestBuildThreadParamsIncludesMCPServers(t *testing.T) {
	params := buildThreadParams(Config{
		MCPServers: map[string]any{
			"design": map[string]any{"url": "http://127.0.0.1:1234/mcp/thread"},
		},
	})

	config, ok := params["config"].(map[string]any)
	if !ok {
		t.Fatalf("config type = %T, want map[string]any", params["config"])
	}
	if config["mcp_servers"] == nil {
		t.Fatal("expected mcp_servers config override")
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
