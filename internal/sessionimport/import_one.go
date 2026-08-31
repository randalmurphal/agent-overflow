package sessionimport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"agent-overflow/internal/chatmodel"
	"agent-overflow/internal/importir"
	"agent-overflow/internal/project"
	"agent-overflow/internal/provider"
	claudesessions "agent-overflow/internal/provider/claude/sessionimport"
	"agent-overflow/internal/provider/codex/rollout"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
)

// ImportOutcome is one provider session's import. Threads contains at most
// one row and is empty when the session held nothing importable — a transcript
// can be all metadata.
type ImportOutcome struct {
	Threads  []store.Thread
	Warnings []importir.Warning
}

// ThreadIDs returns the imported thread id, or an empty slice.
func (o ImportOutcome) ThreadIDs() []string {
	ids := make([]string, 0, len(o.Threads))
	for _, thread := range o.Threads {
		ids = append(ids, thread.ID)
	}
	return ids
}

// ImportOne imports one scanned session.
//
// One provider session becomes at most one AO thread. A Codex rollout is
// linear. A Claude transcript is a DAG, so import selects the active branch —
// the coherent root-to-leaf history `claude --resume` continues — and does
// not merge or promote abandoned sibling alternatives.
//
// Failure is all-or-nothing for the session and isolated from every other
// session. If a write fails after its thread row exists, that thread is
// deleted so the source session remains importable on the next scan.
//
// Nothing here spawns a process. Import is a file read and a SQLite write.
// The project row is the one the SCAN already resolved (resolveProject); a
// workspace that no longer exists still gets a project at its recorded path —
// the session is worth importing either way, and refusing would hide history
// because a directory moved.
func ImportOne(ctx context.Context, d Deps, row Row) (ImportOutcome, error) {
	if d.Store == nil {
		return ImportOutcome{}, fmt.Errorf("sessionimport: import has no store")
	}
	if err := ctx.Err(); err != nil {
		return ImportOutcome{}, err
	}
	if strings.TrimSpace(row.SourcePath) == "" {
		return ImportOutcome{}, fmt.Errorf("sessionimport: %s has no source path", row.ID)
	}
	if strings.TrimSpace(row.SessionID) == "" {
		return ImportOutcome{}, fmt.Errorf("sessionimport: %s has no session id", row.ID)
	}

	proj, err := resolveProject(d.Store, row)
	if err != nil {
		return ImportOutcome{}, fmt.Errorf("sessionimport: resolve project for %s: %w", row.ID, err)
	}

	switch row.Provider {
	case ProviderClaude:
		return importClaude(ctx, d.Store, row, proj)
	case ProviderCodex:
		return importCodex(ctx, d, row, proj)
	default:
		return ImportOutcome{}, fmt.Errorf("sessionimport: %s names unknown provider %q", row.ID, row.Provider)
	}
}

// resolveProject picks the project row this import lands in.
//
// The SCAN already answered the question: `projectIndex.decorate` resolved
// the session's cwd to its MAIN repository root, and a cwd whose worktree has
// since been deleted to the project that still registers it — an answer
// nothing on the filesystem can reproduce afterwards. Re-deriving it here
// could only disagree with the project the listing showed the user, so the
// stamped id wins.
//
// EnsureForWorkspace covers the two cases the stamp does not: a row that
// belongs to no project yet (it creates one at the repository root) and a
// stamped project that has been deleted between the scan and the import.
func resolveProject(s *store.Store, row Row) (store.Project, error) {
	if id := strings.TrimSpace(row.ProjectID); id != "" {
		proj, err := s.GetProject(id)
		if err == nil {
			return proj, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return store.Project{}, err
		}
	}
	proj, _, err := project.EnsureForWorkspace(s, row.ProjectPath)
	return proj, err
}

// branchPlan is one thread-to-be: everything the thread row needs and the
// events that fill it.
type branchPlan struct {
	title      string
	sessionRef string
	events     []importir.Event
	profile    importir.ModelProfile
	leafUUID   string
	// lastActivityAt is the branch's own newest timestamp, which becomes
	// the thread's updated_at.
	lastActivityAt int64
	// endOffset is Codex's resumable file position. It can sit PAST the
	// last event's own offset, because trailing lines that produced no
	// event (an unknown envelope type, a skipped reasoning block) are still
	// consumed — a refresh that re-read them would re-decide them the same
	// way, but at the cost of re-reading the tail on every check.
	endOffset int64
	// identity is the source fingerprint a later refresh compares before it
	// trusts endOffset (store migration v67). Codex only: Claude's refresh
	// anchors on a transcript uuid, which a rewritten file invalidates by
	// itself.
	identity rollout.SourceIdentity
}

