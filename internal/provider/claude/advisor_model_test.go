package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
)

// Wire shapes for the advisor model tests. The parent runs Opus while the
// advisor runs Fable (the configuration that exposed the old
// parent-model stamp); usage.iterations lists each advisor call's model
// in call order, as captured from CLI 2.1.284 session usage.

func advisorMessageStart(parent, id string) string {
	return `{"type":"stream_event","parent_tool_use_id":` + jsonString(parent) +
		`,"event":{"type":"message_start","message":{"id":"` + id + `","role":"assistant","model":"claude-opus-5-5","content":[],"usage":{"input_tokens":3}}}}`
}

func advisorCallEnvelope(parent, msgID, toolUseID, usage string) string {
	return `{"type":"assistant","parent_tool_use_id":` + jsonString(parent) +
		`,"message":{"id":"` + msgID + `","role":"assistant","model":"claude-opus-5-5","content":[{"type":"server_tool_use","id":"` + toolUseID + `","name":"advisor","input":{}}],"usage":` + usage + `}}`
}

func advisorResultEnvelope(parent, msgID, toolUseID string) string {
	return `{"type":"assistant","parent_tool_use_id":` + jsonString(parent) +
		`,"message":{"id":"` + msgID + `","role":"assistant","model":"claude-opus-5-5","content":[{"type":"advisor_tool_result","tool_use_id":"` + toolUseID + `","content":{"type":"advisor_redacted_result","encrypted_content":"opaque"}}],"usage":{"input_tokens":3}}}`
}

func advisorMessageDelta(parent string, advisorModels ...string) string {
	return `{"type":"stream_event","parent_tool_use_id":` + jsonString(parent) +
		`,"event":{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":` + advisorUsage(advisorModels...) + `}}`
}

// advisorUsage is a message usage block whose iterations interleave
// parent calls with one advisor call per model.
func advisorUsage(advisorModels ...string) string {
	iterations := []string{`{"type":"message","input_tokens":2,"output_tokens":10}`}
	for _, model := range advisorModels {
		iterations = append(iterations,
			`{"type":"advisor_message","model":"`+model+`","input_tokens":900,"output_tokens":300}`,
			`{"type":"message","input_tokens":2,"output_tokens":10}`)
	}
	return `{"input_tokens":6,"output_tokens":30,"cache_read_input_tokens":100,"cache_creation_input_tokens":0,"iterations":[` + strings.Join(iterations, ",") + `]}`
}

// advisorModelStamps feeds lines through one parser and returns, per
// advisor tool id, the advisor_model values its EventToolStarts carried
// in order, plus whether each carrying event was a meta-only update.
func advisorModelStamps(t *testing.T, lines ...string) (map[string][]string, map[string][]bool) {
	t.Helper()
	parser := NewParser()
	models := map[string][]string{}
	updates := map[string][]bool{}
	for _, line := range lines {
		events, err := parser.ParseLine(testThreadProto, []byte(line))
		if err != nil {
			t.Fatalf("parse %s: %v", line, err)
		}
		for _, evt := range events {
			if evt.Kind != provider.EventToolStart || evt.ItemType != "advisor" {
				continue
			}
			var meta struct {
				AdvisorModel   string `json:"advisor_model"`
				MetaUpdateOnly bool   `json:"meta_update_only"`
			}
			if err := json.Unmarshal(evt.Meta, &meta); err != nil {
				t.Fatalf("meta unmarshal: %v", err)
			}
			if meta.AdvisorModel == "" {
				continue
			}
			models[evt.ItemID] = append(models[evt.ItemID], meta.AdvisorModel)
			updates[evt.ItemID] = append(updates[evt.ItemID], meta.MetaUpdateOnly)
		}
	}
	return models, updates
}

// Headless stream-json: the call and result envelopes carry no
// iterations; the advisor model arrives on the closing message_delta
// and is stamped onto the call as a meta-only update.
func TestAdvisorModelStampedFromClosingMessageDelta(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	models, updates := advisorModelStamps(t,
		advisorMessageStart("", "msg-1"),
		advisorCallEnvelope("", "msg-1", "srvtoolu_a", noIterations),
		advisorResultEnvelope("", "msg-1", "srvtoolu_a"),
		advisorMessageDelta("", "claude-fable-5-1"),
	)
	if got := models["srvtoolu_a"]; len(got) != 1 || got[0] != "claude-fable-5-1" {
		t.Fatalf("advisor_model stamps: got %v, want [claude-fable-5-1] (never the parent's claude-opus-5-5)", got)
	}
	if got := updates["srvtoolu_a"]; !got[0] {
		t.Fatalf("late stamp must be meta_update_only so triage merges it onto the existing row")
	}
}

// Two advisor calls in one API message pair with the advisor
// iterations by call order.
func TestAdvisorModelPairsMultipleCallsByOrder(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	models, _ := advisorModelStamps(t,
		advisorMessageStart("", "msg-1"),
		advisorCallEnvelope("", "msg-1", "srvtoolu_a", noIterations),
		advisorResultEnvelope("", "msg-1", "srvtoolu_a"),
		advisorCallEnvelope("", "msg-1", "srvtoolu_b", noIterations),
		advisorResultEnvelope("", "msg-1", "srvtoolu_b"),
		advisorMessageDelta("", "claude-fable-5-1", "claude-sonnet-5-5"),
	)
	if got := models["srvtoolu_a"]; len(got) != 1 || got[0] != "claude-fable-5-1" {
		t.Fatalf("first call: got %v, want [claude-fable-5-1]", got)
	}
	if got := models["srvtoolu_b"]; len(got) != 1 || got[0] != "claude-sonnet-5-5" {
		t.Fatalf("second call: got %v, want [claude-sonnet-5-5]", got)
	}
}

