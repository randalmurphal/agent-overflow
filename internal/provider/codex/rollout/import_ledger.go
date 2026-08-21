package rollout

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"agent-overflow/internal/importir"
)

// Codex's own record of sessions IT imported from another coding agent.
//
// `codex` can migrate a Claude Code or Cursor session into a Codex thread
// (`codex-rs/external-agent-migration/`). When it does, it appends a record to
// `<codexHome>/external_agent_session_imports.json` naming the file it read
// and the thread it produced. That file is the only place the provenance
// survives: the resulting rollout is an ordinary Codex rollout whose
// `session_meta.originator` says `codex_cli`, with nothing in it to say the
// conversation started somewhere else.
//
// Reading it lets AO's import picker say two things it otherwise cannot:
//
//  1. "this Codex thread is a Claude Code conversation Codex imported", so the
//     user is not surprised by a thread whose content predates Codex; and
//  2. "you already have this conversation" — when AO's own store already holds
//     the SOURCE session (imported straight from `~/.claude`), the Codex copy
//     is the same conversation a second time.
//
// Read-only, bounded, and never fatal: a Codex home that has never imported
// anything has no such file, and a corrupt one costs labels, not a listing.

// ExternalImportLedgerFile is the ledger's name inside the Codex home
// (`SESSION_IMPORT_LEDGER_FILE`, codex-rs/external-agent-migration/src/
// sessions/ledger.rs @ rust-v0.149.0). Present since 0.147.
const ExternalImportLedgerFile = "external_agent_session_imports.json"

// WarnImportLedgerUnreadable means the ledger exists but could not be read or
// decoded. Sessions still list; they just carry no origin label.
const WarnImportLedgerUnreadable = "codex-import-ledger-unreadable"

// externalImportLedgerMaxBytes bounds the read. The ledger holds one small
// record per imported session — a home with a thousand imports is well under a
// megabyte — and it is a JSON document that must be decoded whole, so there is
// no streaming degrade to fall back on. A file past this is treated as
// unreadable rather than buffered.
const externalImportLedgerMaxBytes = 8 << 20

// The agents Codex can import from. The ledger record itself does NOT name the
// agent — upstream keeps the two readers apart by type (`records_cla` /
// `records_cur`) and by which detector produced the candidate, not by a field
// on disk — so this is derived from the source path's shape. See
// externalImportAgent.
const (
	ExternalImportAgentClaude = "claude-code"
	ExternalImportAgentCursor = "cursor"
)

// ExternalImportRecord is one session Codex imported from another agent.
type ExternalImportRecord struct {
	// Agent is the coding agent the conversation came from, one of the
	// ExternalImportAgent* constants, or "" when the path shape matches
	// neither. Derived, not read — see externalImportAgent.
	Agent string
	// SourcePath is the file Codex read, verbatim from the ledger. It is
	// `fs::canonicalize`d by the writer, so it is absolute and
	// symlink-resolved, and it may name a file that no longer exists.
	SourcePath string
	// SourceSessionID is the source's own session id when the agent's layout
	// makes it recoverable from the path — for Claude Code the `.jsonl`
	// filename stem, which is exactly the id AO's own Claude importer keys
	// on. Empty for an agent whose transcripts are not named by session id.
	SourceSessionID string
	// ImportedAt is when Codex recorded the import, in epoch MS. The ledger
	// stores unix SECONDS (`now_unix_seconds`); the conversion happens here
	// so nothing downstream has to know that. Zero when absent.
	ImportedAt int64
	// Title is the title Codex recorded for the imported session, if any.
	Title string
	// ConnectorNames are the MCP connectors Codex attributed to the source
	// session. Carried because it is the only other user-meaningful field on
	// the record; nothing in AO reads it yet.
	ConnectorNames []string
}

// externalImportLedger mirrors `ImportedExternalAgentSessionLedger`
// (rust-v0.149.0). Serde derives with no `rename_all`, so the JSON keys are
// the Rust field names verbatim. Only `records` is read: the sibling
// `detected_connector_records` describes sources Codex NOTICED, not ones it
// imported, and has no thread id to attach to.
type externalImportLedger struct {
	Records []externalImportLedgerRecord `json:"records"`
}