// importClaude imports the transcript's active branch as one thread.
//
// LoadSession keeps a lightweight DAG skeleton, then ConvertActiveBranch
// decodes only the coherent ancestry Claude itself resumes. Sibling leaves
// are alternate histories, not extra sessions and not rows that can be
// merged into the active timeline without corrupting its causal order.
func importClaude(
	ctx context.Context, s *store.Store, row Row, proj store.Project,
) (ImportOutcome, error) {
	loaded, err := claudesessions.LoadSession(row.SourcePath)
	if err != nil {
		if errors.Is(err, claudesessions.ErrTranscriptTooLarge) {
			// Already user-facing prose, and the run reports it verbatim.
			// A path prefix would only bury the sentence written for the user.
			return ImportOutcome{}, err
		}
		return ImportOutcome{}, fmt.Errorf("sessionimport: read %s: %w", row.SourcePath, err)
	}
	defer loaded.Close()

	im := newSessionImporter(s, row, proj)
	im.warn(loaded.Warnings...)
	if err := ctx.Err(); err != nil {
		return ImportOutcome{}, err
	}
	branch, found, err := loaded.ConvertActiveBranch()
	if err != nil {
		return ImportOutcome{}, fmt.Errorf(
			"sessionimport: convert active branch of %s: %w", row.SourcePath, err)
	}
	if !found {
		return im.outcome(), nil
	}
	im.warn(branch.Warnings...)
	if len(branch.Events) == 0 {
		return im.outcome(), nil
	}
	if err := im.add(branchPlan{
		title:          importedTitle(row.Title),
		sessionRef:     row.SessionID,
		events:         branch.Events,
		profile:        branch.Profile,
		leafUUID:       branch.LeafUUID,
		lastActivityAt: branch.LastActivityAt,
	}); err != nil {
		return ImportOutcome{}, err
	}
	return im.outcome(), nil
}

// importCodex imports one rollout. A rollout is one linear conversation, so
// it is always exactly one thread and it always carries the resume ref.
//
// The source path is re-proved to be inside the Codex home before the file is
// opened. Scan already refuses an escaping `rollout_path`, but the row travels
// through a cache and back over the wire between those two points, and the
// cost of asking again is one string comparison against reading an arbitrary
// file off the user's disk into a thread. See rollout.PathInHome.
func importCodex(
	ctx context.Context, d Deps, row Row, proj store.Project,
) (ImportOutcome, error) {
	sourcePath, err := rollout.PathInHome(d.CodexHome, row.SourcePath)
	if err != nil {
		return ImportOutcome{}, fmt.Errorf("sessionimport: %s: %w", row.ID, err)
	}
	// ONE handle serves both reads below, exactly as codexTail's refresh
	// does. Codex publishes a migrated rollout by writing the canonicalised
	// file and renaming it over the same path, so two independent opens can
	// straddle that rename: the events would come from the file that was
	// there and the fingerprint from the replacement, and the refresh that
	// later trusts the pair would resume the replacement at an offset that
	// describes a different inode's records. A held fd keeps naming the inode
	// it opened, which is what makes the events, the resume offset, and the
	// fingerprint three answers about one file.
	file, err := os.Open(sourcePath)
	if err != nil {
		return ImportOutcome{}, fmt.Errorf("sessionimport: read %s: %w", sourcePath, err)
	}
	defer file.Close()
	parsed, identity, err := codexImportSource(ctx, file, sourcePath, row.SessionID)
	if err != nil {
		return ImportOutcome{}, err
	}
	parsed = rollout.ProjectReviewChildren(ctx, d.CodexHome, row.SessionID, parsed)
	row.SourcePath = sourcePath
	im := newSessionImporter(d.Store, row, proj)
	im.warn(parsed.Warnings...)
	if len(parsed.Events) == 0 {
		return im.outcome(), nil
	}
	if err := im.add(branchPlan{
		title:          importedTitle(row.Title),
		sessionRef:     row.SessionID,
		events:         parsed.Events,
		profile:        preferParsedProfile(parsed.Profile, row.IndexedProfile),
		lastActivityAt: row.LastActivityAt,
		endOffset:      parsed.EndOffset,
		identity:       identity,
	}); err != nil {
		return ImportOutcome{}, err
	}
	return im.outcome(), nil
}

