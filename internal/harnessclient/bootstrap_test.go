package harnessclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/harness/instanceinfo"
)

func TestParseBootstrapLineIgnoresOrdinaryOutput(t *testing.T) {
	for _, line := range []string{"", "2026/08/26 transport: harness mode", "{}"} {
		if _, ok, err := ParseBootstrapLine(line); ok || err != nil {
			t.Errorf("ParseBootstrapLine(%q) = ok %v, err %v; want a miss", line, ok, err)
		}
	}
}

func TestParseBootstrapLineReadsThePayload(t *testing.T) {
	// The backend writes "\n__AO_HARNESS__: {json}\n" — prefix, a space,
	// then the payload — so the parser must tolerate the leading space
	// and any log noise the same line happened to carry.
	line := "some prefix " + BootstrapPrefix + ` {"url":"http://127.0.0.1:5173/?token=abc","port":5173,"token":"abc","dataRoot":"/tmp/root","dataDir":"/tmp/root/agent-overflow","mockProvider":"/bin/mock","pid":9,"version":"dev"}`
	bs, ok, err := ParseBootstrapLine(line)
	if err != nil || !ok {
		t.Fatalf("ParseBootstrapLine: ok %v err %v", ok, err)
	}
	if bs.Port != 5173 || bs.Token != "abc" || bs.DataDir != "/tmp/root/agent-overflow" {
		t.Fatalf("bootstrap = %+v", bs)
	}
	if want := "ws://127.0.0.1:5173/ws?token=abc"; bs.WSURL() != want {
		t.Fatalf("WSURL = %q, want %q", bs.WSURL(), want)
	}
}

func TestParseBootstrapLineRefusesABrokenPayload(t *testing.T) {
	// A line carrying the prefix is a contract, not noise: failing to
	// parse it must be an error rather than a silent miss that becomes a
	// 30-second timeout.
	if _, _, err := ParseBootstrapLine(BootstrapPrefix + " {not json"); err == nil {
		t.Fatal("a malformed bootstrap payload parsed cleanly")
	}
}

func TestWSURLEscapesTheToken(t *testing.T) {
	bs := Bootstrap{Port: 1234, Token: "a b/c+d"}
	if strings.Contains(bs.WSURL(), " ") {
		t.Fatalf("WSURL left a raw space in the query: %q", bs.WSURL())
	}
	if !strings.Contains(bs.WSURL(), "a+b%2Fc%2Bd") {
		t.Fatalf("WSURL = %q, want the token percent-escaped", bs.WSURL())
	}
}

func TestReadInstanceFileCarriesTheIdentityBlock(t *testing.T) {
	dataDir := t.TempDir()
	payload := map[string]any{
		"url": "http://127.0.0.1:9/", "port": 9, "token": "tok",
		"dataRoot": "/tmp/root", "dataDir": dataDir, "mockProvider": "/bin/mock",
		"pid": 4321, "version": "dev",
		"id": "0123abcd", "mode": "harness", "window": true,
		"worktree": "/repo", "startedAt": "2026-08-26T00:00:00Z",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, instanceinfo.InstanceFileName), data, 0o600); err != nil {
		t.Fatalf("write instance file: %v", err)
	}

	bs, err := ReadInstanceFile(dataDir)
	if err != nil {
		t.Fatalf("ReadInstanceFile: %v", err)
	}
	if bs.Token != "tok" || bs.PID != 4321 {
		t.Fatalf("bootstrap half = %+v", bs)
	}
	// The identity half is what ties the file to a registry row; a reader
	// that only got the bootstrap half could not tell soak from harness.
	if bs.ID != "0123abcd" || bs.Mode != instanceinfo.ModeHarness || !bs.Window {
		t.Fatalf("identity half = %+v", bs.Identity)
	}
}

func TestReadInstanceFileRefusesAnUnattachableFile(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := ReadInstanceFile(dataDir); err == nil {
		t.Fatal("a missing instance file read cleanly")
	}
	if err := os.WriteFile(filepath.Join(dataDir, instanceinfo.InstanceFileName), []byte(`{"port":0}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ReadInstanceFile(dataDir); err == nil {
		t.Fatal("an instance file with no port or token read cleanly")
	}
}

func TestBootstrapValidateForRejectsMismatchedRoot(t *testing.T) {
	root := t.TempDir()
	bs := Bootstrap{DataRoot: root, DataDir: filepath.Join(root, "agent-overflow"), Identity: instanceinfo.Identity{ID: instanceinfo.ID(root), IdentityVersion: instanceinfo.IdentityVersion}}
	if err := bs.ValidateFor(t.TempDir(), bs.DataDir); err == nil {
		t.Fatal("ValidateFor accepted a bootstrap from another root")
	}
}

func TestBootstrapValidateForAcceptsLegacyUnknownIdentity(t *testing.T) {
	root := t.TempDir()
	bs := Bootstrap{DataRoot: root, DataDir: filepath.Join(root, "agent-overflow")}
	if err := bs.ValidateFor(root, bs.DataDir); err != nil {
		t.Fatalf("legacy bootstrap should remain readable: %v", err)
	}
}

func TestReadInstanceFileRejectsCurrentIdentityPathMismatch(t *testing.T) {
	dataDir := t.TempDir()
	data, err := json.Marshal(Bootstrap{
		Port: 9, Token: "tok", DataRoot: t.TempDir(), DataDir: dataDir,
		Identity: instanceinfo.Identity{IdentityVersion: instanceinfo.IdentityVersion, ID: "0123abcd", BootNonce: "boot"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, instanceinfo.InstanceFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstanceFile(dataDir); err == nil {
		t.Fatal("ReadInstanceFile accepted a current identity with mismatched paths")
	}
}

func TestReadInstanceFileRefusesSymlinkedDataDirAndFile(t *testing.T) {
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, instanceinfo.InstanceFileName), []byte(`{"port":9,"token":"tok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(t.TempDir(), "data-dir-link")
	if err := os.Symlink(target, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ReadInstanceFile(linkDir); err == nil {
		t.Fatal("ReadInstanceFile followed a symlinked data directory")
	}

	dir := t.TempDir()
	fileTarget := filepath.Join(t.TempDir(), "instance.json")
	if err := os.WriteFile(fileTarget, []byte(`{"port":9,"token":"tok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fileTarget, filepath.Join(dir, instanceinfo.InstanceFileName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ReadInstanceFile(dir); err == nil {
		t.Fatal("ReadInstanceFile followed a symlinked instance file")
	}
}