// externalImportLedgerRecord mirrors `ImportedExternalAgentSessionRecord`:
//
//	source_path: PathBuf
//	content_sha256: String
//	imported_thread_id: ThreadId
//	imported_at: i64                      // unix seconds
//	source_modified_at: Option<i64>       // unix NANOS — deliberately unread
//	connector_names: Vec<String>
//	title: Option<String>
//
// `content_sha256` and `source_modified_at` are the writer's own change
// detection for re-imports and answer nothing AO asks, so they are not
// decoded. Note the unit mismatch between the two timestamps upstream; only
// `imported_at` is read, and it is seconds.
type externalImportLedgerRecord struct {
	SourcePath       string   `json:"source_path"`
	ImportedThreadID string   `json:"imported_thread_id"`
	ImportedAt       int64    `json:"imported_at"`
	ConnectorNames   []string `json:"connector_names"`
	Title            *string  `json:"title"`
}

// ReadExternalImportLedger returns Codex's external-import records keyed by
// the CODEX thread id each import produced, which is the id a listing row
// carries.
//
// Absence is the common case and is not a warning: most Codex homes have
// never imported anything. Anything else that goes wrong — unreadable,
// oversized, not JSON, not the shape we know — costs the labels and raises
// exactly one WarnImportLedgerUnreadable, never an error: the origin badge is
// decoration on a listing that must still list.
//
// A record naming no thread id is skipped. A duplicate thread id keeps the
// LAST record, matching upstream's own writer, which removes and re-pushes a
// re-imported source so the newest record is last.
func ReadExternalImportLedger(codexHome string) (map[string]ExternalImportRecord, []importir.Warning) {
	home := strings.TrimSpace(codexHome)
	if home == "" {
		return nil, nil
	}
	path := filepath.Join(home, ExternalImportLedgerFile)

	// ONE open, and the bound is enforced on the bytes actually read rather
	// than on a size observed before the read. A stat-then-ReadFile pair
	// reads whatever the file grew to in between — the ledger belongs to a
	// live Codex that appends to it — so the bound it checked would not be
	// the bound it applied. The handle's own stat is used only to name the
	// directory case, which needs no second look at the path.
	file, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, []importir.Warning{ledgerWarning(err)}
	}
	defer func() { _ = file.Close() }()
	if info, statErr := file.Stat(); statErr == nil && info.IsDir() {
		return nil, []importir.Warning{ledgerWarning(errors.New("it is a directory"))}
	}
	// One byte past the bound, so an oversized file is DETECTED rather than
	// silently truncated into unparseable JSON.
	raw, err := io.ReadAll(io.LimitReader(file, externalImportLedgerMaxBytes+1))
	if err != nil {
		return nil, []importir.Warning{ledgerWarning(err)}
	}
	if int64(len(raw)) > externalImportLedgerMaxBytes {
		return nil, []importir.Warning{ledgerWarning(fmt.Errorf(
			"it is larger than the %d-byte bound", externalImportLedgerMaxBytes))}
	}
	var ledger externalImportLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return nil, []importir.Warning{ledgerWarning(err)}
	}

	records := make(map[string]ExternalImportRecord, len(ledger.Records))
	for _, record := range ledger.Records {
		threadID := strings.TrimSpace(record.ImportedThreadID)
		if threadID == "" {
			continue
		}
		sourcePath := strings.TrimSpace(record.SourcePath)
		title := ""
		if record.Title != nil {
			title = strings.TrimSpace(*record.Title)
		}
		importedAt := int64(0)
		if record.ImportedAt > 0 {
			importedAt = record.ImportedAt * 1000
		}
		agent := externalImportAgent(sourcePath)
		records[threadID] = ExternalImportRecord{
			Agent:           agent,
			SourcePath:      sourcePath,
			SourceSessionID: externalImportSourceSessionID(agent, sourcePath),
			ImportedAt:      importedAt,
			Title:           title,
			ConnectorNames:  record.ConnectorNames,
		}
	}
	return records, nil
}

