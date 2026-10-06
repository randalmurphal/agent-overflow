package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"agent-overflow/internal/attachedbackends"
	"agent-overflow/internal/buildvariant"
	"agent-overflow/internal/deviceclient"
	"agent-overflow/internal/transport"

	"github.com/google/uuid"
)

// remoteOwnerFiles hold the bound methods of the remote-access features.
// Every exported method in them is either marked //ao:remote, so a build
// without remote access refuses it, or named in remoteOwnerLocalMethods
// with the reason it stays callable. A method added to one of these files
// without that decision fails here.
var remoteOwnerFiles = []string{
	"app_access.go",
	"app_backends.go",
	"app_computer_discovery.go",
	"app_computer_pairing.go",
	"app_native_network.go",
	"app_network.go",
	"app_own_devices.go",
	"app_passkey.go",
	"app_preview.go",
	"app_push.go",
	"app_remote_artifacts.go",
	"app_remote_jobs.go",
	"app_remote_watch.go",
	"app_service_update.go",
	"app_ssh.go",
	"app_tailnet.go",
	"app_thread_transfer.go",
}

// remoteOwnerLocalMethods stay callable in every build. They read state,
// withdraw access or settle work already started; none of them can enable
// remote access or reach another computer (outbound connections are also
// refused by internal/deviceclient's pinned transport).
var remoteOwnerLocalMethods = map[string]string{
	"GetAccessOverview":               "reads the device list",
	"DevicePairingStatus":             "reads a pairing that can no longer be minted",
	"CancelDevicePairing":             "withdraws access",
	"RevokeAccessDevice":              "withdraws access",
	"ForgetAccessDevice":              "withdraws access",
	"RevokeAccessSession":             "withdraws access",
	"ListBackends":                    "lists this computer, which the page needs",
	"RemoveBackend":                   "withdraws access",
	"RenameBackend":                   "names a saved computer",
	"CloseComputerPairing":            "withdraws access",
	"ComputerPairingStatus":           "reads a pairing window that can no longer open",
	"GetNativeNetworkConfig":          "answers the launcher; reports LAN disabled",
	"ReportNativeNetworkState":        "accepts the launcher's report",
	"GetNetworkSettings":              "reads settings",
	"SetNetworkSettings":              "settings validation refuses every remote field",
	"ListOwnDevices":                  "reads the device list",
	"RemoveOwnDevice":                 "withdraws access",
	"ListPasskeys":                    "reads credentials",
	"DeletePasskey":                   "withdraws access",
	"BeginPasskeyStepUp":              "proves presence for a local call",
	"FinishPasskeyStepUp":             "proves presence for a local call",
	"VerifyBrowserUnlock":             "unlocks a page on this computer",
	"GetDevServers":                   "reads this computer's dev servers",
	"DisallowPreviewPort":             "withdraws access",
	"UnregisterPushToken":             "withdraws access",
	"ClearPushSenderCredential":       "withdraws access",
	"GetPushSenderStatus":             "reads settings",
	"RemoteCommandArtifact":           "serves history to an existing peer session, which cannot exist",
	"RemoteCommandLogArtifact":        "serves history to an existing peer session, which cannot exist",
	"AgentRemoteFetchArtifact":        "reads history; the fetch dials through the refused transport",
	"AgentRemoteFetchLog":             "reads history; the fetch dials through the refused transport",
	"RemoteCommandStatus":             "reads history",
	"RemoteCommandCancel":             "settles work already started",
	"RemoteCommandProjects":           "reads this computer's projects",
	"ListAgentComputers":              "reads settings",
	"AgentRemoteComputers":            "reads settings",
	"AgentRemoteStatus":               "reads history",
	"AgentRemoteCancel":               "settles work already started",
	"ListThreadRemoteCommands":        "reads history",
	"CancelThreadRemoteCommand":       "settles work already started",
	"GetServiceUpdateStatus":          "reads status",
	"ListServiceReleases":             "reads releases",
	"CancelServiceUpdate":             "settles work already started",
	"GetSSHConnection":                "reads a setup that can no longer start",
	"CancelSSHConnection":             "withdraws access",
	"ForgetTailnetNode":               "removes tailnet state",
	"RetryThreadTransfer":             "settles work already started",
	"DiscardUnpreparedThreadTransfer": "settles work already started",
	"CancelThreadTransfer":            "settles work already started",
}

func TestRemoteOwnerMethodsAreClassified(t *testing.T) {
	t.Parallel()
	generated := make(map[string]transport.MethodMeta, len(transport.GeneratedMethods))
	for _, m := range transport.GeneratedMethods {
		generated[m.Name] = m
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range remoteOwnerFiles {
		file, err := parser.ParseFile(fset, filepath.Join("internal", "app", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !fn.Name.IsExported() {
				continue
			}
			method := fn.Name.Name
			meta, bound := generated[method]
			if !bound {
				continue
			}
			seen[method] = true
			_, local := remoteOwnerLocalMethods[method]
			switch {
			case meta.Remote && local:
				t.Errorf("%s (%s) is //ao:remote and also listed as local", method, name)
			case !meta.Remote && !local:
				t.Errorf("%s (%s) is neither //ao:remote nor in remoteOwnerLocalMethods", method, name)
			}
		}
	}
	for method := range remoteOwnerLocalMethods {
		if !seen[method] {
			t.Errorf("remoteOwnerLocalMethods names %s, which no remote owner file binds", method)
		}
	}
}

// A build without remote access never offers the remote tools to a
// session, even with an enabled agent computer saved by a build that had
// it. The same fixture registers them in the standard build, so the
// refusal is the gate's and not the fixture's.
func TestRemoteToolsAreNeverOfferedWithoutRemoteAccess(t *testing.T) {
	t.Parallel()
	a := newTestAppWithStore(t)
	t.Cleanup(func() { _ = a.ServiceShutdown() })
	backends := t.TempDir()
	var err error
	if a.backends, err = attachedbackends.New(backends, "source", "test"); err != nil {
		t.Fatal(err)
	}
	peer := deviceclient.Session{BackendID: uuid.NewString(), SessionID: "test", Credential: "test", Endpoint: "https://127.0.0.1:1"}
	if err := deviceclient.SaveSession(backends, peer); err != nil {
		t.Fatal(err)
	}
	if err := a.backends.SetAgentAccess(peer.BackendID, true); err != nil {
		t.Fatal(err)
	}
	thread := makeWorkspaceThread(t, a, uuid.NewString())
	config, err := a.remoteMCPConfigForThread(thread, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if buildvariant.RemoteAccess != (len(config) > 0) {
		t.Fatalf("remote tools registered = %v in a build with remote access = %v", len(config) > 0, buildvariant.RemoteAccess)
	}
}
