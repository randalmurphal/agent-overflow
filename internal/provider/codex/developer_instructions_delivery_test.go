package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/testutil/mockexec"
)

// writeInjectAppServer is a mock app-server that answers `config/read` with
// the given developer instructions, `failMethod` (when set) with a JSON-RPC
// error, and every other request with the thread "mock-thread". Requests
// are appended to requestLog.
func writeInjectAppServer(t *testing.T, dir, requestLog, configured, failMethod string) string {
	t.Helper()
	if strings.ContainsAny(configured, "'\n\"") {
		t.Fatalf("configured instructions %q cannot ride this shell script", configured)
	}
	failClause := ""
	if failMethod != "" {
		failClause = "  if printf '%s' \"$line\" | grep -q '\"method\":\"" + failMethod + "\"'; then\n" +
			"    printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32600,\"message\":\"refused " + failMethod + "\"}}\\n' \"$id\"\n" +
			"    continue\n" +
			"  fi\n"
	}
	script := "#!/bin/bash\n" +
		"while IFS= read -r line; do\n" +
		"  printf '%s\\n' \"$line\" >> '" + requestLog + "'\n" +
		"  id=$(printf '%s' \"$line\" | grep -o '\"id\":[0-9]*' | head -1 | grep -o '[0-9]*')\n" +
		"  if [ -z \"$id\" ]; then continue; fi\n" +
		failClause +
		"  if printf '%s' \"$line\" | grep -q '\"method\":\"config/read\"'; then\n" +
		"    printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"config\":{\"developer_instructions\":\"" + configured + "\"}}}\\n' \"$id\"\n" +
		"  else\n" +
		"    printf '{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"thread\":{\"id\":\"mock-thread\"}}}\\n' \"$id\"\n" +
		"  fi\n" +
		"done\n"
	binary := filepath.Join(dir, "codex")
	mockexec.Write(t, binary, script)
	return binary
}

// requestsFor returns the params of every logged request for method, in
// order.
func requestsFor(t *testing.T, logPath, method string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read request log: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		var req struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal([]byte(line), &req) == nil && req.Method == method {
			out = append(out, req.Params)
		}
	}
	return out
}

// injectedText is the text of the one developer message an inject request
// carried.
func injectedText(t *testing.T, params map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(params["items"])
	var items []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 1 || len(items[0].Content) != 1 {
		t.Fatalf("inject items = %s, want one message with one content part", raw)
	}
	item := items[0]
	if item.Type != "message" || item.Role != "developer" || item.Content[0].Type != "input_text" {
		t.Fatalf("inject item = %+v, want a developer message of input_text", item)
	}
	if params["threadId"] != "mock-thread" {
		t.Fatalf("inject threadId = %v, want the session's thread", params["threadId"])
	}
	return item.Content[0].Text
}

type deliveryRecorder struct {
	mu      sync.Mutex
	records []string
	errors  []string
	fail    error
}

func (r *deliveryRecorder) delivery(received string) *DeveloperInstructionsDelivery {
	return &DeveloperInstructionsDelivery{
		Received: received,
		Record: func(codexThreadID, digest string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.records = append(r.records, codexThreadID+" "+digest)
			return r.fail
		},
	}
}

func (r *deliveryRecorder) onEvent(evt provider.ProviderEvent) {
	if evt.Kind != provider.EventError {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, evt.Content)
}

func (r *deliveryRecorder) snapshot() (records, errs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.records...), append([]string(nil), r.errors...)
}