// The TUI reconstructor forwards message_delta before the assembled
// envelope, which carries the full usage: the launch itself carries the
// model and nothing is stamped twice.
func TestAdvisorModelFromAssembledEnvelopeUsage(t *testing.T) {
	assembled := `{"type":"assistant","message":{"id":"msg-1","role":"assistant","model":"claude-opus-5-5","content":[` +
		`{"type":"server_tool_use","id":"srvtoolu_a","name":"advisor","input":{}},` +
		`{"type":"advisor_tool_result","tool_use_id":"srvtoolu_a","content":{"type":"advisor_redacted_result","encrypted_content":"opaque"}},` +
		`{"type":"server_tool_use","id":"srvtoolu_b","name":"advisor","input":{}},` +
		`{"type":"advisor_tool_result","tool_use_id":"srvtoolu_b","content":{"type":"advisor_redacted_result","encrypted_content":"opaque"}}` +
		`],"usage":` + advisorUsage("claude-fable-5-1", "claude-sonnet-5-5") + `}}`
	models, updates := advisorModelStamps(t,
		advisorMessageStart("", "msg-1"),
		advisorMessageDelta("", "claude-fable-5-1", "claude-sonnet-5-5"),
		assembled,
	)
	if got := models["srvtoolu_a"]; len(got) != 1 || got[0] != "claude-fable-5-1" || updates["srvtoolu_a"][0] {
		t.Fatalf("first call: got %v (updates %v), want one launch stamp claude-fable-5-1", got, updates["srvtoolu_a"])
	}
	if got := models["srvtoolu_b"]; len(got) != 1 || got[0] != "claude-sonnet-5-5" || updates["srvtoolu_b"][0] {
		t.Fatalf("second call: got %v (updates %v), want one launch stamp claude-sonnet-5-5", got, updates["srvtoolu_b"])
	}
}

// A message interrupted before its message_delta leaves its call
// unstamped; the next message's advisor iterations belong to that
// message's calls only.
func TestAdvisorModelInterruptedMessageNotStampedByNext(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	models, _ := advisorModelStamps(t,
		advisorMessageStart("", "msg-1"),
		advisorCallEnvelope("", "msg-1", "srvtoolu_old", noIterations),
		advisorMessageStart("", "msg-2"),
		advisorCallEnvelope("", "msg-2", "srvtoolu_new", noIterations),
		advisorMessageDelta("", "claude-fable-5-1"),
	)
	if got := models["srvtoolu_old"]; len(got) != 0 {
		t.Fatalf("interrupted call: got %v, want no stamp", got)
	}
	if got := models["srvtoolu_new"]; len(got) != 1 || got[0] != "claude-fable-5-1" {
		t.Fatalf("new call: got %v, want [claude-fable-5-1]", got)
	}
}

// Subagent and main-thread messages stream interleaved; each scope's
// message_delta stamps only its own calls.
func TestAdvisorModelScopesAreIndependent(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	models, _ := advisorModelStamps(t,
		advisorMessageStart("", "msg-main"),
		advisorMessageStart("toolu_agent", "msg-sub"),
		advisorCallEnvelope("", "msg-main", "srvtoolu_main", noIterations),
		advisorCallEnvelope("toolu_agent", "msg-sub", "srvtoolu_sub", noIterations),
		advisorMessageDelta("toolu_agent", "claude-sonnet-5-5"),
		advisorMessageDelta("", "claude-fable-5-1"),
	)
	if got := models["srvtoolu_main"]; len(got) != 1 || got[0] != "claude-fable-5-1" {
		t.Fatalf("main call: got %v, want [claude-fable-5-1]", got)
	}
	if got := models["srvtoolu_sub"]; len(got) != 1 || got[0] != "claude-sonnet-5-5" {
		t.Fatalf("subagent call: got %v, want [claude-sonnet-5-5]", got)
	}
}

// TUI order: the next message's message_delta precedes its envelope, so
// message_start must drop the interrupted message's pending call before
// the new advisor iterations arrive.
func TestAdvisorModelInterruptedMessageNotStampedByNextTUIOrder(t *testing.T) {
	noIterations := `{"input_tokens":3,"output_tokens":8}`
	models, _ := advisorModelStamps(t,
		advisorMessageStart("", "msg-1"),
		advisorCallEnvelope("", "msg-1", "srvtoolu_old", noIterations),
		advisorMessageStart("", "msg-2"),
		advisorMessageDelta("", "claude-fable-5-1"),
		advisorCallEnvelope("", "msg-2", "srvtoolu_new", advisorUsage("claude-fable-5-1")),
	)
	if got := models["srvtoolu_old"]; len(got) != 0 {
		t.Fatalf("interrupted call: got %v, want no stamp", got)
	}
	if got := models["srvtoolu_new"]; len(got) != 1 || got[0] != "claude-fable-5-1" {
		t.Fatalf("new call: got %v, want [claude-fable-5-1]", got)
	}
}

// Live CLI 2.1.284 capture (Opus parent, `advisorModel: "fable"`,
// --include-partial-messages): neither the call nor the redacted result
// envelope carries iterations; the closing message_delta reports the
// advisor as claude-fable-5-1.
func TestAdvisorModelLiveCapture2_1_284(t *testing.T) {
	data, err := os.ReadFile("testdata/advisor_redacted_2_1_284.ndjson")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	models, updates := advisorModelStamps(t, lines...)
	const id = "srvtoolu_01BcdDWwEkkCz7zagP6p2mh2"
	if got := models[id]; len(got) != 1 || got[0] != "claude-fable-5-1" || !updates[id][0] {
		t.Fatalf("advisor_model stamps: got %v (updates %v), want one meta-only claude-fable-5-1", got, updates[id])
	}
}
