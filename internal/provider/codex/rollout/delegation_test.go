package rollout

import (
	"encoding/json"
	"testing"

	"agent-overflow/internal/provider"
)

// Delegated-input fixtures, hand-written against rust-v0.162.1: the
// response item is `ResponseItem::FunctionCallOutput` (models.rs; no
// `call_id`, an `fco` id) and the twin is `TurnItem::FunctionCallOutput`
// (protocol/src/items.rs), both written by record_pending_input
// (core/src/hook_runtime.rs). The envelope text is the one in
// thread-store/src/local/tests/delegated_preview_tests.rs.
const (
	delegatedEnvelope = "<codex_delegation>\n  <source_thread_id>source-thread</source_thread_id>\n" +
		"  <input>Inspect &lt;main&gt; &amp; report findings.</input>\n</codex_delegation>"
	delegatedPrompt = "Inspect <main> & report findings."
)

func delegatedResponseLine(t *testing.T, id, namespace, name string, output any) string {
	t.Helper()
	payload := map[string]any{"type": "function_call_output", "id": id, "name": name, "output": output}
	if namespace != "" {
		payload["namespace"] = namespace
	}
	encoded, err := json.Marshal(map[string]any{
		"timestamp": "2026-08-07T19:07:47.000Z",
		"type":      "response_item",
		"payload":   payload,
	})
	if err != nil {
		t.Fatalf("encode fixture line: %v", err)
	}
	return string(encoded)
}

func delegatedItemLine(t *testing.T, id, namespace, name string, output any) string {
	t.Helper()
	item := map[string]any{"type": "FunctionCallOutput", "id": id, "name": name, "output": output}
	if namespace != "" {
		item["namespace"] = namespace
	}
	return pagLine(t, 4, "2026-08-07T19:07:47.010Z", item, 1786133867000, 1786133867010)
}

func userTexts(t *testing.T, res ParseResult) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range res.Events {
		if e.Kind != provider.EventUserText {
			continue
		}
		meta := map[string]any{}
		if len(e.Meta) > 0 {
			if err := json.Unmarshal(e.Meta, &meta); err != nil {
				t.Fatalf("decode user text meta: %v", err)
			}
		}
		meta["content"] = e.Content
		meta["itemID"] = e.ItemID
		out = append(out, meta)
	}
	return out
}

// The thread opens with the delegated prompt, once, as a user row that
// says another Codex thread wrote it. Before, the response item was read as
// a tool output with no call (a detail-unavailable tool row plus a warning)
// and the item was counted unknown, so the thread had no opening message.
func TestParseDelegatedInputImportsAsTheOpeningMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta string
	}{
		{name: "paginated", meta: paginatedMetaLine},
		// policy.rs keeps TurnItem::FunctionCallOutput in legacy files too.
		{name: "legacy", meta: metaLine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRollout(t, testSessionID,
				tc.meta, turnContextLine, taskStartedLine,
				delegatedResponseLine(t, "fco_1", "codex_app", "create_thread", delegatedEnvelope),
				delegatedItemLine(t, "fco_1", "codex_app", "create_thread", delegatedEnvelope),
				pagAgentMirrorLine,
				taskCompleteLn,
			)
			res := parseFixture(t, path)

			users := userTexts(t, res)
			if len(users) != 1 {
				t.Fatalf("user rows = %v, want exactly the delegated prompt", users)
			}
			if users[0]["content"] != delegatedPrompt {
				t.Errorf("content = %q, want %q", users[0]["content"], delegatedPrompt)
			}
			if users[0]["origin"] != DelegatedInputOrigin {
				t.Errorf("origin = %v, want %s", users[0]["origin"], DelegatedInputOrigin)
			}
			if users[0][MetaDelegationSourceThreadKey] != "source-thread" {
				t.Errorf("source thread = %v, want source-thread", users[0][MetaDelegationSourceThreadKey])
			}
			if users[0]["itemID"] != "fco_1" {
				t.Errorf("item id = %v, want fco_1", users[0]["itemID"])
			}
			if n := countKind(res.Events, provider.EventToolComplete) + countKind(res.Events, provider.EventToolStart); n != 0 {
				t.Errorf("tool events = %d, want none: %v", n, kinds(res.Events))
			}
			if len(res.Warnings) != 0 {
				t.Errorf("warnings = %+v, want none", res.Warnings)
			}
			if len(res.UnknownTypes) != 0 {
				t.Errorf("unknown types = %v, want none", res.UnknownTypes)
			}
		})
	}
}

