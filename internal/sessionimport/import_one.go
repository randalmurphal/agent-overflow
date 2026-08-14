package sessionimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// ImportOutcome is one session's import. Threads is empty when the session
// held nothing importable, which is a real answer rather than a failure —
// a transcript can be all metadata.
type ImportOutcome struct {
	Threads  []store.Thread
	Warnings []importir.Warning
}

// ThreadIDs is the outcome's threads, in creation order.
func (o ImportOutcome) ThreadIDs() []string {
	ids := make([]string, 0, len(o.Threads))
	for _, thread := range o.Threads {
		ids = append(ids, thread.ID)
	}
	return ids
}

// ImportOne imports one scanned session.
//
// A Claude transcript becomes one thread PER BRANCH: its conversation is a
// DAG, and an abandoned branch is still a conversation worth keeping. A
// Codex rollout is linear and always becomes exactly one.
//
// Failure is all-or-nothing for the session and isolated from every other
// session. If any branch fails after its thread row exists, every thread this
// call created is deleted — a half-imported session is worse than none,
// because the dedup set keys on the source session id and would hide the
// missing branches from the next scan.
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
	return project.EnsureForWorkspace(s, row.ProjectPath)
}

// branchPlan is one thread-to-be: everything the thread row needs and the
// events that fill it.
type branchPlan struct {
	title      string
	sessionRef string
	events     []importir.Event
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
	// prefixSourceThreadID / prefixBeforeTurn describe a complete-turn
	// history prefix already committed for an earlier Claude branch. The
	// store attaches its immutable chunks before the writer builds events
	// from this branch's suffix.
	prefixSourceThreadID string
	prefixBeforeTurn     int
}

// importClaude imports every branch of one transcript, ONE AT A TIME.
//
// Converting the whole file first and applying afterwards is what the
// memory ceiling forbids: a real transcript reaches hundreds of megabytes
// and a four-leaf session would hold four branches' events at once, on top
// of the decoded file. So each branch is converted, committed, and released
// before the next is read — LoadSession hands back skeletons and
// ConvertBranch decodes only the rows of the branch it is asked for.
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

	// activeThread is the surviving thread cut from the transcript's LAST
	// branch, or -1 when that branch produced nothing. See settleBranches.
	activeThread := -1
	donorThreads := make(map[int]string, len(loaded.Branches))
	for i := range loaded.Branches {
		if err := ctx.Err(); err != nil {
			return ImportOutcome{}, im.fail(err)
		}
		prefix, hasPrefix, err := loaded.FindReusablePrefix(i)
		if err != nil {
			return ImportOutcome{}, im.fail(err)
		}
		start := 0
		if hasPrefix {
			start = prefix.SuffixRow
		}
		branch, err := loaded.ConvertBranchFrom(i, start)
		if err != nil {
			return ImportOutcome{}, im.fail(err)
		}
		im.warn(branch.Warnings...)
		if len(branch.Events) == 0 && !hasPrefix {
			// A branch whose every row was metadata. Producing no thread is
			// a real answer — and it is why the title and resume-ref rules
			// below are computed over the branches that SURVIVED rather than
			// over the transcript's leaf count.
			continue
		}
		plan := branchPlan{
			// Provisional: the disambiguating suffix only belongs on a
			// session that really did import as more than one thread, which
			// is not known until the last branch has been converted.
			title:          branchTitle(row.Title, branch.Title, len(im.created)),
			events:         branch.Events,
			leafUUID:       branch.LeafUUID,
			lastActivityAt: branch.LastActivityAt,
		}
		if hasPrefix {
			plan.prefixSourceThreadID = donorThreads[prefix.DonorIndex]
			plan.prefixBeforeTurn = prefix.NextTurnIndex
		}
		if err := im.add(plan); err != nil {
			return ImportOutcome{}, err
		}
		if err := loaded.AddReusablePrefixDonor(i); err != nil {
			return ImportOutcome{}, im.fail(err)
		}
		donorThreads[i] = im.created[len(im.created)-1].ID
		if i == len(loaded.Branches)-1 {
			activeThread = len(im.created) - 1
		}
	}
	if err := im.settleBranches(activeThread); err != nil {
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
	parsed, err := rollout.Parse(ctx, rollout.ParseOptions{
		Path:      sourcePath,
		SessionID: row.SessionID,
	})
	if err != nil {
		return ImportOutcome{}, fmt.Errorf("sessionimport: read %s: %w", sourcePath, err)
	}
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
		lastActivityAt: row.LastActivityAt,
		endOffset:      parsed.EndOffset,
	}); err != nil {
		return ImportOutcome{}, err
	}
	return im.outcome(), nil
}

