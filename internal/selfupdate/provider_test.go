package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

const stagedArtifactName = "agent-overflow-wsl-amd64.exe"

// stageFixture writes an artifact into a fresh staging dir and returns its path,
// bytes and digest.
func stageFixture(t *testing.T) (path string, payload, digest []byte) {
	t.Helper()
	payload = bytes.Repeat([]byte("PE\x00\x00agent-overflow"), 5_000)
	path, digest = writeArtifact(t, t.TempDir(), stagedArtifactName, payload)
	return path, payload, digest
}

func TestStagedFileProviderCheck(t *testing.T) {
	path, payload, digest := stageFixture(t)
	p := NewStagedFileProvider(path, "0.0.11", digest)

	if p.Name() != ProviderName {
		t.Fatalf("Name = %q, want %q", p.Name(), ProviderName)
	}

	rel, err := p.Check(context.Background(), updater.CheckRequest{CurrentVersion: "0.0.10", Platform: "windows", Arch: "amd64"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rel == nil {
		t.Fatal("Check returned no release for a staged artifact")
	}
	if rel.Version != "0.0.11" {
		t.Fatalf("Version = %q, want 0.0.11", rel.Version)
	}
	if rel.Artifact.Filename != stagedArtifactName {
		t.Fatalf("Artifact.Filename = %q, want %q", rel.Artifact.Filename, stagedArtifactName)
	}
	if rel.Artifact.Size != int64(len(payload)) {
		t.Fatalf("Artifact.Size = %d, want %d", rel.Artifact.Size, len(payload))
	}
	if rel.Artifact.Platform != "windows" || rel.Artifact.Arch != "amd64" {
		t.Fatalf("Artifact platform/arch = %s/%s, want windows/amd64", rel.Artifact.Platform, rel.Artifact.Arch)
	}
	if rel.Verification == nil {
		t.Fatal("Verification is nil — the release would install unverified")
	}
	if rel.Verification.DigestAlgo != "sha256" {
		t.Fatalf("DigestAlgo = %q, want sha256", rel.Verification.DigestAlgo)
	}
	if !bytes.Equal(rel.Verification.Digest, digest) {
		t.Fatalf("Verification.Digest = %x, want %x", rel.Verification.Digest, digest)
	}
}

func TestStagedFileProviderCheckAllowsRollback(t *testing.T) {
	// The staged version is OLDER than the running one: an explicit user-chosen
	// rollback must still resolve, so no newer-than-current gate may creep in.
	path, _, digest := stageFixture(t)
	rel, err := NewStagedFileProvider(path, "0.0.5", digest).
		Check(context.Background(), updater.CheckRequest{CurrentVersion: "0.0.11", Platform: "windows", Arch: "amd64"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if rel == nil {
		t.Fatal("Check returned nil for an older staged version (a newer-than-current gate leaked in?)")
	}
}

func TestStagedFileProviderCheckRejectsUnusableState(t *testing.T) {
	path, _, digest := stageFixture(t)
	missing := filepath.Join(t.TempDir(), "absent.exe")
	dir := t.TempDir()

	tests := []struct {
		name string
		p    *StagedFileProvider
	}{
		{"missing file", NewStagedFileProvider(missing, "0.0.11", digest)},
		{"directory instead of a file", NewStagedFileProvider(dir, "0.0.11", digest)},
		{"empty version", NewStagedFileProvider(path, "", digest)},
		{"short digest", NewStagedFileProvider(path, "0.0.11", digest[:16])},
		{"nil digest", NewStagedFileProvider(path, "0.0.11", nil)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.p.Check(context.Background(), updater.CheckRequest{}); err == nil {
				t.Fatal("Check = nil error, want an error")
			}
		})
	}
}

func TestStagedFileProviderDownload(t *testing.T) {
	path, payload, digest := stageFixture(t)
	p := NewStagedFileProvider(path, "0.0.11", digest)

	var got bytes.Buffer
	var calls int
	var lastWritten, lastTotal int64
	err := p.Download(context.Background(), nil, &got, func(written, total int64) {
		calls++
		lastWritten, lastTotal = written, total
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("streamed %d bytes, want the %d-byte artifact verbatim", got.Len(), len(payload))
	}
	if calls == 0 {
		t.Fatal("Download never reported progress")
	}
	if lastWritten != int64(len(payload)) || lastTotal != int64(len(payload)) {
		t.Fatalf("final progress = %d/%d, want %d/%[3]d", lastWritten, lastTotal, len(payload))
	}
}

func TestStagedFileProviderDownloadMissingFile(t *testing.T) {
	p := NewStagedFileProvider(filepath.Join(t.TempDir(), "absent.exe"), "0.0.11", make([]byte, sha256.Size))
	if err := p.Download(context.Background(), nil, &bytes.Buffer{}, func(int64, int64) {}); err == nil {
		t.Fatal("Download of a missing artifact = nil, want an error")
	}
}

// noopUpdaterHost is the minimal updater.Host needed to drive a real *Updater
// with no window and no application event bus.
type noopUpdaterHost struct{}

func (noopUpdaterHost) Emit(string, ...any) bool                              { return true }
func (noopUpdaterHost) OnEvent(string, func(any)) func()                      { return func() {} }
func (noopUpdaterHost) OpenWindow(updater.WindowOptions) updater.WindowHandle { return nil }
func (noopUpdaterHost) Quit()                                                 {}

// newStagedUpdater wires a StagedFileProvider into a real updater.Updater, with
// the framework's staging kept inside the test's temp dir.
func newStagedUpdater(t *testing.T, p *StagedFileProvider) *updater.Updater {
	t.Helper()
	// The Updater stages downloads under os.MkdirTemp(""); pin TMPDIR so the
	// artifacts it writes land in (and die with) the test's temp dir.
	t.Setenv("TMPDIR", t.TempDir())
	up := updater.New(noopUpdaterHost{})
	if err := up.Init(updater.Config{
		CurrentVersion: "0.0.10",
		Providers:      []updater.Provider{p},
		Platform:       "windows",
		Arch:           "amd64",
		Window:         updater.WindowNone,
	}); err != nil {
		t.Fatalf("updater.Init: %v", err)
	}
	return up
}

func TestStagedFileProviderDrivesRealUpdater(t *testing.T) {
	// End-to-end through the stock machinery: Check → download (streaming hash)
	// → verify → stage. Proves the Release this provider builds is shaped the way
	// the framework's verifier expects.
	path, payload, digest := stageFixture(t)
	up := newStagedUpdater(t, NewStagedFileProvider(path, "0.0.11", digest))

	rel, err := up.Check(context.Background())
	if err != nil {
		t.Fatalf("Updater.Check: %v", err)
	}
	if rel == nil {
		t.Fatal("Updater.Check found no release")
	}
	if err := up.DownloadAndInstall(context.Background()); err != nil {
		t.Fatalf("DownloadAndInstall: %v", err)
	}
	if got := up.State(); got != updater.StateReady {
		t.Fatalf("state = %s, want %s", got, updater.StateReady)
	}

	staged := up.DownloadedPath()
	if filepath.Base(staged) != stagedArtifactName {
		t.Fatalf("DownloadedPath = %q, want a file named %q", staged, stagedArtifactName)
	}
	gotBytes, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read the updater's staged file: %v", err)
	}
	if !bytes.Equal(gotBytes, payload) {
		t.Fatalf("staged file differs from the source artifact (%d vs %d bytes)", len(gotBytes), len(payload))
	}
}

func TestStagedFileProviderTamperedArtifactFailsVerification(t *testing.T) {
	// The digest is the release's, but the staged bytes were changed after
	// staging: the Updater's streaming hash must reject the install.
	path, _, digest := stageFixture(t)
	up := newStagedUpdater(t, NewStagedFileProvider(path, "0.0.11", digest))

	if _, err := up.Check(context.Background()); err != nil {
		t.Fatalf("Updater.Check: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper with the staged artifact: %v", err)
	}
	if err := up.DownloadAndInstall(context.Background()); err == nil {
		t.Fatal("DownloadAndInstall of a tampered artifact = nil, want a verification error")
	}
	if got := up.State(); got != updater.StateError {
		t.Fatalf("state = %s, want %s", got, updater.StateError)
	}
	if up.DownloadedPath() != "" {
		t.Fatalf("DownloadedPath = %q, want empty after a failed verification", up.DownloadedPath())
	}
}
