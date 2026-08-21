package sessionimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/provider/codex/rollout"
)

// import_one_test.go — the single-handle rule on the IMPORT side.
//
// The refresh path has the same rule and the same reasoning
// (TestCodexRefreshReadsIdentityAndTailFromTheHeldHandleNotThePath), but the
// consequence here is worse: the import is what RECORDS the pair a later
// refresh trusts. Reading the events through one open and the fingerprint
// through another lets a Codex history migration land between them, and the
// row then claims the replacement's fingerprint against the original's byte
// offset. Every later refresh compares that fingerprint, finds it matching,
// and resumes the replacement from an offset that addresses a different
// record — the fingerprint reads as PROOF while naming the wrong file.
//
// The swap cannot be interposed between the two reads in situ: nothing yields
// between them and a racing rename would land in an unknown window. So this
// pins the property at the boundary codexImportSource depends on — given one
// handle, both answers describe the fd, and neither re-resolves the path.

func TestCodexImportReadsEventsAndIdentityFromTheHeldHandleNotThePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-08-07T15-07-44-"+codexThreadA+".jsonl")

	originalHeader := `{"timestamp":"2026-08-07T15:07:44.000Z","type":"session_meta",` +
		`"payload":{"id":"` + codexThreadA + `","cwd":"/fixture/repo",` +
		`"originator":"codex_cli","cli_version":"0.149.0","history_mode":"legacy"}}`
	original := originalHeader + "\n" +
		`{"timestamp":"2026-08-07T15:07:45.000Z","type":"event_msg",` +
		`"payload":{"type":"task_started","turn_id":"turn-1"}}` + "\n" +
		`{"timestamp":"2026-08-07T15:07:46.000Z","type":"event_msg",` +
		`"payload":{"type":"user_message","message":"original history"}}` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("write original rollout: %v", err)
	}

	// The handle importCodex holds, opened before anything replaces the path.
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open rollout: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })

	// The migration: a paginated header and paginated records, published
	// atomically over the same path.
	replacement := `{"timestamp":"2026-08-07T15:08:00.000Z","type":"session_meta",` +
		`"payload":{"id":"` + codexThreadA + `","cwd":"/fixture/repo",` +
		`"originator":"codex_cli","cli_version":"0.149.0","history_mode":"paginated"}}` + "\n" +
		`{"timestamp":"2026-08-07T15:08:01.000Z","type":"event_msg",` +
		`"payload":{"type":"task_started","turn_id":"turn-1"}}` + "\n" +
		`{"timestamp":"2026-08-07T15:08:02.000Z","type":"response_item",` +
		`"payload":{"type":"message","id":"m1","role":"user",` +
		`"content":[{"type":"input_text","text":"migrated history"}]}}` + "\n"
	staged := filepath.Join(dir, "staged.jsonl")
	if err := os.WriteFile(staged, []byte(replacement), 0o644); err != nil {
		t.Fatalf("write replacement rollout: %v", err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatalf("publish replacement over the rollout path: %v", err)
	}

	parsed, identity, err := codexImportSource(context.Background(), file, path, codexThreadA)
	if err != nil {
		t.Fatalf("codexImportSource: %v", err)
	}
	if got := onlyUserText(t, parsed.Events); got != "original history" {
		t.Fatalf("events came from %q, want the original history — the parse followed the path", got)
	}
	if identity.HistoryMode != "legacy" {
		t.Fatalf("HistoryMode = %q, want legacy — the identity read followed the path",
			identity.HistoryMode)
	}
	if identity.MetaHash == "" {
		t.Fatal("no fingerprint recorded, so the refresh guard would never arm")
	}
	// The offset the row would record has to describe the SAME file the
	// fingerprint names: it is the whole point of the pair.
	if parsed.EndOffset != int64(len(original)) {
		t.Fatalf("EndOffset = %d, want the original file's %d", parsed.EndOffset, len(original))
	}

	// And the discrimination is real: the same two calls resolved through the
	// PATH answer about the replacement, which is exactly the mismatched pair
	// a second open would have recorded.
	fresh, err := os.Open(path)
	if err != nil {
		t.Fatalf("re-open rollout path: %v", err)
	}
	defer fresh.Close()
	freshIdentity, err := rollout.ReadSourceIdentityAt(fresh, path, codexThreadA)
	if err != nil {
		t.Fatalf("ReadSourceIdentityAt on a fresh handle: %v", err)
	}
	if freshIdentity.HistoryMode != "paginated" || freshIdentity.MetaHash == identity.MetaHash {
		t.Fatalf("fixture did not actually replace the file: identity = %+v", freshIdentity)
	}
}

// A rollout whose header cannot be fingerprinted still imports: the thread
// records no fingerprint and refreshes under the size test alone, exactly as
// every thread did before migration v67. Refusing the import instead would
// make an unreadable header cost the user their history, over a guard whose
// whole posture is that an absent fingerprint is UNKNOWN.
func TestCodexImportKeepsHistoryWhenTheHeaderCannotBeFingerprinted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-08-07T15-07-44-"+codexThreadA+".jsonl")
	// A first record past the bounded head read: the identity scan reaches
	// the end of its window without a newline, so it reports nothing and
	// errors on nothing.
	const headWindow = 4 << 20
	headPrefix := `{"timestamp":"2026-08-07T15:07:44.000Z","type":"session_meta",` +
		`"payload":{"id":"` + codexThreadA + `","history_mode":"legacy","pad":"`
	const headSuffix = `"}}`
	head := headPrefix + strings.Repeat("p", headWindow+4096-len(headPrefix)-len(headSuffix)) + headSuffix
	body := head + "\n" +
		`{"timestamp":"2026-08-07T15:07:45.000Z","type":"event_msg",` +
		`"payload":{"type":"task_started","turn_id":"turn-1"}}` + "\n" +
		`{"timestamp":"2026-08-07T15:07:46.000Z","type":"event_msg",` +
		`"payload":{"type":"user_message","message":"still imported"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open rollout: %v", err)
	}
	defer file.Close()

	parsed, identity, err := codexImportSource(context.Background(), file, path, codexThreadA)
	if err != nil {
		t.Fatalf("codexImportSource refused an unfingerprintable header: %v", err)
	}
	if got := onlyUserText(t, parsed.Events); got != "still imported" {
		t.Fatalf("events = %q, want the history to import anyway", got)
	}
	if identity.MetaHash != "" {
		t.Fatalf("MetaHash = %q, want empty for a header the head read cannot reach", identity.MetaHash)
	}
}
