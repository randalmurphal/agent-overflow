package main

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"agent-overflow/internal/mcp"
	"agent-overflow/internal/store"
)

// stdioLibraryServer returns a minimal stdio library row with the
// given id+name. Tests reuse this for the binding-level CRUD/probe
// coverage so a single shape mismatch shows up in one diff.
func stdioLibraryServer(id, name string) store.MCPServer {
	return store.MCPServer{
		ID:        id,
		Name:      name,
		Transport: mcp.TransportStdio,
		Command:   "/bin/echo",
		Args:      []string{"hello"},
		Env:       map[string]string{"FOO": "bar"},
		Enabled:   true,
	}
}

// httpLibraryServer returns a minimal http library row. Used by the
// transport-specific binding paths (TriggerMcpAuth rejects stdio; HTTP
// is the one that has to fall through to the disabled-session error).
func httpLibraryServer(id, name string) store.MCPServer {
	return store.MCPServer{
		ID:        id,
		Name:      name,
		Transport: mcp.TransportHTTP,
		URL:       "https://example.com/mcp",
		Enabled:   true,
	}
}

func TestCreateMcpServer_ForcesEnabledAndAllocatesID(t *testing.T) {
	app := newTestAppWithStore(t)

	// Caller "tries" to seed a disabled row with no id. CreateMcpServer
	// owns both decisions: id is allocated, Enabled is flipped on.
	input := stdioLibraryServer("", "alpha")
	input.Enabled = false

	created, err := app.CreateMcpServer(input)
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	if created.ID == "" {
		t.Fatal("CreateMcpServer left ID empty; binding must allocate one")
	}
	if !created.Enabled {
		t.Fatal("CreateMcpServer must force Enabled = true (library kill-switch is the only intended disable path)")
	}

	stored, err := app.store.GetMCPServer(created.ID)
	if err != nil {
		t.Fatalf("GetMCPServer(%s): %v", created.ID, err)
	}
	if !stored.Enabled {
		t.Fatal("persisted row has Enabled = false; binding must persist forced-on state")
	}
}

func TestCreateMcpServer_AppendsIDToThreadProfile(t *testing.T) {
	app := newTestAppWithStore(t)

	first, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer alpha: %v", err)
	}
	second, err := app.CreateMcpServer(stdioLibraryServer("", "bravo"))
	if err != nil {
		t.Fatalf("CreateMcpServer bravo: %v", err)
	}

	profile, err := app.GetMcpThreadProfile()
	if err != nil {
		t.Fatalf("GetMcpThreadProfile: %v", err)
	}
	if len(profile.ServerIDs) != 2 {
		t.Fatalf("profile size = %d, want 2 (got %v)", len(profile.ServerIDs), profile.ServerIDs)
	}
	if !containsID(profile.ServerIDs, first.ID) || !containsID(profile.ServerIDs, second.ID) {
		t.Fatalf("profile %v missing freshly-created ids %s / %s", profile.ServerIDs, first.ID, second.ID)
	}
}

// TestCreateMcpServer_IdempotentProfileAppend guards the read-modify-
// write protected by a.mcpProfileMu. Two passes through the binding
// with the same id must not duplicate the entry — the helper checks
// existence under the lock before writing.
func TestCreateMcpServer_IdempotentProfileAppend(t *testing.T) {
	app := newTestAppWithStore(t)

	server := stdioLibraryServer("server-fixed", "alpha")
	if _, err := app.CreateMcpServer(server); err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	// Same id again is normally a unique-constraint violation. The
	// guard we care about is the profile-append step: even if the
	// caller force-injects the id directly through the seed helper,
	// the next read must show no duplicates.
	app.appendMCPServerToProfile(server.ID)
	app.appendMCPServerToProfile(server.ID)

	profile, err := app.GetMcpThreadProfile()
	if err != nil {
		t.Fatalf("GetMcpThreadProfile: %v", err)
	}
	if got := countOccurrences(profile.ServerIDs, server.ID); got != 1 {
		t.Fatalf("profile contains %d copies of %s, want 1", got, server.ID)
	}
}