// codexImportSource reads one rollout's events and its source fingerprint
// from a single open handle.
//
// Both answers are POSITIONAL reads of the same fd (Parse seeks it,
// ReadSourceIdentityAt uses io.ReaderAt), never a second resolution of the
// path — see the caller for why that matters. A fingerprint that cannot be
// read is NOT fatal: an unfingerprinted thread simply refreshes under the size
// test alone, exactly as every thread did before v67, and refresh.go treats a
// recorded "" as UNKNOWN for that reason. A failed PARSE is fatal, because
// there is no history to import without it.
func codexImportSource(
	ctx context.Context, file *os.File, sourcePath, sessionID string,
) (rollout.ParseResult, rollout.SourceIdentity, error) {
	parsed, err := rollout.Parse(ctx, rollout.ParseOptions{
		File:      file,
		Path:      sourcePath,
		SessionID: sessionID,
	})
	if err != nil {
		return rollout.ParseResult{}, rollout.SourceIdentity{},
			fmt.Errorf("sessionimport: read %s: %w", sourcePath, err)
	}
	identity, identityErr := rollout.ReadSourceIdentityAt(file, sourcePath, sessionID)
	if identityErr != nil {
		identity = rollout.SourceIdentity{}
	}
	return parsed, identity, nil
}

// newImportedThread builds the thread row one imported branch lands in.
func newImportedThread(
	row Row, proj store.Project,
	title, sessionRef string,
	events []importir.Event, profile importir.ModelProfile, lastActivityAt int64,
) store.Thread {
	createdAt := row.CreatedAt
	if createdAt <= 0 {
		createdAt = firstEventMillis(events)
	}
	updatedAt := lastActivityAt
	if updatedAt < createdAt {
		updatedAt = createdAt
	}

	// An imported thread is read on arrival. Its history already happened,
	// and CreateThread's default (a read marker at CreatedAt) would surface
	// every imported thread as unread the moment it lands — the same reason
	// ApplyImportBatch leaves threads.updated_at alone.
	lastReadAt := updatedAt

	return chatmodel.SanitizeThread(store.Thread{
		ID:            uuid.NewString(),
		ProjectID:     proj.ID,
		ProjectPath:   proj.Path,
		Title:         importedTitle(title),
		Provider:      row.Provider,
		Model:         profile.Model,
		WorkspacePath: row.ProjectPath,
		Branch:        row.GitBranch,
		// Creation provenance stays empty by design. The session already
		// happened, on a machine and at a commit this import cannot recover:
		// the provider session file records the branch (above) and nothing
		// else, and observing the workspace now would stamp today's head onto
		// a thread that ran months ago. Empty reads as "not known", which is
		// exactly what is true here.
		Origin:          store.ThreadOrigin{},
		CreatedByDevice: "",
		Mode:            threadmode.DefaultCreateMode,
		ReasoningEffort: profile.ReasoningEffort,
		ContextWindow:   profile.ContextWindow,
		RuntimeMode:     string(provider.DefaultRuntimeMode),
		SessionRef:      sessionRef,
		ImportSource:    row.Provider,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
		LastReadAt:      &lastReadAt,
	})
}

// firstEventMillis is the fallback thread creation time for a session whose
// listing carried none. Still the provider's own clock — never now().
func firstEventMillis(events []importir.Event) int64 {
	for _, evt := range events {
		if !evt.Timestamp.IsZero() {
			return evt.Timestamp.UnixMilli()
		}
	}
	return 0
}

// preferParsedProfile keeps the rollout as the authority and fills only
// fields it did not record from Codex's index-level fallback. The index is
// incomplete for some sources, but reading columns already in the scan costs
// nothing and is better than discarding provider-recorded state.
func preferParsedProfile(parsed, indexed importir.ModelProfile) importir.ModelProfile {
	if parsed.Model == "" {
		parsed.Model = indexed.Model
		if parsed.ReasoningEffort == "" {
			parsed.ReasoningEffort = indexed.ReasoningEffort
		}
		return parsed
	}
	if parsed.Model == indexed.Model && parsed.ReasoningEffort == "" {
		parsed.ReasoningEffort = indexed.ReasoningEffort
	}
	return parsed
}

// importedTitleFallback is what a session with no recoverable title gets.
// Blank titles are what the sidebar renders by default, so there is no
// silent-empty option.
const importedTitleFallback = "Imported session"

func importedTitle(title string) string {
	if trimmed := strings.TrimSpace(title); trimmed != "" {
		return trimmed
	}
	return importedTitleFallback
}
