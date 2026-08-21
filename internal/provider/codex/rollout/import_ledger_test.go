package rollout

import (
	"os"
	"path/filepath"
	"testing"
)

// Synthetic ledgers, hand-written against
// `ImportedExternalAgentSessionLedger` / `ImportedExternalAgentSessionRecord`
// at codex tag **rust-v0.149.0** (codex-rs/external-agent-migration/src/
// sessions/ledger.rs). Serde derives with no `rename_all`, so the JSON keys
// are the Rust field names verbatim. Nothing here reads a real `~/.codex`.

func writeLedger(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ExternalImportLedgerFile), []byte(body), 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	return home
}

func TestReadExternalImportLedgerRecoversClaudeProvenance(t *testing.T) {
	home := writeLedger(t, `{
  "records": [
    {
      "source_path": "/home/dev/.claude/projects/-home-dev-repo/aaaaaaaa-1111-4111-8111-111111111111.jsonl",
      "content_sha256": "d0f1...",
      "imported_thread_id": "33333333-3333-4333-8333-333333333333",
      "imported_at": 1786133870,
      "source_modified_at": 1786133860000000000,
      "connector_names": ["linear"],
      "title": "Fix the parser"
    }
  ],
  "detected_connector_records": [
    {"source_path": "/home/dev/.claude/projects/-home-dev-repo/other.jsonl", "connector_names": ["sentry"]}
  ]
}`)

	records, warnings := ReadExternalImportLedger(home)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	record, ok := records["33333333-3333-4333-8333-333333333333"]
	if !ok {
		t.Fatalf("no record keyed by the imported thread id: %+v", records)
	}
	if record.Agent != ExternalImportAgentClaude {
		t.Errorf("agent = %q, want %q", record.Agent, ExternalImportAgentClaude)
	}
	// The `.jsonl` stem IS the id AO's own Claude importer keys on, which is
	// what makes the duplicate-by-origin check possible.
	if record.SourceSessionID != "aaaaaaaa-1111-4111-8111-111111111111" {
		t.Errorf("source session id = %q", record.SourceSessionID)
	}
	// Upstream stores unix SECONDS here (`now_unix_seconds`); AO's wire is ms.
	if record.ImportedAt != 1786133870000 {
		t.Errorf("importedAt = %d, want the seconds value in ms", record.ImportedAt)
	}
	if record.Title != "Fix the parser" {
		t.Errorf("title = %q", record.Title)
	}
	if len(record.ConnectorNames) != 1 || record.ConnectorNames[0] != "linear" {
		t.Errorf("connector names = %v", record.ConnectorNames)
	}
	// `detected_connector_records` describes sources Codex NOTICED, not ones
	// it imported: no thread id, so nothing to key on.
	if len(records) != 1 {
		t.Errorf("records = %d, want only the imported one", len(records))
	}
}

func TestReadExternalImportLedgerRecognisesCursorAndUnknownLayouts(t *testing.T) {
	home := writeLedger(t, `{
  "records": [
    {"source_path": "/home/dev/.cursor/projects/repo-9f2/agent-transcripts/2026-08-07.md",
     "content_sha256": "a", "imported_thread_id": "cursor-thread", "imported_at": 1786133870,
     "connector_names": [], "title": null},
    {"source_path": "/var/tmp/exported-conversation.json",
     "content_sha256": "b", "imported_thread_id": "mystery-thread", "imported_at": 1786133871,
     "connector_names": [], "title": null}
  ]
}`)

	records, warnings := ReadExternalImportLedger(home)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v, want none", warnings)
	}
	if got := records["cursor-thread"].Agent; got != ExternalImportAgentCursor {
		t.Errorf("cursor agent = %q, want %q", got, ExternalImportAgentCursor)
	}
	// Cursor transcripts are not named by session id, so there is nothing to
	// recover and the duplicate check simply does not apply to them.
	if got := records["cursor-thread"].SourceSessionID; got != "" {
		t.Errorf("cursor source session id = %q, want empty", got)
	}
	// An unrecognised layout yields no agent rather than a guess.
	if got := records["mystery-thread"].Agent; got != "" {
		t.Errorf("unknown-layout agent = %q, want empty", got)
	}
	if got := records["mystery-thread"].SourcePath; got != "/var/tmp/exported-conversation.json" {
		t.Errorf("source path = %q", got)
	}
}

// A ledger written on Windows and read on Linux (a copied home, a WSL mount)
// must still resolve, because the agent is derived from path SEGMENTS.
func TestReadExternalImportLedgerHandlesWindowsPaths(t *testing.T) {
	home := writeLedger(t, `{"records":[
{"source_path":"C:\\Users\\dev\\.claude\\projects\\-c-repo\\bbbbbbbb-2222-4222-8222-222222222222.jsonl",
 "content_sha256":"c","imported_thread_id":"win-thread","imported_at":1786133870,
 "connector_names":[],"title":null}]}`)

	records, _ := ReadExternalImportLedger(home)
	if got := records["win-thread"].Agent; got != ExternalImportAgentClaude {
		t.Errorf("agent = %q, want %q", got, ExternalImportAgentClaude)
	}
	if got := records["win-thread"].SourceSessionID; got != "bbbbbbbb-2222-4222-8222-222222222222" {
		t.Errorf("source session id = %q", got)
	}
}

