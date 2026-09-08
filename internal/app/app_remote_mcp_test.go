package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"agent-overflow/internal/attachedbackends"
	appbrowser "agent-overflow/internal/browser"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/remotejobs"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"github.com/google/uuid"
)

func remoteMCPThread(t *testing.T, a *App, name string) (store.Thread, string) {
	t.Helper()
	project, err := a.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "source", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	thread := store.Thread{ID: uuid.NewString(), ProjectID: project.ID, Provider: name, ProjectPath: project.Path, WorkspacePath: project.Path}
	err = a.store.CreateThread(thread)
	if err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	a.sessionManager().put(thread.ID, session{Token: token, Provider: name})
	return thread, token
}

func remoteMCPEndpoint(t *testing.T, a *App, thread store.Thread, token string) string {
	t.Helper()
	configs, err := a.remoteMCPConfigForThread(thread, token)
	if err != nil {
		t.Fatal(err)
	}
	return configs[remoteMCPName].(map[string]any)["url"].(string)
}

func remoteMCPRequest(t *testing.T, endpoint, method string, params any) (int, map[string]json.RawMessage) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Post(endpoint, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]json.RawMessage
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, result
}

func remoteMCPCall(t *testing.T, endpoint, name string, args any, wantError bool) json.RawMessage {
	t.Helper()
	status, reply := remoteMCPRequest(t, endpoint, "tools/call", map[string]any{
		"name": name, "arguments": args,
		"_meta": map[string]any{"progressToken": 1, "callId": "test-call", "threadId": "provider-thread"},
	})
	if status != http.StatusOK || reply["error"] != nil {
		t.Fatalf("tool response: %d %s", status, reply)
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(reply["result"], &result); err != nil {
		t.Fatal(err)
	}
	if result.IsError != wantError || len(result.Content) != 1 {
		t.Fatalf("tool result: %s", reply["result"])
	}
	return json.RawMessage(result.Content[0].Text)
}

func TestRemoteMCPCommandsCrossPairedTLSAndRespectOwnership(t *testing.T) {
	destination := newPairedBackend(t)
	source := identityApp(t)
	manager, err := attachedbackends.New(t.TempDir(), "source", "test")
	if err != nil {
		t.Fatal(err)
	}
	source.backends = manager
	t.Cleanup(func() { _ = source.remoteMCPServer().Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	invite, _ := destination.mintLink(t, "full")
	peer, err := manager.Add(ctx, invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.app.ConfirmDevicePairing(invite.LinkID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Await(ctx, peer.ID); err != nil {
		t.Fatal(err)
	}
	project, err := destination.app.store.CreateProject(store.Project{ID: uuid.NewString(), Name: "target", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	destination.app.remoteJobs, err = remotejobs.New(ctx, destination.app.store, func(ctx context.Context, cwd string, argv []string, out io.Writer) (int, error) {
		starts.Add(1)
		if len(argv) == 2 && argv[1] == "--quick" {
			_, _ = io.WriteString(out, "quick result")
			return 0, nil
		}
		if cwd != project.Path || strings.Join(argv, " ") != "test-helper --wait" {
			t.Errorf("wrong execution: %s %v", cwd, argv)
		}
		_, _ = io.WriteString(out, strings.Repeat("é", 12000))
		<-ctx.Done()
		return -1, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(destination.app.remoteJobs.Close)
	thread, token := remoteMCPThread(t, source, string(provider.Codex))
	endpoint := remoteMCPEndpoint(t, source, thread, token)
	status, reply := remoteMCPRequest(t, endpoint, "initialize", map[string]any{})
	if status != 200 || strings.Contains(string(reply["result"]), "remote run") {
		t.Fatalf("initialize: %s", reply)
	}
	_, reply = remoteMCPRequest(t, endpoint, "tools/list", map[string]any{})
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(reply["result"], &tools); err != nil || len(tools.Tools) != 4 {
		t.Fatalf("tools: %s, %v", reply, err)
	}
	id := uuid.NewString()
	run := map[string]any{"computer_id": peer.ID, "project_id": project.ID, "request_id": id, "argv": []string{"test-helper", "--wait"}, "wait_seconds": 0}
	remoteMCPCall(t, endpoint, "remote_run", run, true) // Pairing is not command opt-in.
	if err := source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	list := remoteMCPCall(t, endpoint, "remote_computers", map[string]any{}, false)
	if !strings.Contains(string(list), project.ID) {
		t.Fatalf("missing destination project: %s", list)
	}
	invalid := map[string]any{"computer_id": peer.ID, "project_id": project.ID, "request_id": id, "argv": []string{"test-helper", "--wait"}, "max_output_bytes": -1}
	remoteMCPCall(t, endpoint, "remote_run", invalid, true)
	if starts.Load() != 0 {
		t.Fatal("invalid result options executed a command")
	}
	remoteMCPCall(t, endpoint, "remote_run", run, false)
	args := map[string]any{"computer_id": peer.ID, "request_id": id, "wait_seconds": 0.3}
	result := remoteMCPCall(t, endpoint, "remote_status", args, false)
	var receipt remoteMCPResult
	if err := json.Unmarshal(result, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "running" || receipt.SourceThreadID != thread.ID || len(receipt.Output) > defaultRemoteOutputBytes || receipt.OmittedOutputBytes == 0 || !utf8.ValidString(receipt.Output) {
		t.Fatalf("receipt: state=%s bytes=%d omitted=%d", receipt.State, len(receipt.Output), receipt.OmittedOutputBytes)
	}
	remoteMCPCall(t, endpoint, "remote_run", run, false)
	if starts.Load() != 1 {
		t.Fatal("retry executed twice")
	}
	other, otherToken := remoteMCPThread(t, source, string(provider.Claude))
	otherEndpoint := remoteMCPEndpoint(t, source, other, otherToken)
	remoteMCPCall(t, otherEndpoint, "remote_cancel", args, true)
	if err := source.setRemoteThreadMCPEnabled(thread, false); err != nil {
		t.Fatal(err)
	}
	remoteMCPCall(t, endpoint, "remote_status", args, true)
	_, reply = remoteMCPRequest(t, endpoint, "tools/list", nil)
	if string(reply["result"]) != "{\"tools\":[]}" {
		t.Fatalf("disabled list: %s", reply)
	}
	if err := source.setRemoteThreadMCPEnabled(thread, true); err != nil {
		t.Fatal(err)
	}
	if err := source.SetAgentComputerEnabled(ctx, peer.ID, false); err != nil {
		t.Fatal(err)
	}
	run["request_id"] = uuid.NewString()
	remoteMCPCall(t, endpoint, "remote_run", run, true)
	args["max_output_bytes"] = 0
	remoteMCPCall(t, endpoint, "remote_cancel", args, false)
	result = remoteMCPCall(t, endpoint, "remote_status", args, false)
	receipt = remoteMCPResult{}
	if err := json.Unmarshal(result, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "canceled" || receipt.Output != "" {
		t.Fatalf("cancel receipt: %s", result)
	}
	if err := source.SetAgentComputerEnabled(ctx, peer.ID, true); err != nil {
		t.Fatal(err)
	}
	quick := map[string]any{"computer_id": peer.ID, "project_id": project.ID, "request_id": uuid.NewString(), "argv": []string{"test-helper", "--quick"}}
	result = remoteMCPCall(t, endpoint, "remote_run", quick, false)
	receipt = remoteMCPResult{}
	if err := json.Unmarshal(result, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "succeeded" || receipt.Output != "quick result" {
		t.Fatalf("quick command did not return its result: %s", result)
	}
	source.sessionManager().take(thread.ID)
	remoteMCPCall(t, endpoint, "remote_status", args, true)
	replacement := remoteMCPEndpoint(t, source, thread, "new-session")
	source.revokeRemoteMCP(thread.ID, token)
	if status, _ := remoteMCPRequest(t, endpoint, "tools/list", nil); status != 404 {
		t.Fatal("old capability survived replacement")
	}
	if status, _ := remoteMCPRequest(t, replacement, "tools/list", nil); status != 200 {
		t.Fatal("old teardown revoked replacement")
	}
}

func TestRemoteMCPRegistrationAndRowsKeepProvidersAndBrowserSeparate(t *testing.T) {
	a := identityApp(t)
	a.backends, _ = attachedbackends.New(t.TempDir(), "source", "test")
	t.Cleanup(func() { _ = a.remoteMCPServer().Close() })
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		thread, token := remoteMCPThread(t, a, name)
		endpoint := remoteMCPEndpoint(t, a, thread, token)
		_, reply := remoteMCPRequest(t, endpoint, "tools/list", nil)
		if string(reply["result"]) != "{\"tools\":[]}" {
			t.Fatalf("unpaired tools: %s", reply)
		}
		thread.Provider = string(provider.ClaudeTUI)
		if config, err := a.remoteMCPConfigForThread(thread, token); err != nil || len(config) != 0 {
			t.Fatal("TUI received remote MCP", config, err)
		}
	}
	// Browser projection must only touch its own row, even with a second managed server.
	a.browser.mcp = appbrowser.NewMCPServer(nil, false)
	t.Cleanup(func() { _ = a.browser.mcp.Close() })
	rows := a.withBrowserMCPRow(store.Thread{ID: "t"}, []ThreadMCPServer{{Name: remoteMCPName, Status: "connected"}}, true)
	if rows[0].Disabled || rows[0].Name != remoteMCPName || len(rows) != 2 {
		t.Fatalf("browser changed remote row: %#v", rows)
	}
	phase := transport.WithCallerScope(context.Background(), transport.CallerScope{Kind: transport.ScopeKindPhase, ThreadID: "phase"})
	if _, err := a.AgentRemoteComputers(phase); err == nil {
		t.Fatal("phase without remote-commands grant admitted")
	}
}

func TestRemoteMCPResultBudgetIsExplicitAndUTF8Safe(t *testing.T) {
	for _, budget := range []int{0, 1, 2, 3, 100} {
		got := remoteResult("target", RemoteCommand{Output: "aé界z", Truncated: true}, remoteResultOptions{MaxOutputBytes: &budget})
		if len(got.Output) > budget || !utf8.ValidString(got.Output) || got.RetainedOutputBytes != 7 || got.OmittedOutputBytes+len(got.Output) != 7 || !got.Truncated {
			t.Fatalf("budget %d: %#v", budget, got)
		}
	}
}

func TestRemoteMCPRefreshesLiveProvidersAndKeepsThreadDisable(t *testing.T) {
	for _, name := range []string{string(provider.Claude), string(provider.Codex)} {
		t.Run(name, func(t *testing.T) {
			a, _, _ := newMCPTestApp(t)
			a.backends, _ = attachedbackends.New(t.TempDir(), "source", "test")
			ctx, cancel := context.WithCancel(context.Background())
			a.appCtx = ctx
			t.Cleanup(func() { cancel(); a.remoteMCP.wg.Wait(); _ = a.remoteMCPServer().Close() })
			thread, token := remoteMCPThread(t, a, name)
			remoteMCPEndpoint(t, a, thread, token)
			capture := t.TempDir()
			if name == string(provider.Claude) {
				binary := writeClaudeMcpToggleCaptureBinary(t, capture)
				script, err := os.ReadFile(binary)
				if err != nil {
					t.Fatal(err)
				}
				script = []byte(strings.ReplaceAll(string(script), `"subtype":"mcp_toggle"`, `"subtype":"mcp_reconnect"`))
				if err := os.WriteFile(binary, script, 0700); err != nil {
					t.Fatal(err)
				}
				ps, err := claude.NewSession(ctx, thread.ID, claude.Config{Binary: binary, WorkDir: thread.WorkspacePath}, func(provider.ProviderEvent) {})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ps.Close() })
				a.sessionManager().put(thread.ID, session{Token: token, Provider: name, Claude: ps})
			} else {
				ps, err := codex.NewSession(ctx, thread.ID, codex.Config{Binary: writeCodexRefreshCaptureBinary(t, capture, "remote-provider", ""), Model: "gpt-5", WorkDir: thread.WorkspacePath}, func(provider.ProviderEvent) {})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = ps.Close() })
				a.sessionManager().put(thread.ID, session{Token: token, Provider: name, Codex: ps})
			}
			a.remoteMCPServer().SetThreadEnabled(thread.ID, false)
			// The refresh pass must not re-enable a thread the user opted out of.
			a.startRemoteMCPRefresh()
			a.signalRemotePeers()
			a.remoteMCPServer().SetThreadEnabled(thread.ID, true)
			a.signalRemotePeers()
			if name == string(provider.Claude) {
				envelope := readClaudeMcpToggleCapture(t, capture, 3*time.Second)
				if envelope.Request["serverName"] != remoteMCPName || envelope.Request["subtype"] != "mcp_reconnect" {
					t.Fatalf("refresh: %#v", envelope)
				}
			} else if got := readCodexReloadCapture(t, capture, 3*time.Second); got != "config/mcpServer/reload" {
				t.Fatalf("reload: %s", got)
			}
			// Tool gating remains immediate even while provider refresh is in flight.
			a.remoteMCPServer().SetThreadEnabled(thread.ID, false)
			if a.remoteMCPServer().ThreadEnabled(thread.ID) {
				t.Fatal("refresh reset thread opt-out")
			}
		})
	}
}
