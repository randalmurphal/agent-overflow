// app_harness_seed.go — HarnessSeed / HarnessReset: declarative
// database + workspace fixtures for the agent test harness.
//
// Seeding runs through production paths for projects, threads, and workflow
// items. Completed ordinary-thread history writes the rows the app would have
// persisted after the fact; workflow definitions/profile/item driving live in
// app_harness_workflows.go and never write workflow persistence directly.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"agent-overflow/internal/harness"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
)

// HarnessSeedSpec is the root fixture document.
type HarnessSeedSpec struct {
	// ProviderHome writes files under the harness-owned provider home
	// (<dataRoot>/home) before any project seeds: `.claude.json` MCP
	// config, `.claude/settings.json`, skills, or a pre-baked
	// `.claude/projects/...` transcript to pair with a thread's
	// SessionRef. Paths are slash-separated and relative to that home;
	// anything resolving outside it is refused.
	ProviderHome []HarnessSeedHomeFile `json:"providerHome,omitempty"`
	Projects     []HarnessSeedProject  `json:"projects,omitempty"`
}

// HarnessSeedHomeFile is one file written under the provider home.
type HarnessSeedHomeFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// HarnessSeedProject creates (or reuses) a workspace and its threads.
type HarnessSeedProject struct {
	// Name labels the generated workspace dir when Repo is used.
	Name string `json:"name"`
	// Path points at an existing directory to use as the project root.
	// Mutually exclusive with Repo.
	Path string `json:"path,omitempty"`
	// Repo builds a throwaway git repository under
	// <dataRoot>/workspaces/<Name>.
	Repo    *harness.RepoSpec   `json:"repo,omitempty"`
	Threads []HarnessSeedThread `json:"threads,omitempty"`
	// Workflows installs project-scoped definitions/profile data and starts
	// work items through WorkflowStartRun.
	Workflows *HarnessSeedWorkflows `json:"workflows,omitempty"`
}

// HarnessSeedThread creates one thread plus optional pre-baked history.
type HarnessSeedThread struct {
	Title string `json:"title,omitempty"`
	// Provider defaults to "claude"; Model to a provider-appropriate
	// default.
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Mode defaults to "chat"; RuntimeMode to "approval-required".
	Mode        string `json:"mode,omitempty"`
	RuntimeMode string `json:"runtimeMode,omitempty"`
	// SessionRef pre-binds a provider session cursor (pair with a
	// seeded ~/.claude session file to exercise resume flows).
	SessionRef string            `json:"sessionRef,omitempty"`
	Archived   bool              `json:"archived,omitempty"`
	Turns      []HarnessSeedTurn `json:"turns,omitempty"`
}

// HarnessSeedTurn is one completed exchange: the user message plus the
// assistant-side items that followed it.
type HarnessSeedTurn struct {
	UserText string            `json:"userText"`
	Items    []HarnessSeedItem `json:"items,omitempty"`
	// Incomplete leaves the turn without completed_at — the state a
	// crash recovery or "still running" UI probe sees.
	Incomplete bool `json:"incomplete,omitempty"`
	// StopReason defaults to "end_turn" for completed turns.
	StopReason string `json:"stopReason,omitempty"`
}

// HarnessSeedItem is one timeline row. Kind uses the store's item kinds
// (assistant_text, thinking, tool_call, command, command_output, error,
// compaction). Unset fields get the same defaults the store applies.
type HarnessSeedItem struct {
	Kind     string          `json:"kind"`
	Role     string          `json:"role,omitempty"`   // default "assistant"
	Status   string          `json:"status,omitempty"` // default "completed"
	Summary  string          `json:"summary"`
	ToolName string          `json:"toolName,omitempty"`
	Meta     json.RawMessage `json:"meta,omitempty"`
	// Payload attaches heavy content loaded on demand (diff bodies,
	// command output, thinking text).
	Payload *HarnessSeedPayload `json:"payload,omitempty"`
}

// HarnessSeedPayload is the heavy-content sidecar for an item.
type HarnessSeedPayload struct {
	Kind string `json:"kind"`
	Meta string `json:"meta,omitempty"`
	Data string `json:"data"`
}

// HarnessSeedResult reports what was created, in spec order.
type HarnessSeedResult struct {
	// HomeFiles lists the providerHome paths written, home-relative.
	HomeFiles []string                   `json:"homeFiles,omitempty"`
	Projects  []HarnessSeedProjectResult `json:"projects"`
}