// newImportedThread builds the thread row one imported branch lands in.
func newImportedThread(
	row Row, proj store.Project,
	title, sessionRef string,
	events []importir.Event, lastActivityAt int64,
) store.Thread {
	model, effort, contextWindow := sessionModelProfile(events)

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
		ID:              uuid.NewString(),
		ProjectID:       proj.ID,
		ProjectPath:     proj.Path,
		Title:           importedTitle(title),
		Provider:        row.Provider,
		Model:           model,
		WorkspacePath:   row.ProjectPath,
		Branch:          row.GitBranch,
		Mode:            threadmode.DefaultCreateMode,
		ReasoningEffort: effort,
		ContextWindow:   contextWindow,
		RuntimeMode:     string(provider.DefaultRuntimeMode),
		SessionRef:      sessionRef,
		ImportSource:    row.Provider,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
		LastReadAt:      &lastReadAt,
	})
}

// unknownModelPlaceholder is what the Claude reader records for an assistant
// message whose envelope carried no model. It is a marker, not a model, and
// must never reach the thread row — chatmodel would then clamp effort and
// context window against a model the registry has never heard of.
const unknownModelPlaceholder = "unknown"

// sessionModelProfile reads the model setup a session actually ran with off
// its own events, newest wins.
//
// The two providers say it in different places and both are authoritative
// for their own file: Codex stamps model / effort / context window onto every
// turn start (from `turn_context`), while a Claude transcript names the model
// only on the per-message usage the reader folds into each turn's completion.
// Empty results are fine — chatmodel.SanitizeThread substitutes the
// provider's own defaults, which is exactly what a session with no recorded
// model ran on.
func sessionModelProfile(events []importir.Event) (model, effort string, contextWindow int) {
	for _, evt := range events {
		switch evt.Kind {
		case provider.EventTurnStart:
			if value := metaString(evt.Meta, "model"); value != "" {
				model = value
			}
			if value := metaString(evt.Meta, "effort"); value != "" {
				effort = value
			}
			if value := metaInt(evt.Meta, "contextWindow"); value > 0 {
				contextWindow = value
			}
		case provider.EventTurnComplete:
			wire, ok := evt.TurnComplete.(*provider.WireTurnCompleteMeta)
			if !ok || wire == nil {
				continue
			}
			for _, usage := range wire.ModelUsage {
				if name := strings.TrimSpace(usage.Model); name != "" && name != unknownModelPlaceholder {
					model = name
				}
			}
		}
	}
	return model, effort, contextWindow
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

// metaInt reads one top-level integer key off an event meta.
func metaInt(raw json.RawMessage, key string) int {
	if len(raw) == 0 {
		return 0
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return 0
	}
	var value int
	if json.Unmarshal(obj[key], &value) != nil {
		return 0
	}
	return value
}

// importedTitleFallback is what a session with no recoverable title gets.
// Blank titles are what the sidebar renders by default, so there is no
// silent-empty option.
const importedTitleFallback = "Imported session"

// branchTitleSuffixRunes bounds the branch disambiguator. The suffix is a
// user prompt and can be a whole paragraph.
const branchTitleSuffixRunes = 60

func importedTitle(title string) string {
	if trimmed := strings.TrimSpace(title); trimmed != "" {
		return trimmed
	}
	return importedTitleFallback
}

// branchTitle names one branch of a transcript that imports as more than
// one thread.
//
// Sibling branches would otherwise land in the sidebar as identical rows,
// so each takes its leaf's last prompt as a suffix — the one thing that
// actually distinguishes them — and falls back to an ordinal when the
// transcript recorded no prompt for that leaf.
//
// Whether a suffix is wanted at all is NOT decided here: a branch can
// convert to zero events, so how many threads a session really produced is
// only known once the last one has been converted. settleBranches strips
// the suffix back off when exactly one branch survived.
func branchTitle(sessionTitle, leafTitle string, index int) string {
	base := strings.TrimSpace(sessionTitle)
	suffix := capRunes(strings.TrimSpace(leafTitle), branchTitleSuffixRunes)
	switch {
	case base == "":
		base = importedTitleFallback
	case suffix == base:
		suffix = ""
	}
	if suffix == "" {
		suffix = fmt.Sprintf("branch %d", index+1)
	}
	return base + " — " + suffix
}

// capRunes truncates on a rune boundary, marking the cut so a clipped title
// does not read as the whole prompt.
func capRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimRight(string(runes[:max]), " \t") + "…"
}