// Upstream's writer removes and re-pushes a re-imported source, so the newest
// record for a thread id is the LAST one.
func TestReadExternalImportLedgerKeepsTheLastRecordForAThread(t *testing.T) {
	home := writeLedger(t, `{"records":[
{"source_path":"/home/dev/.claude/projects/-r/old.jsonl","content_sha256":"a",
 "imported_thread_id":"t","imported_at":1000,"connector_names":[],"title":"old"},
{"source_path":"/home/dev/.claude/projects/-r/new.jsonl","content_sha256":"b",
 "imported_thread_id":"t","imported_at":2000,"connector_names":[],"title":"new"}]}`)

	records, _ := ReadExternalImportLedger(home)
	if got := records["t"].Title; got != "new" {
		t.Errorf("title = %q, want the last record's", got)
	}
}

// The common case: a Codex home that has never imported anything has no
// ledger at all. That must be silent, not a warning on every scan.
func TestReadExternalImportLedgerTreatsAbsenceAsSilence(t *testing.T) {
	records, warnings := ReadExternalImportLedger(t.TempDir())
	if len(records) != 0 || len(warnings) != 0 {
		t.Fatalf("records = %v, warnings = %+v, want both empty", records, warnings)
	}
	if records, warnings := ReadExternalImportLedger("  "); records != nil || warnings != nil {
		t.Fatalf("empty home should read nothing: %v / %+v", records, warnings)
	}
}

// Everything that is not absence costs the labels and raises exactly one
// warning. It must never fail a listing: the badge is decoration.
func TestReadExternalImportLedgerDegradesOnCorruption(t *testing.T) {
	for name, body := range map[string]string{
		"not json":                `{"records": [`,
		"wrong shape":             `["a", "b"]`,
		"records is not an array": `{"records": 7}`,
	} {
		t.Run(name, func(t *testing.T) {
			records, warnings := ReadExternalImportLedger(writeLedger(t, body))
			if len(records) != 0 {
				t.Errorf("records = %v, want none", records)
			}
			if len(warnings) != 1 || warnings[0].Code != WarnImportLedgerUnreadable {
				t.Fatalf("warnings = %+v, want one %s", warnings, WarnImportLedgerUnreadable)
			}
		})
	}
}

func TestReadExternalImportLedgerRefusesAnOversizedFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ExternalImportLedgerFile)
	if err := os.WriteFile(path, make([]byte, externalImportLedgerMaxBytes+1), 0o644); err != nil {
		t.Fatalf("write oversized ledger: %v", err)
	}
	records, warnings := ReadExternalImportLedger(home)
	if len(records) != 0 {
		t.Errorf("records = %v, want none", records)
	}
	if len(warnings) != 1 || warnings[0].Code != WarnImportLedgerUnreadable {
		t.Fatalf("warnings = %+v, want one %s", warnings, WarnImportLedgerUnreadable)
	}
}

// A record with no thread id names nothing a listing row can join to.
func TestReadExternalImportLedgerSkipsRecordsWithNoThreadID(t *testing.T) {
	home := writeLedger(t, `{"records":[
{"source_path":"/home/dev/.claude/projects/-r/a.jsonl","content_sha256":"a",
 "imported_thread_id":"","imported_at":1000,"connector_names":[],"title":null}]}`)

	records, warnings := ReadExternalImportLedger(home)
	if len(records) != 0 {
		t.Errorf("records = %v, want none", records)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none — a skipped record is not a broken file", warnings)
	}
}

// A RELOCATED Claude home is not named `.claude`, so the labels cannot key on
// that segment alone. AO itself supports one — `sessionimport.Deps` takes the
// projects dir as an argument — so this is the layout, not an edge case.
func TestReadExternalImportLedgerRecognisesARelocatedClaudeHomeByLayout(t *testing.T) {
	home := writeLedger(t, `{"records":[
{"source_path":"/mnt/data/agent-homes/claude/projects/-mnt-repo/cccccccc-3333-4333-8333-333333333333.jsonl",
 "content_sha256":"a","imported_thread_id":"relocated","imported_at":1786133870,
 "connector_names":[],"title":null},
{"source_path":"/mnt/data/agent-homes/claude/projects/-mnt-repo/notes.jsonl",
 "content_sha256":"b","imported_thread_id":"not-a-session","imported_at":1786133870,
 "connector_names":[],"title":null}]}`)

	records, _ := ReadExternalImportLedger(home)
	if got := records["relocated"].Agent; got != ExternalImportAgentClaude {
		t.Errorf("agent = %q, want %q", got, ExternalImportAgentClaude)
	}
	if got := records["relocated"].SourceSessionID; got != "cccccccc-3333-4333-8333-333333333333" {
		t.Errorf("source session id = %q", got)
	}
	// The stem must pass the SAME admission rule the Claude lister applies, or
	// a recovered id could match a session that lister would never produce.
	if got := records["not-a-session"].Agent; got != "" {
		t.Errorf("non-UUID stem should not be recognised by layout alone, got %q", got)
	}
	if got := records["not-a-session"].SourceSessionID; got != "" {
		t.Errorf("source session id = %q, want empty", got)
	}
}