// HarnessSeedProjectResult pairs created ids with their workspace path.
type HarnessSeedProjectResult struct {
	ProjectID   string   `json:"projectId"`
	Path        string   `json:"path"`
	ThreadIDs   []string `json:"threadIds"`
	WorkItemIDs []string `json:"workItemIds"`
}

// HarnessSeed decodes and applies a seed spec.
//
// It takes RAW JSON rather than a typed parameter so the decode is
// STRICT: `internal/harness/scenario`'s parser has always refused unknown
// fields, and until this the seed half of the same document format
// accepted them — so a caller's `treads: [...]` seeded nothing, returned
// success, and failed several assertions later as a missing thread. See
// app_harness_json.go. In-process callers use seed() with a typed spec.
//
// Harness methods are not in the generated TS bindings (the receiver is
// registered only by an isolated boot), so the wire shape is unchanged by
// this: the dispatcher hands any JSON object straight through.
func (h *Harness) HarnessSeed(raw json.RawMessage) (HarnessSeedResult, error) {
	var spec HarnessSeedSpec
	if err := decodeStrictJSON("seed spec", raw, &spec); err != nil {
		return HarnessSeedResult{}, err
	}
	return h.seed(spec)
}

// seed applies the spec. Not transactional across projects: a
// mid-spec failure returns the error with earlier projects already
// created — HarnessReset is the recovery tool, and partial state plus a
// loud error beats a bespoke rollback engine inside a test harness.
func (h *Harness) seed(spec HarnessSeedSpec) (result HarnessSeedResult, err error) {
	// See Harness.mutate: a concurrent reset would RemoveAll the workspaces
	// tree this seed is writing repositories into.
	h.mutate.Lock()
	defer h.mutate.Unlock()
	if len(spec.Projects) == 0 && len(spec.ProviderHome) == 0 {
		return HarnessSeedResult{}, fmt.Errorf("seed spec has no projects and no providerHome files")
	}
	// Home files first: a thread with a SessionRef pairs with a transcript
	// under `<home>/.claude/projects`, and resume probes the file the
	// moment the thread's session starts.
	for fi, file := range spec.ProviderHome {
		written, err := h.seedHomeFile(file)
		if err != nil {
			return result, fmt.Errorf("seed providerHome file %d (%s): %w", fi+1, file.Path, err)
		}
		result.HomeFiles = append(result.HomeFiles, written)
	}
	workflowSeed := specHasWorkflowSeed(spec)
	if workflowSeed {
		if err := validateWorkflowSeedPlan(spec); err != nil {
			return HarnessSeedResult{}, err
		}
		if _, requireErr := h.app.requireWorkflowEngine(); requireErr != nil {
			return HarnessSeedResult{}, fmt.Errorf("seed workflows: %w", requireErr)
		}
	}
	for pi, project := range spec.Projects {
		created, err := h.seedProject(project)
		if err != nil {
			return result, fmt.Errorf("seed project %d (%s): %w", pi+1, project.Name, err)
		}
		result.Projects = append(result.Projects, created)
	}
	if workflowSeed {
		if err := h.seedWorkflowItemsForProjects(spec, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// seedHomeFile writes one providerHome file under the harness-owned
// credential home. CredentialHome, not HomeDir: HomeDir is empty under
// AO_HARNESS_KEEP_HOME, while CredentialHome is always `<dataRoot>/home`
// — the home App.providerHome() resolves against — so a seeded fixture
// can never land in the developer's real `~/.claude`.
func (h *Harness) seedHomeFile(file HarnessSeedHomeFile) (string, error) {
	if h.paths.CredentialHome == "" {
		return "", fmt.Errorf("no harness provider home configured")
	}
	rel := path.Clean(file.Path)
	// Backslashes and colons are refused even where the OS would accept
	// them as filename bytes: the spec is slash-separated portable paths,
	// and `C:\...` or `a\b` from a Windows-minded caller must fail the
	// same way on every platform rather than seed a literally-named file.
	if file.Path == "" || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, "../") || path.IsAbs(rel) ||
		strings.ContainsAny(rel, `\:`) {
		return "", fmt.Errorf("path must be relative to the provider home (slash-separated, no traversal)")
	}
	dest := filepath.Join(h.paths.CredentialHome, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, []byte(file.Content), 0o600); err != nil {
		return "", err
	}
	return rel, nil
}

func (h *Harness) seedProject(spec HarnessSeedProject) (HarnessSeedProjectResult, error) {
	if (spec.Path == "") == (spec.Repo == nil) {
		return HarnessSeedProjectResult{}, fmt.Errorf("set exactly one of path or repo")
	}
	path := spec.Path
	if spec.Repo != nil {
		if spec.Name == "" {
			return HarnessSeedProjectResult{}, fmt.Errorf("repo projects need a name (it becomes the workspace dir)")
		}
		// The name must stay a single component under <dataRoot>/workspaces:
		// a traversal like "../outside" would generate (and later reset-wipe)
		// a repo outside the harness-owned tree.
		if spec.Name != filepath.Base(spec.Name) || spec.Name == "." || spec.Name == ".." ||
			strings.ContainsAny(spec.Name, `/\`) {
			return HarnessSeedProjectResult{}, fmt.Errorf("project name %q must be a plain directory name (no path separators or traversal)", spec.Name)
		}
		path = filepath.Join(h.paths.DataRoot, "workspaces", spec.Name)
		if err := harness.CreateRepo(path, *spec.Repo); err != nil {
			return HarnessSeedProjectResult{}, err
		}
	}

	projectRow, err := h.app.CreateProject(path)
	if err != nil {
		return HarnessSeedProjectResult{}, fmt.Errorf("create project: %w", err)
	}
	// CreateProject returns the caller-built row; reload the persisted row so
	// migration/store-owned fields such as the project slug are available to
	// the production config-dir helper below.
	projectRow, err = h.app.store.GetProject(projectRow.ID)
	if err != nil {
		return HarnessSeedProjectResult{}, fmt.Errorf("reload project: %w", err)
	}
	out := HarnessSeedProjectResult{ProjectID: projectRow.ID, Path: projectRow.Path}

	for ti, threadSpec := range spec.Threads {
		threadID, err := h.seedThread(projectRow.ID, threadSpec)
		if err != nil {
			return out, fmt.Errorf("thread %d: %w", ti+1, err)
		}
		out.ThreadIDs = append(out.ThreadIDs, threadID)
	}
	if spec.Workflows != nil {
		if err := h.seedProjectWorkflowFiles(projectRow, *spec.Workflows); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (h *Harness) seedThread(projectID string, spec HarnessSeedThread) (string, error) {
	providerName := spec.Provider
	if providerName == "" {
		providerName = "claude"
	}
	model := spec.Model
	if model == "" {
		if providerName == "codex" {
			model = "gpt-5.2-codex"
		} else {
			model = "claude-opus-4-7"
		}
	}
	mode := spec.Mode
	if mode == "" {
		mode = "chat"
	}
	runtimeMode := spec.RuntimeMode
	if runtimeMode == "" {
		runtimeMode = "approval-required"
	}

	thread, err := h.app.CreateThread(CreateThreadOptions{
		ProjectID:   projectID,
		Title:       spec.Title,
		Provider:    providerName,
		Model:       model,
		Mode:        mode,
		RuntimeMode: runtimeMode,
	})
	if err != nil {
		return "", fmt.Errorf("create thread: %w", err)
	}

	if spec.SessionRef != "" {
		if _, err := h.app.store.UpdateSessionRef(thread.ID, spec.SessionRef); err != nil {
			return thread.ID, fmt.Errorf("set session ref: %w", err)
		}
	}
	if err := h.seedHistory(thread.ID, spec.Turns); err != nil {
		return thread.ID, err
	}
	if spec.Archived {
		if err := h.app.ArchiveThread(thread.ID); err != nil {
			return thread.ID, fmt.Errorf("archive: %w", err)
		}
	}
	return thread.ID, nil
}

// seedHistory writes turns + items with naturally spaced timestamps:
// the transcript ends "now" and each turn is a minute apart, so
// relative times and sidebar ordering look like a real session.
func (h *Harness) seedHistory(threadID string, turns []HarnessSeedTurn) error {
	if len(turns) == 0 {
		return nil
	}
	base := time.Now().Add(-time.Duration(len(turns)) * time.Minute).UnixMilli()
	for i, turn := range turns {
		// Turn indexes are 0-based, exactly as sendMessageLocked
		// allocates them: the first turn of a live thread is 0
		// (LastTurnIndex on an empty thread, un-incremented because
		// there are no prior items). Seeding from 1 made every seeded
		// thread one index off from the same conversation produced
		// live, which silently put turn-0-specific behavior out of
		// reach of the whole e2e suite — the conversation rollback
		// branches on `anchor.TurnIndex == 0` to drop the provider
		// session reference instead of slicing its transcript, and no
		// seeded fixture could ever reach it.
		turnIndex := i
		at := base + int64(i)*time.Minute.Milliseconds()
		if err := h.seedTurn(threadID, turnIndex, at, turn); err != nil {
			return fmt.Errorf("turn %d: %w", turnIndex, err)
		}
	}
	return nil
}

func (h *Harness) seedTurn(threadID string, turnIndex int, at int64, spec HarnessSeedTurn) error {
	if spec.UserText == "" {
		return fmt.Errorf("userText must be non-empty")
	}
	turnID := fmt.Sprintf("%s:%d", threadID, turnIndex)
	if err := h.app.store.InsertTurn(store.Turn{
		TurnID:    turnID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		StartedAt: at,
	}); err != nil {
		return fmt.Errorf("insert turn: %w", err)
	}

	if err := h.app.store.InsertItem(store.Item{
		ID:        uuid.NewString(),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: 0,
		Kind:      "user_text",
		Role:      "user",
		Summary:   spec.UserText,
		CreatedAt: at,
		UpdatedAt: at,
	}); err != nil {
		return fmt.Errorf("insert user item: %w", err)
	}

	for ii, itemSpec := range spec.Items {
		itemAt := at + int64(ii+1)*1000
		if err := h.seedItem(threadID, turnIndex, ii+1, itemAt, itemSpec); err != nil {
			return fmt.Errorf("item %d: %w", ii+1, err)
		}
	}

	if !spec.Incomplete {
		stopReason := spec.StopReason
		if stopReason == "" {
			stopReason = "end_turn"
		}
		completedAt := at + int64(len(spec.Items)+1)*1000
		if err := h.app.store.UpdateTurnCompleted(turnID, completedAt, stopReason, "", "", ""); err != nil {
			return fmt.Errorf("complete turn: %w", err)
		}
	}
	return nil
}

func (h *Harness) seedItem(threadID string, turnIndex, itemIndex int, at int64, spec HarnessSeedItem) error {
	if spec.Kind == "" {
		return fmt.Errorf("kind must be non-empty")
	}
	role := spec.Role
	if role == "" {
		role = "assistant"
	}
	item := store.Item{
		ID:        uuid.NewString(),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		ItemIndex: itemIndex,
		Kind:      spec.Kind,
		Role:      role,
		Status:    spec.Status,
		Summary:   spec.Summary,
		ToolName:  spec.ToolName,
		Meta:      string(spec.Meta),
		CreatedAt: at,
		UpdatedAt: at,
	}
	if spec.Payload == nil {
		return h.app.store.InsertItem(item)
	}
	payload := store.Payload{
		ID:        uuid.NewString(),
		Kind:      spec.Payload.Kind,
		Meta:      spec.Payload.Meta,
		Data:      []byte(spec.Payload.Data),
		CreatedAt: at,
	}
	item.PayloadID = payload.ID
	return h.app.store.InsertItemWithPayload(item, payload)
}

// HarnessReset returns the harness to a blank slate without a process
// restart: every provider session is stopped, in-flight turn rows are
// settled the same way startup crash recovery settles them, every
// project is deleted through the production cascade (which reaps
// checkpoints, drafts, attachments, and per-thread in-memory state),
// generated seed workspaces are removed so the same names re-seed
// cleanly, and harness-owned control state (scenario rules, an active
// replay, an in-flight recording, mock registrations) is dropped — the
// per-test isolation
// contract covers everything a previous test could have set, not just
// DB rows. Recorded bundles survive: they are captured artifacts, not
// test state. The caller reloads the page afterwards; DB-derived
// frontend state rebuilds from zero.
func (h *Harness) HarnessReset() (err error) {
	// See Harness.mutate: two resets would both list the same projects and
	// double-cascade them, and a concurrent seed would leave a project row
	// pointing at a workspace this reset is about to delete.
	h.mutate.Lock()
	defer h.mutate.Unlock()
	// Harness-owned state first: a running replay emits events and an
	// in-flight recording holds a capture window — both must die before
	// the state they reference does. A perf run is the same shape one
	// layer out: it holds a sampler goroutine AND a set of armed in-page
	// meters, and the caller reloads the page after a reset, so nothing
	// would ever disarm them.
	h.stopPerfRunForReset()
	h.mu.Lock()
	replayer := h.replayer
	recording := h.recording
	h.recording = nil
	h.scenarioRules = nil
	h.mu.Unlock()
	if replayer != nil {
		replayer.Stop()
	}
	if recording != nil && recording.dir != "" {
		if err := os.RemoveAll(recording.dir); err != nil {
			return fmt.Errorf("discard in-flight recording %q: %w", recording.Name, err)
		}
		log.Printf("harness: reset: discarded in-flight recording %q", recording.Name)
	}

	resumeEngine, err := h.prepareWorkflowReset()
	if err != nil {
		if resumeEngine != nil {
			err = errors.Join(err, resumeEngine())
		}
		return err
	}
	defer func() {
		if resumeEngine != nil {
			err = errors.Join(err, resumeEngine())
		}
	}()

	threads, err := h.app.store.ListThreads()
	if err != nil {
		return fmt.Errorf("list threads: %w", err)
	}
	for _, t := range threads {
		if err := h.app.StopSession(t.ID); err != nil {
			// Keep going: a half-dead session must not wedge the reset;
			// the delete cascade below is the authoritative cleanup.
			log.Printf("harness: reset: stop session %s: %v", t.ID, err)
		}
	}
	if h.app.triage != nil {
		if _, err := h.app.triage.RecoverCrashedTurns(); err != nil {
			return fmt.Errorf("settle in-flight turns: %w", err)
		}
	}
	// Mock registrations belong to the sessions just stopped; carrying
	// them across the reset would leak previous tests' (exited) mocks
	// into the next test's HarnessListMocks.
	h.mu.Lock()
	ctrl := h.control
	h.mu.Unlock()
	if ctrl != nil {
		ctrl.ClearMocks()
	}
	projects, err := h.app.ListProjects()
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	for _, p := range projects {
		// Production deletion cascades these rows too (D25), and cleans up the
		// run worktrees git still knows about. Reset owns disk cleanup wholesale
		// — it removes the generated workspace tree below — so it drops the rows
		// first and hands DeleteProject a project with nothing left to walk,
		// rather than spending a git subprocess per checkout on fixtures that are
		// about to be deleted outright.
		if err := h.app.store.DeleteProjectWorkflowRecords(p.Project.ID); err != nil {
			return fmt.Errorf("delete workflow records for project %s: %w", p.Project.ID, err)
		}
		if _, err := h.app.DeleteProject(p.Project.ID); err != nil {
			return fmt.Errorf("delete project %s: %w", p.Project.ID, err)
		}
	}
	// Persisted UI view state names entity ids — the workflows overlay
	// stack persists work-item ids — so left in place the next test's
	// fresh page restores a selection onto rows this reset just deleted
	// (observed as WorkflowGetItem sql.ErrNoRows toasts). The caller
	// reloads the page after a reset, so a cleared scope rebuilds from
	// defaults.
	if err := h.app.store.ClearUIState(); err != nil {
		return fmt.Errorf("clear persisted ui state: %w", err)
	}
	// The session-import scan is a cached projection OF the rows just
	// deleted: its dedup subtracts sessions AO already has, so a scan taken
	// before this reset would keep hiding provider sessions whose threads no
	// longer exist for up to its TTL. Nothing else invalidates it (the
	// production reset is an import run finishing), so reset drops it here.
	h.app.sessionImportScanCache().Reset()
	// Generated seed workspaces live under <dataRoot>/workspaces only —
	// removing the tree lets the next test seed the same project names
	// (CreateRepo refuses a surviving .git). User-supplied Path projects
	// are elsewhere and untouched.
	if err := os.RemoveAll(filepath.Join(h.paths.DataRoot, "workspaces")); err != nil {
		return fmt.Errorf("remove generated workspaces: %w", err)
	}
	for _, dir := range []string{
		filepath.Join(h.paths.DataDir, "projects"),
		filepath.Join(h.paths.DataDir, "workflows"),
		filepath.Join(h.paths.DataDir, "workflow-runs"),
	} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove workflow harness state %q: %w", dir, err)
		}
	}
	// Provider trees under the harness-owned home: seeded providerHome
	// fixtures AND the transcripts mock providers wrote during the test
	// (EnvTranscriptHome points here). Left in place they leak into the
	// next test's session-import scan and resume probes. CredentialHome is
	// always `<dataRoot>/home` — never the developer's real home, even
	// under AO_HARNESS_KEEP_HOME. The boot-written `.gitconfig` beside
	// these trees survives. The empty-path guard covers test fixtures
	// that build a Harness without a provider home.
	if h.paths.CredentialHome != "" {
		for _, name := range []string{".claude", ".claude.json", ".codex"} {
			p := filepath.Join(h.paths.CredentialHome, name)
			if err := os.RemoveAll(p); err != nil {
				return fmt.Errorf("remove provider home state %q: %w", p, err)
			}
		}
	}
	return nil
}