func ledgerWarning(cause error) importir.Warning {
	return importir.Warning{
		Code: WarnImportLedgerUnreadable,
		Message: "Codex's record of sessions it imported from other agents could not be read (" +
			cause.Error() + "), so imported sessions are not labelled as such.",
	}
}

// externalImportAgent derives which agent a ledger source path came from.
//
// The record carries no agent field: upstream separates the two by TYPE
// (`SessionRecordFormat::Cla` / `Cur`, chosen by whichever detector produced
// the candidate) and never persists the choice. The two layouts its detectors
// walk are distinct enough to recover it (rust-v0.149.0,
// `detect/sessions/cla.rs` and `cur.rs`):
//
//	Claude Code: <externalAgentHome>/projects/<slug>/<uuid>.jsonl
//	Cursor:      <externalAgentHome>/projects/<slug>/agent-transcripts/<file>
//
// Three tests, in order:
//
//  1. Cursor's `agent-transcripts` directory (or a `.cursor` home segment).
//     It goes first because it is the strongest signal and cannot collide
//     with the Claude layout.
//  2. A `.claude` home segment.
//  3. Claude's LAYOUT — `projects/<slug>/<uuid>.jsonl` — which is what
//     recognises a RELOCATED Claude home, whose directory is not named
//     `.claude` at all. AO itself supports one (`Deps.ClaudeProjectsDir` is
//     injected), so keying only on the home's name would silently drop the
//     labels for exactly those users.
//
// An unrecognised shape yields "" rather than a guess — the picker renders no
// badge, which is the honest answer for a layout this build has not seen.
func externalImportAgent(sourcePath string) string {
	if sourcePath == "" {
		return ""
	}
	segments := pathSegments(sourcePath)
	for _, segment := range segments {
		if segment == "agent-transcripts" || segment == ".cursor" {
			return ExternalImportAgentCursor
		}
	}
	for _, segment := range segments {
		if segment == ".claude" {
			return ExternalImportAgentClaude
		}
	}
	if n := len(segments); n >= 3 && segments[n-3] == "projects" &&
		claudeTranscriptSessionID(segments[n-1]) != "" {
		return ExternalImportAgentClaude
	}
	return ""
}

// claudeSessionFileRe is the same filename admission rule
// `internal/provider/claude/sessionimport` applies when it lists transcripts:
// a canonical UUID with a `.jsonl` extension. Keeping the two in agreement is
// what makes the recovered id safe to compare against AO's own Claude import
// state — a stem that lister would never produce must never match one.
var claudeSessionFileRe = regexp.MustCompile(
	`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

func claudeTranscriptSessionID(base string) string {
	match := claudeSessionFileRe.FindStringSubmatch(base)
	if match == nil {
		return ""
	}
	return match[1]
}

// externalImportSourceSessionID recovers the SOURCE agent's own session id
// from the path, when its layout encodes one.
//
// Claude Code names each transcript `<sessionId>.jsonl`, and that stem is
// exactly what `internal/provider/claude/sessionimport` uses as the session
// id — which is what lets a caller ask whether AO already holds the same
// conversation. Cursor's transcript files are not named by session id, so
// there is nothing to recover and the duplicate check simply does not apply
// to them.
func externalImportSourceSessionID(agent, sourcePath string) string {
	if agent != ExternalImportAgentClaude {
		return ""
	}
	// Split on both separators rather than using filepath.Base, for the same
	// reason externalImportAgent does: the ledger's paths are written by
	// whichever OS ran Codex, and filepath.Base on Linux would return a
	// Windows path whole.
	segments := pathSegments(sourcePath)
	if len(segments) == 0 {
		return ""
	}
	return claudeTranscriptSessionID(segments[len(segments)-1])
}

// pathSegments splits a ledger path on BOTH separators. The ledger is written
// by whichever OS ran Codex, and a Windows-authored path read on Linux (a
// copied home, a WSL-mounted one) would otherwise be one long segment.
func pathSegments(path string) []string {
	return strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
}
