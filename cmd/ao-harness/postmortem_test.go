package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestPostmortemRequiresAnAbsoluteCanonicalRoot(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{filepath.Base(root), root + string(filepath.Separator)} {
		if _, err := validatePostmortemRoot(name); err == nil {
			t.Fatalf("validatePostmortemRoot(%q) accepted a non-canonical root", name)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := validatePostmortemRoot(link); err == nil {
		t.Fatal("validatePostmortemRoot accepted a symlink root")
	}
}

func TestPostmortemIsReadOnlyAndDoesNotAttach(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "logs", "backend-stderr.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("backend started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotTree(root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := inspectPostmortem(root, postmortemOptions{
		Root: root, MaxFiles: 100, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "clean" || report.Files != 1 {
		t.Fatalf("report = %+v, want one clean evidence file", report)
	}
	after, err := snapshotTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("postmortem changed its root")
	}
}

func TestPostmortemAllowsTheOwnedBinarySymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "agent-overflow")
	if err := os.WriteFile(target, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, appDataDirName, "harness-instance.json")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	markerData, err := json.Marshal(map[string]string{"executablePath": target})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, markerData, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, appDataDirName, postmortemCLIBinDirName, postmortemCLICommandName)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	logPath := filepath.Join(root, "logs", "backend-stderr.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("backend started\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := inspectPostmortem(root, postmortemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "clean" {
		t.Fatalf("report status = %q, findings=%+v", report.Status, report.Findings)
	}
}

func TestPostmortemRejectsAnUnsafeOwnedBinarySymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "directory")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, appDataDirName, postmortemCLIBinDirName, postmortemCLICommandName)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	report, err := inspectPostmortem(root, postmortemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "findings" {
		t.Fatalf("report status = %q, want findings", report.Status)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Code == "symlink" && finding.Path == link {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want unsafe binary symlink", report.Findings)
	}
}

func TestPostmortemRejectsAnArbitrarySymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "evidence")
	if err := os.WriteFile(target, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "logs", "copied.log")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	report, err := inspectPostmortem(root, postmortemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range report.Findings {
		if finding.Code == "symlink" && finding.Path == link {
			return
		}
	}
	t.Fatalf("findings = %+v, want arbitrary symlink finding", report.Findings)
}

func TestPostmortemRejectsLiveOwnershipMarker(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "harness-instance.json")
	data, err := json.Marshal(map[string]int{"pid": os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, data, 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, registryDir: filepath.Join(t.TempDir(), "registry")}
	if err := refusePostmortemOwner(e, root); err == nil {
		t.Fatal("postmortem accepted a root owned by this live process")
	}
}

func TestPostmortemRequestedUIRequiresSnapshots(t *testing.T) {
	root := t.TempDir()
	report, err := inspectPostmortem(root, postmortemOptions{
		Root: root, UI: true, UIDiff: true, MaxFiles: 100, MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "findings" || len(report.Findings) < 2 {
		t.Fatalf("report = %+v, want missing UI and no-evidence findings", report)
	}
}

func TestPostmortemRejectsIncompleteBundle(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bundles", "run-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"event":"x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := inspectPostmortem(root, postmortemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "findings" {
		t.Fatalf("report status = %q, want findings", report.Status)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Code == "incomplete_bundle" {
			found = true
		}
	}
	if !found {
		t.Fatalf("findings = %+v, want incomplete bundle", report.Findings)
	}
}

func TestPostmortemReportsLogLineCountsAndRunArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "run-manifest.json"), []byte(`{"state":"failed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "compare-supervisor.json"), []byte(`{"status":"failed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "run-backend.stderr.log")
	if err := os.WriteFile(logPath, []byte("started\nfinished\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := inspectPostmortem(root, postmortemOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "clean" {
		t.Fatalf("report status = %q, findings=%+v", report.Status, report.Findings)
	}
	for _, artifact := range report.Artifacts {
		if filepath.Base(artifact.Path) == filepath.Base(logPath) && artifact.Lines != 2 {
			t.Fatalf("log artifact lines = %d, want 2", artifact.Lines)
		}
	}
}

func TestPostmortemDatabaseReadDoesNotTouchWALSource(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "agent-overflow.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE evidence (value TEXT); INSERT INTO evidence VALUES ('wal-only')`); err != nil {
		t.Fatal(err)
	}
	walPath := dbPath + "-wal"
	if _, err := os.Stat(walPath); err != nil {
		t.Fatal("expected uncheckpointed WAL", err)
	}
	before, err := snapshotTree(root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := inspectPostmortem(root, postmortemOptions{Root: root})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	after, err := snapshotTree(root)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		db.Close()
		t.Fatal("postmortem changed database/WAL evidence")
	}
	if len(report.Databases) != 1 || !report.Databases[0].ReadOnly || report.Databases[0].Integrity != "ok" {
		db.Close()
		t.Fatalf("database report = %+v", report.Databases)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func snapshotTree(root string) ([]byte, error) {
	var out []byte
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf((*byteBuffer)(&out), "%s|%o|%d|%d\x00", path, info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out = append(out, []byte(path)...)
			out = append(out, 0)
			out = append(out, data...)
			out = append(out, 0)
		}
		return nil
	})
	return out, err
}

// byteBuffer adapts the existing byte-slice tree snapshot to fmt.Fprintf
// without allocating a bytes.Buffer for each test call.
type byteBuffer []byte

func (b *byteBuffer) Write(p []byte) (int, error) {
	*b = append(*b, p...)
	return len(p), nil
}