// A file with only the item (no response-item mirror) still opens with the
// prompt. `send_message_to_thread` and the TUI namespace are the same input,
// and a content-items body reads as its joined text, as to_text() does.
func TestParseDelegatedInputFromTheItemAlone(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		delegatedItemLine(t, "fco_2", "codex_tui", "send_message_to_thread", []any{
			map[string]any{"type": "input_text", "text": "first line"},
			map[string]any{"type": "input_text", "text": "second line"},
		}),
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	users := userTexts(t, res)
	if len(users) != 1 || users[0]["content"] != "first line\nsecond line" {
		t.Fatalf("user rows = %v, want the joined content items", users)
	}
	if _, present := users[0][MetaDelegationSourceThreadKey]; present {
		t.Errorf("unwrapped text carried a source thread: %v", users[0])
	}
}

// Only Codex's delegation tools are prompts. Any other output submitted as
// input keeps its previous handling: an orphan tool output, and an unknown
// item, so drift stays visible.
func TestParseOtherSubmittedOutputsAreNotDelegatedInput(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		delegatedResponseLine(t, "fco_3", "other", "create_thread", delegatedEnvelope),
		delegatedItemLine(t, "fco_3", "other", "create_thread", delegatedEnvelope),
		delegatedResponseLine(t, "fco_4", "codex_app", "shell", delegatedEnvelope),
		taskCompleteLn,
	)
	res := parseFixture(t, path)

	if users := userTexts(t, res); len(users) != 0 {
		t.Fatalf("user rows = %v, want none", users)
	}
	if got := res.UnknownTypes["event_msg/item_completed/FunctionCallOutput"]; got != 1 {
		t.Errorf("unknown FunctionCallOutput items = %d, want 1 (%v)", got, res.UnknownTypes)
	}
	if got := countKind(res.Events, provider.EventToolComplete); got != 2 {
		t.Errorf("orphan tool completions = %d, want 2", got)
	}
}

// delegatedInputText follows delegated_output_preview byte for byte, so the
// opening message equals the preview the thread is listed under.
// Cases from codex-rs/state/src/delegated_preview_tests.rs.
func TestDelegatedInputTextMatchesCodexPreview(t *testing.T) {
	t.Parallel()
	wrapped := "<codex_delegation>\n  <source_thread_id>source</source_thread_id>\n  <input>Check &lt;main&gt; &amp; &amp;lt;literal&amp;gt;</input>\n</codex_delegation>"
	for _, tc := range []struct {
		text, wantInput, wantSource string
	}{
		{wrapped, "Check <main> & &lt;literal&gt;", "source"},
		{"plain &lt;text&gt;", "plain &lt;text&gt;", ""},
		{"<codex_delegation>incomplete", "<codex_delegation>incomplete", ""},
	} {
		input, source := delegatedInputText(tc.text)
		if input != tc.wantInput || source != tc.wantSource {
			t.Errorf("delegatedInputText(%q) = (%q, %q), want (%q, %q)", tc.text, input, source, tc.wantInput, tc.wantSource)
		}
	}
}

// A blank delegated input is no message, as upstream's preview is None.
func TestParseBlankDelegatedInputWritesNoRow(t *testing.T) {
	path := writeRollout(t, testSessionID,
		paginatedMetaLine, taskStartedLine,
		delegatedResponseLine(t, "fco_5", "codex_tui", "create_thread", "  "),
		taskCompleteLn,
	)
	res := parseFixture(t, path)
	if users := userTexts(t, res); len(users) != 0 {
		t.Fatalf("user rows = %v, want none", users)
	}
}