func startDeliverySession(t *testing.T, binary, dir, guide, resume string, recorder *deliveryRecorder, received string) *Session {
	t.Helper()
	sess, err := NewSession(context.Background(), "thread-1", Config{
		Binary:                        binary,
		WorkDir:                       dir,
		ResumeThreadID:                resume,
		DeveloperInstructions:         guide,
		DeveloperInstructionsDelivery: recorder.delivery(received),
	}, recorder.onEvent)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// A new thread reads the composed text in its initial context: nothing is
// injected, and the digest of that text is recorded for the thread.
func TestFreshStartRecordsTheComposedTextAndInjectsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeInjectAppServer(t, dir, requestLog, "configured", "")
	recorder := &deliveryRecorder{}

	startDeliverySession(t, binary, dir, "guide", "", recorder, "ignored on a fresh start")

	if got := requestsFor(t, requestLog, "thread/inject_items"); len(got) != 0 {
		t.Fatalf("a fresh start injected %v", got)
	}
	records, errs := recorder.snapshot()
	want := "mock-thread " + DeveloperInstructionsDigest("configured\n\nguide")
	if len(records) != 1 || records[0] != want || len(errs) != 0 {
		t.Fatalf("records = %v, errors = %v; want one record of the composed text", records, errs)
	}
}

// A resume whose composed text is the one the thread last received injects
// nothing and records nothing.
func TestResumeWithUnchangedTextInjectsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeInjectAppServer(t, dir, requestLog, "configured", "")
	recorder := &deliveryRecorder{}

	startDeliverySession(t, binary, dir, "guide", "mock-thread", recorder, DeveloperInstructionsDigest("configured\n\nguide"))

	if got := requestsFor(t, requestLog, "thread/inject_items"); len(got) != 0 {
		t.Fatalf("an unchanged text was injected: %v", got)
	}
	if records, errs := recorder.snapshot(); len(records) != 0 || len(errs) != 0 {
		t.Fatalf("records = %v, errors = %v; want none", records, errs)
	}
	// The thread-level value still rides the resume, for the next compaction.
	if got := findRequestParams(t, requestLog, "thread/resume")["developerInstructions"]; got != "configured\n\nguide" {
		t.Fatalf("resume developerInstructions = %v", got)
	}
}

// A resume whose composed text changed injects it once, after the resume
// and before the session is returned, and records the new digest.
func TestResumeWithChangedTextInjectsItOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, received string
	}{
		{"changed", DeveloperInstructionsDigest("configured\n\nold guide")},
		{"never recorded", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			requestLog := filepath.Join(dir, "requests.jsonl")
			binary := writeInjectAppServer(t, dir, requestLog, "configured", "")
			recorder := &deliveryRecorder{}

			sess := startDeliverySession(t, binary, dir, "new guide", "mock-thread", recorder, tc.received)

			injects := requestsFor(t, requestLog, "thread/inject_items")
			if len(injects) != 1 {
				t.Fatalf("injects = %v, want exactly one", injects)
			}
			text := injectedText(t, injects[0])
			if !strings.HasSuffix(text, "\n\nconfigured\n\nnew guide") || !strings.Contains(text, "replaces the developer instructions given at its start") {
				t.Fatalf("injected text = %q, want the replacement notice and the whole composed text", text)
			}
			records, errs := recorder.snapshot()
			want := "mock-thread " + DeveloperInstructionsDigest("configured\n\nnew guide")
			if len(records) != 1 || records[0] != want || len(errs) != 0 {
				t.Fatalf("records = %v, errors = %v; want the new digest recorded once", records, errs)
			}

			// The same text again, as a turn-start check would ask: nothing more.
			if err := sess.EnsureDeveloperInstructions(context.Background(), "new guide"); err != nil {
				t.Fatalf("EnsureDeveloperInstructions: %v", err)
			}
			if got := requestsFor(t, requestLog, "thread/inject_items"); len(got) != 1 {
				t.Fatalf("an unchanged text was injected again: %d injects", len(got))
			}
			// A further change injects once more.
			if err := sess.EnsureDeveloperInstructions(context.Background(), "third guide"); err != nil {
				t.Fatalf("EnsureDeveloperInstructions: %v", err)
			}
			injects = requestsFor(t, requestLog, "thread/inject_items")
			if len(injects) != 2 || !strings.HasSuffix(injectedText(t, injects[1]), "\n\nconfigured\n\nthird guide") {
				t.Fatalf("injects after a second change = %v", injects)
			}
		})
	}
}

