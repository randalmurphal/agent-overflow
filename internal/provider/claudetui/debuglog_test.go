package claudetui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/logging"
)

func TestRunePrefix(t *testing.T) {
	if got := runePrefix("hello", 10); got != "hello" {
		t.Errorf("under limit = %q, want unchanged", got)
	}
	if got := runePrefix("hello", 3); got != "hel…" {
		t.Errorf("truncate = %q, want hel…", got)
	}
	if got := runePrefix("héllo", 2); got != "hé…" {
		t.Errorf("multibyte = %q, want hé… (no split)", got)
	}
}

func TestCapStrings(t *testing.T) {
	if got := capStrings([]string{"a", "b"}, 5); len(got) != 2 {
		t.Errorf("under cap len = %d, want 2", len(got))
	}
	if got := capStrings([]string{"a", "b", "c"}, 2); len(got) != 2 {
		t.Errorf("over cap len = %d, want 2", len(got))
	}
}

// TestLogClassifyWritesCredentialFreeSummary drives one title-generation-shaped
// auxiliary call through the classify logger and asserts the NDJSON entry shape:
// the right class, status, counts, the system marker, and the <session>
// discriminator in last_user_prefix — the fields the phantom investigation
// reads — with no credential-header material.
func TestLogClassifyWritesCredentialFreeSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.ndjson")
	lg, err := logging.NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	s := &Session{threadID: "thread-1", evlog: lg}

	body := `{"model":"claude-haiku","max_tokens":512,"tools":[],` +
		`"system":[{"type":"text","text":"Generate a concise, sentence-case title"}],` +
		`"messages":[{"role":"user","content":"<session>mermaid stuff</session>"}]}`
	s.logClassify(classAuxiliary, 200, []byte(body))
	if err := lg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entry := readSingleEntry(t, path)
	if entry.Direction != "classify" || entry.Provider != logProvider || entry.ThreadID != "thread-1" {
		t.Errorf("entry envelope wrong: %+v", entry)
	}
	var sum classifySummary
	if err := json.Unmarshal([]byte(entry.Data), &sum); err != nil {
		t.Fatalf("unmarshal summary: %v", err)
	}
	if sum.Class != "auxiliary" {
		t.Errorf("class = %q, want auxiliary", sum.Class)
	}
	if sum.Status != 200 || sum.NumTools != 0 || sum.NumMsgs != 1 {
		t.Errorf("shape wrong: %+v", sum)
	}
	if !strings.Contains(sum.System, "Generate a concise") {
		t.Errorf("system_prefix missing title marker: %q", sum.System)
	}
	if !strings.Contains(sum.LastUser, "<session>") {
		t.Errorf("last_user_prefix missing <session> discriminator: %q", sum.LastUser)
	}
	for _, secret := range []string{"Authorization", "Bearer", "x-api-key"} {
		if strings.Contains(string(entry.Data), secret) {
			t.Errorf("classify log leaked credential-shaped text %q: %q", secret, entry.Data)
		}
	}
}

// TestLogEnvelopeWritesParserFeed asserts an envelope is logged verbatim under
// direction "in" — the headless raw-stdout analog.
func TestLogEnvelopeWritesParserFeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ev.ndjson")
	lg, err := logging.NewLogger(path, 0)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	s := &Session{threadID: "thread-x", evlog: lg}
	env := `{"type":"stream_event","event":{"type":"content_block_delta"}}`
	s.logEnvelope(json.RawMessage(env))
	if err := lg.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entry := readSingleEntry(t, path)
	if entry.Direction != "in" || entry.Provider != logProvider {
		t.Errorf("wrong envelope entry: %+v", entry)
	}
	if string(entry.Data) != env {
		t.Errorf("data = %q, want %q", entry.Data, env)
	}
}

// TestDebugLogNilLoggerNoop guards the production posture: with no logger the
// helpers must be silent no-ops, never panicking.
func TestDebugLogNilLoggerNoop(t *testing.T) {
	s := &Session{threadID: "t"} // evlog nil
	s.logEnvelope(json.RawMessage(`{"type":"x"}`))
	s.logClassify(classAgent, 200, []byte(`{"model":"x","max_tokens":9}`))
}

func readSingleEntry(t *testing.T, path string) logging.ProviderEventEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 log line, got %d: %q", len(lines), raw)
	}
	var entry logging.ProviderEventEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal entry: %v", err)
	}
	return entry
}
