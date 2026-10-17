package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-overflow/internal/harness/control"
)

// injectItems answers `thread/inject_items {threadId, items}`, which appends
// raw Responses API items to a loaded thread's model-visible history without
// starting a turn. Upstream answers `{}`; an unknown thread and an item that
// is not a response item are invalid requests (-32600), raised before
// anything is recorded (app-server turn_processor.rs
// thread_inject_items_response_inner, rust-v0.162.1).
//
// The mock has no model, so the observable effect is the report: each message
// item's role and text reach the spec as ReportInjectedItems, posted before
// the answer.
func (a *codexAdapter) injectItems(id json.RawMessage, params json.RawMessage) {
	threadID := readParamString(params, "threadId")
	a.mu.Lock()
	loaded := a.loadedThread
	a.mu.Unlock()
	if !loaded || threadID == "" || threadID != a.e.currentVars()["THREAD_ID"] {
		a.writeRPCError(id, -32600, "thread not found: "+threadID)
		return
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(readParamRaw(params, "items"), &raw); err != nil {
		a.writeRPCError(id, -32600, fmt.Sprintf("items is not a list of response items: %v", err))
		return
	}
	items := make([]control.InjectedItem, 0, len(raw))
	for index, value := range raw {
		item, err := injectedMessage(value)
		if err != nil {
			a.writeRPCError(id, -32600, fmt.Sprintf("items[%d] is not a valid response item: %v", index, err))
			return
		}
		items = append(items, item)
	}
	a.e.rep.report(control.Report{
		Kind:          control.ReportInjectedItems,
		SessionRef:    threadID,
		InjectedItems: items,
	})
	a.writeRPCResult(id, `{}`)
}

// injectedMessage reads one `message` response item. Other item types are
// valid upstream, but nothing AO sends uses them, so the mock refuses them
// rather than report an item it cannot describe.
func injectedMessage(raw json.RawMessage) (control.InjectedItem, error) {
	var item struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return control.InjectedItem{}, err
	}
	if item.Type != "message" || item.Role == "" {
		return control.InjectedItem{}, fmt.Errorf("the mock models message items only, got type %q role %q", item.Type, item.Role)
	}
	texts := make([]string, 0, len(item.Content))
	for _, part := range item.Content {
		if part.Type != "input_text" && part.Type != "output_text" {
			return control.InjectedItem{}, fmt.Errorf("unsupported content part %q", part.Type)
		}
		texts = append(texts, part.Text)
	}
	return control.InjectedItem{Role: item.Role, Text: strings.Join(texts, "")}, nil
}
