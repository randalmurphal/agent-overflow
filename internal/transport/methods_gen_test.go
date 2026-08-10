package transport

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

// wireSafeMethods is the hand-maintained allow-list of every App method
// the dispatcher exposes to non-loopback (LAN-attached) peers. It is
// the positive partner to LocalOnlyMethods: the union of the two MUST
// cover every entry in GeneratedMethods.
//
// The list lives in test scope (not internalmethods.go) on purpose:
// nothing in the runtime dispatcher consults it. The dispatcher
// already refuses LocalOnlyMethods from non-loopback peers and lets
// everything else through; this map is a forcing function that makes
// "let everything else through" a deliberate per-method choice instead
// of an implicit default.
//
// Adding a new App method falls into one of these cases:
//
//  1. The method touches local FS, spawns processes, mutates settings,
//     controls a provider session, writes attachments, or returns
//     credentials. Add to LocalOnlyMethods in internalmethods.go (with
//     a category-block comment explaining the threat shape).
//  2. The method is safe for a LAN-attached token-holder to call.
//     Add to wireSafeMethods below.
//
// TestGeneratedMethods_AllClassified will fail with a missing-name
// pointer if a new method lands without a classification. The test
// failure message names the method and reminds the developer to pick
// one branch — never default by silence.
var wireSafeMethods = map[string]bool{
	// Syntax-highlight span metadata: pure text-in/metadata-out over
	// content the caller already holds. The scope-resolving variant
	// (HighlightPatchWithContext) reads local file content and lives in
	// LocalOnlyMethods.
	"HighlightClassNames":    true,
	"HighlightSchemaVersion": true,
	"HighlightCode":          true,
	"HighlightPatch":         true,

	// Project lifecycle (CRUD, sort, archive). User-driven UI surface
	// the remote browser must reach.
	"ArchiveProject":             true,
	"UnarchiveProject":           true,
	"DeleteProject":              true,
	"RenameProject":              true,
	"ListProjects":               true,
	"UpdateProjectSortPositions": true,

	// Thread lifecycle (CRUD, archive, pin, read/unread). Same
	// user-driven UI surface.
	"ArchiveThread":        true,
	"UnarchiveThread":      true,
	"DeleteThread":         true,
	"RenameThread":         true,
	"GetThread":            true,
	"ListArchivedThreads":  true,
	"ListThreads":          true,
	"PinThread":            true,
	"UnpinThread":          true,
	"MarkThreadRead":       true,
	"MarkThreadUnread":     true,
	"SwitchThread":         true,
	"GetThreadRuntimeMode": true,

	// Usage accounting reads (append-only ledger aggregates and the
	// latest provider-reported quota windows; no credentials, no FS).
	"GetUsageStats":          true,
	"GetRateLimitsSnapshots": true,

	// GetClaudeSlashCommands returns {name, description, argumentHint} for
	// the provider-executed commands the last account probe reported. It is
	// the composer's cold-thread menu seed and is deliberately NOT
	// loopback-only, on the same grounds as GetKeybindings below:
	//
	//   - it never spawns, never reads the filesystem, and never touches
	//     credentials — it is an in-memory read of what a probe already
	//     left behind (internal/claudecommands);
	//   - the shape carries no paths and no environment: names, prose
	//     descriptions, and argument hints, which is UI affordance data,
	//     not the URL/bearer-reference inventory that puts the MCP surface
	//     in category 8;
	//   - the SAME rich entries already reach remote peers on the
	//     `provider:commands` event channel, which is not in
	//     loopbackOnlyEventChannels, so refusing the RPC would buy no
	//     confidentiality while emptying the command menu on any thread
	//     without a live session.
	//
	// A future reviewer running this exercise: if provider:commands ever
	// becomes loopback-only, or the shape grows a path/env field, re-run the
	// decision — don't leave this entry standing on a premise that moved.
	"GetClaudeSlashCommands": true,

	// Per-client UI view state (ui_state table). Remote clients are
	// the point: each presents an opaque client ID and can only touch
	// its own "client:<id>" scope (built server-side in app_uistate.go,
	// which also bounds batch/key/value sizes). Opaque preference
	// strings — no credentials, no FS, same reasoning as keybindings.
	"GetUIState":    true,
	"SetUIState":    true,
	"DeleteUIState": true,

	// Timeline reads (item slice / turn / search).
	"GetThreadItem":           true,
	"ListItems":               true,
	"ListItemsAfterCursor":    true,
	"ListItemsAfterTurn":      true,
	"ListItemsBeforeCursor":   true,
	"ListItemsBeforeTurn":     true,
	"ListRecentThreadItems":   true,
	"ListRecentTurns":         true,
	"ListSubagentDescendants": true,
	"ListThreadSliceAround":   true,
	// The stamp-gated form of ListThreadSliceAround: same store read,
	// same window, plus the two counters that let the caller skip the
	// rows entirely. Store-read-only — no FS, no process, no credentials
	// — and it is the RPC remote clients benefit from most.
	"SyncThreadWindow":     true,
	"SearchThreadMessages": true,
	"SearchThreadItems":    true,

	// Payload reads. Authorization via getThreadPayloadMeta's
	// (threadID, payloadID) linkage check. Moved from LocalOnly
	// to support remote-mode timeline rendering.
	"GetPayloadPreview": true,
	"GetPayloadChunk":   true,
	"GetPayloadData":    true,

	// Edits-scope review reads: SQLite-only projections of persisted
	// tool-call diff payloads (no FS, no git, no provider session).
	"ListThreadEditDiffs": true,
	"GetTurnEditsDiff":    true,

	// Live-state counts (the per-thread surface is LocalOnly in
	// category 2 because it leaks composer drafts; these are
	// global-thread-count reads with no sensitive content).
	"CountRunningBackgroundTasks": true,
	"ListLiveBackgroundTasks":     true,

	// Discussion CRUD + transcript reads. Channel messages and FSM
	// state are the user's deliberation surface from the browser — both
	// pure reads with no provider side effect. PostChannelMessage moved
	// to LocalOnly (internalmethods.go category 2): it now arms the
	// next participant's turn prompt, which drives a live provider
	// session the same way SendMessage does. This is the "discussion
	// channels grow a side-effecting path" case the comment used to
	// warn about.
	"CreateDiscussion":         true,
	"DeleteDiscussion":         true,
	"UpdateDiscussion":         true,
	"GetDiscussion":            true,
	"ListDiscussions":          true,
	"ListDiscussionsForThread": true,
	"GetChannelMessages":       true,
	"GetChannelState":          true,

	// Proposed-plan inline comments. CRUD is wire-safe; the
	// LocalOnly SendPlanRevisionComments path is what hands them to
	// the provider for a revision turn.
	"CreateProposedPlanComment": true,
	"DeleteProposedPlanComment": true,
	"UpdateProposedPlanComment": true,
	"ListProposedPlanComments":  true,
	"ListThreadProposedPlans":   true,

	// Design-mode read of stored option choices. Workdir mutations
	// are LocalOnly in category 4.
	"ListDesignOptions": true,

	// Attachment listings (metadata only — bytes/thumbnails are
	// LocalOnly in category 4).
	"ListAttachments": true,

	// Composer-favorite reads (writes go through SetChatBarFavorite,
	// which is LocalOnly in category 3).
	"ListChatBarFavorites": true,

	// Settings reads. GetSettings defensively redacts every
	// RemoteEndpoint.Token before returning so the LAN-bound caller
	// cannot enumerate saved credentials; the on-demand
	// GetRemoteEndpointToken path stays loopback-only in category 6.
	"GetSettings":        true,
	"GetEditorSettings":  true,
	"GetContextSettings": true,
	// GetKeybindings: see the explicit carve-out comment in
	// internalmethods.go above the LocalOnlyMethods map. Frontend
	// has no client-side defaults; LocalOnly would zero every
	// keyboard shortcut on the remote browser. Mutations stay
	// LocalOnly in category 3.
	"GetKeybindings": true,

	// Host environment probe (no FS read, no credential).
	"IsWSL": true,

	"WorkflowListItems":           true,
	"WorkflowListUnresolvedItems": true,
	"WorkflowGetItem":             true,
	"WorkflowListItemCosts":       true,
	"WorkflowListDefinitions":     true,
	"WorkflowGetJobNotes":         true,
	// Automation rows: definition, trigger rendering, next fire, and the fire
	// record. Read-only, no FS/process reach; every automation mutation
	// (including Run now) is LocalOnly.
	"WorkflowListAutomations": true,
	// Pure read of the global pause flag — one boolean, no FS/process
	// reach. The mutating WorkflowSetGlobalPause stays LocalOnly.
	"WorkflowGetEngineState": true,

	// Build version string.
	"Version": true,
}

