package main

import (
	"bytes"
	"testing"

	"agent-overflow/internal/harness/control"
)

// thread/inject_items answers `{}` for the loaded thread and reports each
// message's role and text before the answer; an unknown thread or an item
// it cannot read is refused the way upstream refuses it, with nothing
// reported.
func TestCodexInjectItemsReportsTheMessages(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	a := newCodexUnitAdapter(t, &buf, nil)
	var reports []control.Report
	a.e.rep.observe = func(rep control.Report) {
		if rep.Kind == control.ReportInjectedItems {
			reports = append(reports, rep)
		}
	}

	inject := `{"threadId":"th-unit","items":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"new "},{"type":"input_text","text":"instructions"}]}]}`
	if frame := codexRequest(t, a, &buf, "thread/inject_items", inject); frame.Error == nil || frame.Error.Code != -32600 {
		t.Fatalf("inject before any thread was loaded = %+v, want -32600", frame)
	}

	codexRequest(t, a, &buf, "thread/resume", `{"threadId":"th-unit"}`)
	frame := codexRequest(t, a, &buf, "thread/inject_items", inject)
	if frame.Error != nil || string(frame.Result) != "{}" {
		t.Fatalf("inject on the loaded thread = %+v, want {}", frame)
	}
	if len(reports) != 1 || reports[0].SessionRef != "th-unit" ||
		len(reports[0].InjectedItems) != 1 ||
		reports[0].InjectedItems[0] != (control.InjectedItem{Role: "developer", Text: "new instructions"}) {
		t.Fatalf("reports = %+v, want one developer message on th-unit", reports)
	}

	for _, bad := range []string{
		`{"threadId":"th-other","items":[]}`,
		`{"threadId":"th-unit","items":[{"type":"function_call"}]}`,
		`{"threadId":"th-unit","items":[{"type":"message","role":"developer","content":[{"type":"input_image"}]}]}`,
		`{"threadId":"th-unit","items":"not a list"}`,
	} {
		if frame := codexRequest(t, a, &buf, "thread/inject_items", bad); frame.Error == nil || frame.Error.Code != -32600 {
			t.Errorf("inject %s = %+v, want -32600", bad, frame)
		}
	}
	if len(reports) != 1 {
		t.Fatalf("a refused inject was reported: %+v", reports)
	}
}