// The guide switched off: the text the thread should now read is the
// user's configured value alone, still read with config/read, and with
// nothing configured either the message says the earlier text no longer
// applies. The resume itself still omits the override.
func TestResumeWithTheGuideRemovedInjectsWhatRemains(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, configured, wantSuffix string
	}{
		{"configured text remains", "configured", "\n\nconfigured"},
		{"nothing remains", "", "no longer apply. Other context, such as permissions and the environment, is unchanged."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			requestLog := filepath.Join(dir, "requests.jsonl")
			binary := writeInjectAppServer(t, dir, requestLog, tc.configured, "")
			recorder := &deliveryRecorder{}

			startDeliverySession(t, binary, dir, "", "mock-thread", recorder, DeveloperInstructionsDigest("guide"))

			if _, present := findRequestParams(t, requestLog, "thread/resume")["developerInstructions"]; present {
				t.Fatal("a resume with no guide carried an override")
			}
			injects := requestsFor(t, requestLog, "thread/inject_items")
			if len(injects) != 1 || !strings.HasSuffix(injectedText(t, injects[0]), tc.wantSuffix) {
				t.Fatalf("injects = %v, want one ending %q", injects, tc.wantSuffix)
			}
			records, _ := recorder.snapshot()
			if len(records) != 1 || records[0] != "mock-thread "+DeveloperInstructionsDigest(tc.configured) {
				t.Fatalf("records = %v", records)
			}
		})
	}
}

// A session that does not track delivery (a throwaway that only forks or
// reverts) neither reads config for a check nor injects.
func TestUntrackedResumeNeverInjects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeInjectAppServer(t, dir, requestLog, "configured", "")

	sess, err := NewSession(context.Background(), "thread-1", Config{
		Binary: binary, WorkDir: dir, ResumeThreadID: "mock-thread",
	}, func(provider.ProviderEvent) {})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if err := sess.EnsureDeveloperInstructions(context.Background(), "guide"); err != nil {
		t.Fatalf("EnsureDeveloperInstructions: %v", err)
	}
	if got := requestsFor(t, requestLog, "thread/inject_items"); len(got) != 0 {
		t.Fatalf("an untracked session injected %v", got)
	}
}

// A refused injection leaves the session usable, records nothing (so the
// next start tries again), and tells the user.
func TestFailedInjectionIsReportedAndNotRecorded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeInjectAppServer(t, dir, requestLog, "", "thread/inject_items")
	recorder := &deliveryRecorder{}

	startDeliverySession(t, binary, dir, "guide", "mock-thread", recorder, "")

	records, errs := recorder.snapshot()
	if len(records) != 0 {
		t.Fatalf("a refused injection was recorded: %v", records)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "refused thread/inject_items") || !strings.Contains(errs[0], "next session start") {
		t.Fatalf("errors = %v, want one naming the refusal and the retry", errs)
	}
}

// A record that cannot be written is reported; the injection stands.
func TestFailedRecordIsReported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeInjectAppServer(t, dir, requestLog, "", "")
	recorder := &deliveryRecorder{fail: errors.New("disk full")}

	startDeliverySession(t, binary, dir, "guide", "mock-thread", recorder, "")

	if got := requestsFor(t, requestLog, "thread/inject_items"); len(got) != 1 {
		t.Fatalf("injects = %v, want one", got)
	}
	if _, errs := recorder.snapshot(); len(errs) != 1 || !strings.Contains(errs[0], "disk full") {
		t.Fatalf("errors = %v, want the failed record reported", errs)
	}
}

// A config read that fails leaves the composed text unknown: no injection
// and no record, and one error that says why.
func TestUnreadableConfigSkipsTheCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	requestLog := filepath.Join(dir, "requests.jsonl")
	binary := writeInjectAppServer(t, dir, requestLog, "", "config/read")
	recorder := &deliveryRecorder{}

	startDeliverySession(t, binary, dir, "", "mock-thread", recorder, DeveloperInstructionsDigest("guide"))

	if got := requestsFor(t, requestLog, "thread/inject_items"); len(got) != 0 {
		t.Fatalf("an unknown composition was injected: %v", got)
	}
	records, errs := recorder.snapshot()
	if len(records) != 0 || len(errs) != 1 || !strings.Contains(errs[0], "may not reach this thread") {
		t.Fatalf("records = %v, errors = %v", records, errs)
	}
}