// TestGeneratedMethods_AllClassified is the positive partner to
// TestLocalOnlyMethods_AllExist. It walks every GeneratedMethods entry
// and asserts the method is classified as EITHER LocalOnly (in
// internalmethods.go) OR wireSafe (the allowlist above) — never
// neither. A new App method that lands without a classification fails
// the test with a name + remediation pointer.
//
// Failure here means a new method needs a deliberate LAN-safety
// decision: lock it down in LocalOnlyMethods or add it to
// wireSafeMethods.
func TestGeneratedMethods_AllClassified(t *testing.T) {
	var unclassified []string
	for _, m := range GeneratedMethods {
		if !LocalOnlyMethods[m.Name] && !wireSafeMethods[m.Name] {
			unclassified = append(unclassified, m.Name)
		}
	}
	if len(unclassified) == 0 {
		return
	}
	sort.Strings(unclassified)
	t.Fatalf(
		"%d App methods landed without a LAN-safety classification.\n"+
			"Each method below must be added to EITHER LocalOnlyMethods "+
			"(internal/transport/internalmethods.go) — for methods that touch "+
			"local FS, spawn processes, mutate settings, control a provider "+
			"session, write attachments, or return credentials — OR "+
			"wireSafeMethods (this file) for methods that are safe to expose "+
			"to LAN-attached peers.\nUnclassified: %v",
		len(unclassified), unclassified,
	)
}