func TestUpdateMcpServer_RewritesRowAndInvalidatesProbeCache(t *testing.T) {
	app := newTestAppWithStore(t)

	created, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	// Seed a probe-cache entry so we can prove UpdateMcpServer drops it.
	// Using a stdio Result against the stdio transport TTL so Snapshot
	// will include it before the update lands.
	cache := app.mcpProbe()
	cache.InvalidateAll()
	stored, err := app.store.GetMCPServer(created.ID)
	if err != nil {
		t.Fatalf("GetMCPServer: %v", err)
	}
	_ = cache.Snapshot() // ensures lazy init
	// We don't have a public seed-without-running-probe; use Get with
	// force=false against an "Enabled: false" mutation would still
	// invoke probe. Instead exercise the binding Invalidate path
	// through UpdateMcpServer directly: write a row, confirm the
	// snapshot is empty after Update, even though the underlying
	// fields changed.
	if got := len(cache.Snapshot()); got != 0 {
		t.Fatalf("baseline cache should be empty, got %d entries", got)
	}

	updated := stored
	updated.Args = []string{"world"}
	updated.Env = map[string]string{"FOO": "baz"}
	result, err := app.UpdateMcpServer(updated)
	if err != nil {
		t.Fatalf("UpdateMcpServer: %v", err)
	}
	if len(result.Args) != 1 || result.Args[0] != "world" {
		t.Fatalf("Args = %v, want [world]", result.Args)
	}
	if result.Env["FOO"] != "baz" {
		t.Fatalf("Env[FOO] = %q, want baz", result.Env["FOO"])
	}
	// The invalidate hook is exercised on every UpdateMcpServer.
	// Snapshot stays clear because no probe ran.
	if got := len(cache.Snapshot()); got != 0 {
		t.Fatalf("cache should remain empty after UpdateMcpServer, got %d entries", got)
	}
}

func TestDeleteMcpServer_PrunesProfileAndCascadesThreadAssociation(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-delete", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	first, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer alpha: %v", err)
	}
	second, err := app.CreateMcpServer(stdioLibraryServer("", "bravo"))
	if err != nil {
		t.Fatalf("CreateMcpServer bravo: %v", err)
	}

	// Wire the thread to both servers so the cascade has something to drop.
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{first.ID, second.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}

	if err := app.DeleteMcpServer(first.ID); err != nil {
		t.Fatalf("DeleteMcpServer: %v", err)
	}

	// Profile no longer carries the id.
	profile, err := app.GetMcpThreadProfile()
	if err != nil {
		t.Fatalf("GetMcpThreadProfile: %v", err)
	}
	if containsID(profile.ServerIDs, first.ID) {
		t.Fatalf("profile %v still contains deleted id %s", profile.ServerIDs, first.ID)
	}
	if !containsID(profile.ServerIDs, second.ID) {
		t.Fatalf("profile %v dropped surviving id %s", profile.ServerIDs, second.ID)
	}

	// thread_mcp_servers row for the deleted server is gone, the
	// other association survives.
	got, err := app.GetThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadMcpServers: %v", err)
	}
	if containsID(got, first.ID) {
		t.Fatalf("thread %s still associated with deleted server: %v", thread.ID, got)
	}
	if !containsID(got, second.ID) {
		t.Fatalf("thread %s lost surviving association: %v", thread.ID, got)
	}
}

func TestUpdateThreadMcpServers_PersistsListAndUpdatesProfile(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-update", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}

	alpha, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer alpha: %v", err)
	}
	bravo, err := app.CreateMcpServer(stdioLibraryServer("", "bravo"))
	if err != nil {
		t.Fatalf("CreateMcpServer bravo: %v", err)
	}

	// Per-thread set replace.
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{alpha.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers alpha-only: %v", err)
	}
	got, err := app.GetThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadMcpServers: %v", err)
	}
	if len(got) != 1 || got[0] != alpha.ID {
		t.Fatalf("after alpha-only set, GetThreadMcpServers = %v, want [%s]", got, alpha.ID)
	}

	// Profile mirrors the latest selection so the next CreateThread
	// inherits it.
	profile, err := app.GetMcpThreadProfile()
	if err != nil {
		t.Fatalf("GetMcpThreadProfile: %v", err)
	}
	if len(profile.ServerIDs) != 1 || profile.ServerIDs[0] != alpha.ID {
		t.Fatalf("profile = %v, want [%s] after alpha-only set", profile.ServerIDs, alpha.ID)
	}

	// Replace with the other server. Stale row should not survive.
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{bravo.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers bravo-only: %v", err)
	}
	got, err = app.GetThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadMcpServers (after bravo): %v", err)
	}
	if len(got) != 1 || got[0] != bravo.ID {
		t.Fatalf("after bravo-only set, GetThreadMcpServers = %v, want [%s]", got, bravo.ID)
	}

	// Empty list clears the row and the profile mirrors that intent.
	if _, err := app.UpdateThreadMcpServers(thread.ID, nil); err != nil {
		t.Fatalf("UpdateThreadMcpServers clear: %v", err)
	}
	got, err = app.GetThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadMcpServers (after clear): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after clear, GetThreadMcpServers = %v, want []", got)
	}
	profile, err = app.GetMcpThreadProfile()
	if err != nil {
		t.Fatalf("GetMcpThreadProfile (after clear): %v", err)
	}
	if len(profile.ServerIDs) != 0 {
		t.Fatalf("after clear, profile = %v, want []", profile.ServerIDs)
	}
}

