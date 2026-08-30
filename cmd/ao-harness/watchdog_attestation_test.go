package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agent-overflow/internal/harness/governor"
	"agent-overflow/internal/harness/instanceinfo"
	"agent-overflow/internal/harnessclient"
)

func writeContainmentFixture(t *testing.T, root string, evidence harnessContainmentEvidence) {
	t.Helper()
	path := filepath.Join(root, appDataDirName, "logs", "harness-containment.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func containmentFixture(t *testing.T) (target, harnessclient.Bootstrap, harnessContainmentEvidence) {
	t.Helper()
	root := t.TempDir()
	dataDir := filepath.Join(root, appDataDirName)
	launcher := instanceinfo.Identity{
		IdentityVersion:          instanceinfo.IdentityVersion,
		ID:                       instanceinfo.ID(root),
		Mode:                     instanceinfo.ModePerf,
		BootNonce:                "boot",
		Worktree:                 "/worktree",
		StartedAt:                "now",
		LauncherPid:              42,
		LauncherProcessStartTime: "1234",
		LauncherExecutablePath:   "/tmp/agent-overflow.exe",
		LauncherProfile:          string(instanceinfo.ModePerf),
		LauncherDataRoot:         root,
		LauncherWebviewProfile:   "/tmp/webview-perf",
		LauncherPIDNamespace:     "windows",
	}
	bs := harnessclient.Bootstrap{
		DataRoot: root,
		DataDir:  dataDir,
		PID:      77,
		Identity: launcher,
	}
	row := instanceinfo.Instance{Row: instanceinfo.Row{Identity: launcher, PID: bs.PID, DataRoot: root, DataDir: dataDir}}
	evidence := harnessContainmentEvidence{
		Version:            1,
		Enforcement:        "windows-job+linux-rlimit-data",
		WindowsJob:         true,
		LinuxPID:           bs.PID,
		MemoryLimitBytes:   governor.DefaultCeilingBytes,
		WatchdogIntervalMS: 100,
		Mode:               string(bs.Mode),
		DataRoot:           bs.DataRoot,
		LauncherPID:        launcher.LauncherPid,
		LauncherStartTime:  launcher.LauncherProcessStartTime,
		LauncherExecutable: launcher.LauncherExecutablePath,
		LauncherProfile:    launcher.LauncherProfile,
		LauncherWebview:    launcher.LauncherWebviewProfile,
	}
	return target{DataRoot: root, DataDir: dataDir, Row: &row}, bs, evidence
}

func TestRequireActiveHarnessBoundaryAcceptsMatchingWSLEvidence(t *testing.T) {
	target, bs, evidence := containmentFixture(t)
	writeContainmentFixture(t, target.DataRoot, evidence)
	if err := requireActiveHarnessBoundary(target, bs); err != nil {
		t.Fatal(err)
	}
}

func TestRequireActiveHarnessBoundaryRejectsStaleOrMismatchedEvidence(t *testing.T) {
	cases := []struct {
		name string
		edit func(*harnessContainmentEvidence)
	}{
		{"wrong backend pid", func(e *harnessContainmentEvidence) { e.LinuxPID++ }},
		{"wrong data root", func(e *harnessContainmentEvidence) { e.DataRoot = "/stale/root" }},
		{"wrong launcher birth", func(e *harnessContainmentEvidence) { e.LauncherStartTime = "9999" }},
		{"wrong launcher executable", func(e *harnessContainmentEvidence) { e.LauncherExecutable = "/tmp/other.exe" }},
		{"wrong memory limit", func(e *harnessContainmentEvidence) { e.MemoryLimitBytes++ }},
		{"wrong mode", func(e *harnessContainmentEvidence) { e.Mode = string(instanceinfo.ModeHarness) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, bs, evidence := containmentFixture(t)
			tc.edit(&evidence)
			writeContainmentFixture(t, target.DataRoot, evidence)
			if err := requireActiveHarnessBoundary(target, bs); err == nil {
				t.Fatal("mismatched WSL containment evidence was accepted")
			}
		})
	}
}

func TestRequireActiveHarnessBoundaryRejectsUnknownEvidenceFields(t *testing.T) {
	target, bs, evidence := containmentFixture(t)
	writeContainmentFixture(t, target.DataRoot, evidence)
	path := filepath.Join(target.DataRoot, appDataDirName, "logs", "harness-containment.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"enforcement":"windows-job+linux-rlimit-data","windowsJob":true,"linuxPid":77,"memoryLimitBytes":629145600,"watchdogIntervalMs":100,"mode":"perf","dataRoot":"`+target.DataRoot+`","launcherPid":42,"launcherStartTime":"1234","launcherExecutable":"/tmp/agent-overflow.exe","launcherProfile":"perf","launcherWebviewProfile":"/tmp/webview-perf","unexpected":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireActiveHarnessBoundary(target, bs); err == nil {
		t.Fatal("unknown containment evidence field was accepted")
	}
}