// TestWireSafeMethods_AllExist guards wireSafeMethods against silent
// decay (the symmetric check to TestLocalOnlyMethods_AllExist). A
// rename in the App receiver should fail the test rather than leave a
// stale entry that quietly stops covering anything.
func TestWireSafeMethods_AllExist(t *testing.T) {
	known := make(map[string]bool, len(GeneratedMethods))
	for _, m := range GeneratedMethods {
		known[m.Name] = true
	}
	for name := range wireSafeMethods {
		if !known[name] {
			t.Errorf("wireSafeMethods[%q] does not match any entry in GeneratedMethods — typo or stale entry", name)
		}
	}
}

// TestWireSafeAndLocalOnlyDisjoint asserts the two classification sets
// don't overlap. A method classified both ways is a logic bug: the
// runtime would refuse it from LAN callers (LocalOnly wins in the
// dispatcher), while the test gate would treat it as deliberately
// exposed. Catching the overlap statically is the right place.
func TestWireSafeAndLocalOnlyDisjoint(t *testing.T) {
	for name := range wireSafeMethods {
		if LocalOnlyMethods[name] {
			t.Errorf("%q appears in both wireSafeMethods and LocalOnlyMethods — pick one", name)
		}
	}
}

// TestMethodsGen_InSync regenerates methods_gen.go into a tempfile and
// asserts the bytes match the committed file. A developer who adds an
// App method without running `go run ./internal/transport/methodgen`
// fails this test, and the failure message points to the fix.
//
// Skipped on Windows in CI because the methodgen tool reads the repo
// root and the relative-path math depends on POSIX-y filesystem layout.
// The CI matrix runs the test on Linux, which is sufficient.
func TestMethodsGen_InSync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("methodgen integrity test runs on POSIX CI")
	}

	repoRoot := findRepoRoot(t)

	tempDir := t.TempDir()
	tempOut := filepath.Join(tempDir, "methods_gen.go")

	cmd := exec.Command("go", "run", "./internal/transport/methodgen",
		"-out", tempOut,
		"-root", repoRoot,
	)
	cmd.Dir = repoRoot
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run methodgen: %v", err)
	}

	want, err := os.ReadFile(tempOut)
	if err != nil {
		t.Fatalf("read tempfile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repoRoot, "internal/transport/methods_gen.go"))
	if err != nil {
		t.Fatalf("read committed: %v", err)
	}

	if !bytes.Equal(want, got) {
		t.Fatalf("methods_gen.go is out of sync with App methods.\n" +
			"Run `go run ./internal/transport/methodgen` and commit the result.")
	}
}

// TestLocalOnlyMethods_AllExist guards the LocalOnlyMethods set against
// silent decay. Every name in LocalOnlyMethods MUST correspond to a
// real method in GeneratedMethods — a typo would otherwise let a
// LAN-attached caller invoke the privileged method with no enforcement
// at all (the dispatcher would never find a name match, so the LAN-
// only refusal branch wouldn't fire either).
//
// Failure here means LocalOnlyMethods drifted: rename the entry to
// match the App method, or drop it if the App method has been removed.
func TestLocalOnlyMethods_AllExist(t *testing.T) {
	known := make(map[string]bool, len(GeneratedMethods))
	for _, m := range GeneratedMethods {
		known[m.Name] = true
	}
	for name := range LocalOnlyMethods {
		if !known[name] {
			t.Errorf("LocalOnlyMethods[%q] does not match any entry in GeneratedMethods — typo or stale entry", name)
		}
	}
}

// findRepoRoot walks up from the test binary's location until it
// finds go.mod. Tests run from internal/transport/, so we expect to
// find go.mod two levels up.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (no go.mod above test cwd)")
	return ""
}