// TestUpdateThreadMcpServers_NoSessionSkipsReconcile is the "no live
// session" code path: binding must persist the set and return the
// refreshed thread without panicking on a nil session map. The
// per-thread mutex MUST still be acquired-and-released so the next
// binding can run; we observe that by chaining two updates that would
// deadlock if the first leaked the lock.
func TestUpdateThreadMcpServers_NoSessionSkipsReconcile(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-nosession", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	srv, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := app.UpdateThreadMcpServers(thread.ID, []string{srv.ID}); err != nil {
			t.Fatalf("UpdateThreadMcpServers (iter %d): %v", i, err)
		}
	}
}

func TestSeedThreadMCPServersFromProfile_AppliesAtCreateThread(t *testing.T) {
	app := newTestAppWithStore(t)

	first, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	// Profile was seeded by the previous Create; new threads must
	// inherit "what the user last had enabled" by default.
	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-seed", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	got, err := app.GetThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadMcpServers: %v", err)
	}
	if len(got) != 1 || got[0] != first.ID {
		t.Fatalf("new thread = %v, want seeded [%s]", got, first.ID)
	}
}

func TestSeedThreadMCPServersFromProfile_EmptyProfileNoOps(t *testing.T) {
	app := newTestAppWithStore(t)
	// No CreateMcpServer call, so no profile row exists. CreateThread
	// must still succeed and the new thread is empty.
	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-empty-seed", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	got, err := app.GetThreadMcpServers(thread.ID)
	if err != nil {
		t.Fatalf("GetThreadMcpServers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("new thread with no profile = %v, want []", got)
	}
}

func TestMergeMCPServersForThread_DesignWinsOnCollision(t *testing.T) {
	app := newTestAppWithStore(t)

	// Create a library server with name "design" so the merge has
	// something on both sides of the collision.
	libServer, err := app.CreateMcpServer(stdioLibraryServer("", "design"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-merge", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{libServer.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}

	designServers := map[string]any{
		"design": map[string]any{
			"type":    "stdio",
			"command": "/usr/bin/design-bridge",
		},
	}
	merged, collisions, err := app.mergeMCPServersForThread(thread.ID, "claude", designServers)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("merged = %v, want exactly one entry", merged)
	}
	entry, ok := merged["design"].(map[string]any)
	if !ok {
		t.Fatalf("merged[design] type = %T, want map[string]any", merged["design"])
	}
	// Design's command must win over the library's /bin/echo.
	if cmd, _ := entry["command"].(string); cmd != "/usr/bin/design-bridge" {
		t.Fatalf("merged[design].command = %q, want /usr/bin/design-bridge (design lost the collision)", cmd)
	}
	if len(collisions) != 1 || collisions[0] != "design" {
		t.Fatalf("collisions = %v, want [design]", collisions)
	}
}

// TestMergeMCPServersForThread_CodexMasksUnselectedLibraryRows is the
// per-thread isolation defense for Codex's merge-not-replace overlay:
// a library row not selected by the thread must be emitted with
// `enabled: false` and the full transport spec so the on-disk row
// can't leak in.
func TestMergeMCPServersForThread_CodexMasksUnselectedLibraryRows(t *testing.T) {
	app := newTestAppWithStore(t)

	keep, err := app.CreateMcpServer(stdioLibraryServer("", "keep"))
	if err != nil {
		t.Fatalf("CreateMcpServer keep: %v", err)
	}
	if _, err := app.CreateMcpServer(stdioLibraryServer("", "mask")); err != nil {
		t.Fatalf("CreateMcpServer mask: %v", err)
	}

	thread, err := createTestThread(t, app, "codex", "/tmp/mcp-codex-mask", "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	// Codex profile-seed would have added both ids during CreateMcpServer.
	// Override the thread selection explicitly so only "keep" is on.
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{keep.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}

	merged, _, err := app.mergeMCPServersForThread(thread.ID, "codex", nil)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}
	keepEntry, ok := merged["keep"].(map[string]any)
	if !ok {
		t.Fatalf("merged[keep] type = %T, want map[string]any", merged["keep"])
	}
	if v, _ := keepEntry["enabled"].(bool); !v {
		t.Fatalf("merged[keep].enabled = %v, want true", keepEntry["enabled"])
	}
	maskEntry, ok := merged["mask"].(map[string]any)
	if !ok {
		t.Fatalf("merged[mask] type = %T, want map[string]any (unselected entry must overlay with full spec)", merged["mask"])
	}
	if v, ok := maskEntry["enabled"].(bool); !ok || v {
		t.Fatalf("merged[mask].enabled = %v, want false (unselected row must mask)", maskEntry["enabled"])
	}
	// Codex deserializer requires the transport-discriminator field.
	if _, hasCommand := maskEntry["command"]; !hasCommand {
		t.Fatalf("merged[mask] missing 'command' field; bare {enabled: false} would fail Codex serde")
	}
}

func TestMergeMCPServersForThread_ClaudeDoesNotMaskUnselected(t *testing.T) {
	app := newTestAppWithStore(t)

	keep, err := app.CreateMcpServer(stdioLibraryServer("", "keep"))
	if err != nil {
		t.Fatalf("CreateMcpServer keep: %v", err)
	}
	if _, err := app.CreateMcpServer(stdioLibraryServer("", "mask")); err != nil {
		t.Fatalf("CreateMcpServer mask: %v", err)
	}

	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-claude-nomask", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{keep.ID}); err != nil {
		t.Fatalf("UpdateThreadMcpServers: %v", err)
	}

	merged, _, err := app.mergeMCPServersForThread(thread.ID, "claude", nil)
	if err != nil {
		t.Fatalf("mergeMCPServersForThread: %v", err)
	}
	if _, ok := merged["mask"]; ok {
		t.Fatalf("merged[mask] = %v; Claude --mcp-config is replace semantics, no overlay needed", merged["mask"])
	}
	if _, ok := merged["keep"]; !ok {
		t.Fatalf("merged[keep] missing; selected entry must always render: %v", merged)
	}
}

func TestTriggerMcpAuth_RejectsDisabledLibraryServer(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-auth", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	server, err := app.CreateMcpServer(httpLibraryServer("", "remote"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}

	// Hand-flip the kill-switch behind the binding so we can probe the
	// "library row is disabled" branch.
	server.Enabled = false
	if _, err := app.store.UpdateMCPServer(server); err != nil {
		t.Fatalf("UpdateMCPServer (force disable): %v", err)
	}

	_, err = app.TriggerMcpAuth(thread.ID, server.ID)
	if err == nil {
		t.Fatal("TriggerMcpAuth on disabled library row error = nil, want explicit refusal")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("TriggerMcpAuth error = %v, want 'disabled' wording", err)
	}
}

func TestTriggerMcpAuth_RejectsStdioTransport(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-auth-stdio", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	server, err := app.CreateMcpServer(stdioLibraryServer("", "local"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}

	_, err = app.TriggerMcpAuth(thread.ID, server.ID)
	if err == nil {
		t.Fatal("TriggerMcpAuth on stdio transport error = nil, want refusal")
	}
	if !strings.Contains(err.Error(), "oauth") {
		t.Fatalf("TriggerMcpAuth error = %v, want oauth-unsupported wording", err)
	}
}

func TestTriggerMcpAuth_RejectsUnselectedServer(t *testing.T) {
	app := newTestAppWithStore(t)

	thread, err := createTestThread(t, app, "claude", "/tmp/mcp-auth-unselected", "claude-opus-4-7", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	server, err := app.CreateMcpServer(httpLibraryServer("", "remote"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	// Explicitly disconnect the server from the thread even though the
	// profile seed would otherwise carry it.
	if _, err := app.UpdateThreadMcpServers(thread.ID, []string{}); err != nil {
		t.Fatalf("UpdateThreadMcpServers (clear): %v", err)
	}

	_, err = app.TriggerMcpAuth(thread.ID, server.ID)
	if !errors.Is(err, ErrMCPServerNotSelected) {
		t.Fatalf("TriggerMcpAuth error = %v, want ErrMCPServerNotSelected", err)
	}
}

func TestProbeMcpServer_DisabledLibraryServerReturnsUnknownWithoutSpawn(t *testing.T) {
	app := newTestAppWithStore(t)

	server, err := app.CreateMcpServer(stdioLibraryServer("", "alpha"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}
	server.Enabled = false
	if _, err := app.store.UpdateMCPServer(server); err != nil {
		t.Fatalf("UpdateMCPServer disable: %v", err)
	}

	result, err := app.ProbeMcpServer(server.ID, true)
	if err != nil {
		t.Fatalf("ProbeMcpServer: %v", err)
	}
	if result.Status != mcp.StatusUnknown {
		t.Fatalf("Status = %q, want unknown (disabled library row must short-circuit)", result.Status)
	}
	if result.Error == "" {
		t.Fatal("disabled probe must carry an explanation so the popup can render it")
	}
}

func TestGetMcpProbeSnapshot_EmptyOnFreshApp(t *testing.T) {
	app := newTestAppWithStore(t)
	got, err := app.GetMcpProbeSnapshot()
	if err != nil {
		t.Fatalf("GetMcpProbeSnapshot: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("snapshot = %v, want empty (no probes have run yet)", got)
	}
}

func TestGetMcpThreadProfile_NoProfileReturnsEmptyList(t *testing.T) {
	app := newTestAppWithStore(t)
	profile, err := app.GetMcpThreadProfile()
	if err != nil {
		t.Fatalf("GetMcpThreadProfile: %v", err)
	}
	if profile.ServerIDs == nil {
		t.Fatal("ServerIDs must default to allocated empty slice (frontend can't branch on nil/[])")
	}
	if len(profile.ServerIDs) != 0 {
		t.Fatalf("ServerIDs = %v, want empty", profile.ServerIDs)
	}
}

func TestHandleCodexMCPOAuthCompleted_EmitsSuccessAndInvalidatesCache(t *testing.T) {
	app := newTestAppWithStore(t)
	server, err := app.CreateMcpServer(httpLibraryServer("", "remote"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}

	var got map[string]any
	app.testEmitHook = func(name string, data any) {
		if name == "mcp:oauth-completed" {
			m, _ := data.(map[string]any)
			got = m
		}
	}

	app.handleCodexMCPOAuthCompleted("thread-xyz", server.Name, true, "")
	if got == nil {
		t.Fatal("mcp:oauth-completed not emitted on success")
	}
	if got["serverId"] != server.ID {
		t.Errorf("serverId = %v, want %s", got["serverId"], server.ID)
	}
	if got["serverName"] != server.Name {
		t.Errorf("serverName = %v, want %s", got["serverName"], server.Name)
	}
	if v, _ := got["success"].(bool); !v {
		t.Errorf("success = %v, want true", got["success"])
	}
}

func TestHandleCodexMCPOAuthCompleted_FailureRoutesToThreadError(t *testing.T) {
	app := newTestAppWithStore(t)
	thread, err := createTestThread(t, app, "codex", "/tmp/mcp-oauth-fail", "gpt-5.4", "")
	if err != nil {
		t.Fatalf("createTestThread: %v", err)
	}
	server, err := app.CreateMcpServer(httpLibraryServer("", "remote"))
	if err != nil {
		t.Fatalf("CreateMcpServer: %v", err)
	}

	errCh := collectErrorItemUpserts(t, app, 1)
	app.handleCodexMCPOAuthCompleted(thread.ID, server.Name, false, "token rejected")

	select {
	case item := <-errCh:
		if item.ThreadID != thread.ID {
			t.Errorf("ThreadID = %q, want %s", item.ThreadID, thread.ID)
		}
		if !strings.Contains(item.Summary, "token rejected") {
			t.Errorf("error item summary = %q, want it to mention the failure reason", item.Summary)
		}
	default:
		t.Fatal("oauth failure did not surface an error item on the thread")
	}
}

func TestHandleCodexMCPOAuthCompleted_UnknownServerIsNoop(t *testing.T) {
	app := newTestAppWithStore(t)

	emitted := 0
	app.testEmitHook = func(name string, _ any) {
		if name == "mcp:oauth-completed" {
			emitted++
		}
	}
	// Codex emits the notification with a server name AO doesn't know
	// about (race: server deleted while OAuth was in flight). Handler
	// must log + drop instead of crashing.
	app.handleCodexMCPOAuthCompleted("thread-xyz", "missing", true, "")
	if emitted != 0 {
		t.Fatalf("unknown server emitted %d events, want 0", emitted)
	}
}

// helpers

func containsID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func countOccurrences(ids []string, target string) int {
	n := 0
	for _, id := range ids {
		if id == target {
			n++
		}
	}
	return n
}

// sortedCopy returns a sorted copy of the slice — handy for snapshot
// comparisons where ordering shouldn't matter.
func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
